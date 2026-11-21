package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"taskctl/internal/store"
)

func (a *app) watchContext() (context.Context, context.CancelFunc, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	if a.o.has("timeout") {
		d, err := a.o.duration("timeout", 0)
		if err != nil {
			stop()
			return nil, nil, err
		}
		child, cancel := context.WithTimeout(ctx, d)
		return child, func() { cancel(); stop() }, nil
	}
	return ctx, stop, nil
}
func contextError(ctx context.Context) error {
	if ctx.Err() == context.DeadlineExceeded {
		return &exitError{5, "watch timed out"}
	}
	return &exitError{130, "watch interrupted"}
}
func callback(ctx context.Context, path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, path)
	cmd.Stdin = bytes.NewReader(append(b, '\n'))
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("callback failed; event was not acknowledged: %w", err)
	}
	return nil
}
func (a *app) emitWatch(ctx context.Context, v any, human string) error {
	if program := a.o.get("exec"); program != "" {
		if err := callback(ctx, program, v); err != nil {
			return err
		}
	}
	return a.ok(v, human)
}
func terminal(outcome string) error {
	if outcome == "cancelled" {
		return &exitError{4, "watched work includes cancelled tasks"}
	}
	return nil
}
func isTerminal(outcome string) bool { return outcome == "completed" || outcome == "cancelled" }
func (a *app) watch(waitOnly bool) error {
	if err := a.o.allow("after", "timeout", "interval", "exec", "once", "inactivity"); err != nil {
		return err
	}
	id, err := a.o.position(2)
	if err != nil {
		return err
	}
	if waitOnly && (a.o.has("after") || a.o.has("exec") || a.o.has("once")) {
		return usage("wait does not accept --after, --exec or --once")
	}
	after, err := a.o.number("after", 0)
	if err != nil {
		return err
	}
	interval, err := a.o.duration("interval", 250*time.Millisecond)
	if err != nil {
		return err
	}
	if interval < 10*time.Millisecond {
		return usage("interval must be at least 10ms")
	}
	def, _ := time.ParseDuration(a.cfg.Inactivity)
	age, err := a.o.duration("inactivity", def)
	if err != nil {
		return err
	}
	ctx, cancel, err := a.watchContext()
	if err != nil {
		return err
	}
	defer cancel()
	if !a.o.has("after") {
		snap, cursor, err := a.s.Current(id)
		if err != nil {
			return err
		}
		after = cursor
		if !waitOnly {
			if err = a.emitWatch(ctx, map[string]any{"type": "snapshot", "cursor": cursor, "snapshot": snap}, id+" "+snap.Outcome); err != nil {
				return err
			}
		}
		if isTerminal(snap.Outcome) {
			if waitOnly {
				if err = a.ok(map[string]any{"task_id": id, "outcome": snap.Outcome}, id+" "+snap.Outcome); err != nil {
					return err
				}
			}
			return terminal(snap.Outcome)
		}
		if a.o.yes("once") {
			return nil
		}
	}
	lastScan := time.Time{}
	for {
		if ctx.Err() != nil {
			return contextError(ctx)
		}
		if time.Since(lastScan) >= time.Second {
			if _, err = a.s.DetectInactivity(age); err != nil {
				return err
			}
			lastScan = time.Now()
		}
		snap, boundary, err := a.s.Current(id)
		if err != nil {
			return err
		}
		page, err := a.s.Events(id, after, 100)
		if err != nil {
			return err
		}
		for _, event := range page.Events {
			if !waitOnly {
				if err = a.emitWatch(ctx, map[string]any{"type": "event", "cursor": event.ID, "event": event}, fmt.Sprintf("E%d %s %s %s", event.ID, event.TaskID, event.Kind, clean(event.Message))); err != nil {
					return err
				}
			}
			after = event.ID
		}
		if page.Cursor > after {
			after = page.Cursor
		}
		if !page.More && isTerminal(snap.Outcome) {
			if boundary > after {
				after = boundary
			}
			if err = a.ok(map[string]any{"type": "outcome", "task_id": id, "outcome": snap.Outcome, "cursor": after}, id+" "+snap.Outcome); err != nil {
				return err
			}
			return terminal(snap.Outcome)
		}
		if a.o.yes("once") {
			return nil
		}
		if page.More {
			continue
		}
		select {
		case <-ctx.Done():
			return contextError(ctx)
		case <-time.After(interval):
		}
	}
}
func (a *app) subscriptions() error {
	if len(a.o.pos) < 2 {
		return usage("subscriptions requires pending, acknowledge or watch")
	}
	id, err := a.o.position(2)
	if err != nil {
		return err
	}
	session, err := a.o.require("session")
	if err != nil {
		return err
	}
	switch a.o.pos[1] {
	case "pending":
		if err = a.o.allow("session", "limit", "lease"); err != nil {
			return err
		}
		limit, err := a.o.number("limit", 20)
		if err != nil {
			return err
		}
		lease, err := a.o.duration("lease", 5*time.Minute)
		if err != nil {
			return err
		}
		d, err := a.s.Offer(id, session, int(limit), lease)
		if err != nil {
			return err
		}
		if d.Token == "" {
			return nil
		}
		if a.o.yes("json") {
			return a.json(d)
		}
		return a.printDelivery(d)
	case "acknowledge":
		if err = a.o.allow("session", "token", "through"); err != nil {
			return err
		}
		token, err := a.o.require("token")
		if err != nil {
			return err
		}
		if !a.o.has("through") {
			return usage("--through is required")
		}
		through, err := a.o.number("through", 0)
		if err != nil {
			return err
		}
		if err = a.s.Acknowledge(id, session, token, through); err != nil {
			return err
		}
		return a.ok(map[string]any{"subscription_id": id, "acknowledged_through": through}, id+" acknowledged through E"+strconv.FormatInt(through, 10))
	case "watch":
		return a.watchSubscription(id, session)
	}
	return usage("unknown subscriptions command " + a.o.pos[1])
}
func (a *app) printDelivery(d store.Delivery) error {
	if d.Snapshot != nil {
		fmt.Fprintln(a.out, d.Subscription.TaskID, d.Snapshot.Outcome)
		for _, t := range d.Snapshot.Tasks {
			fmt.Fprintf(a.out, "%s %s %s\n", t.ID, t.Status, clean(t.Title))
		}
		if d.Snapshot.Omitted > 0 {
			fmt.Fprintf(a.out, "%d more tasks; use tasks show/history\n", d.Snapshot.Omitted)
		}
	}
	for _, e := range d.Events {
		fmt.Fprintf(a.out, "E%d %s %s %s\n", e.ID, e.TaskID, e.Kind, clean(e.Message))
	}
	fmt.Fprintf(a.out, "acknowledge: taskctl --project %s subscriptions acknowledge %s --session %s --token %s --through %d\n", shellLiteral(a.p.ID), shellLiteral(d.Subscription.ID), shellLiteral(d.Subscription.Session), shellLiteral(d.Token), d.Through)
	if d.More {
		fmt.Fprintln(a.out, "more updates remain after this batch")
	}
	return nil
}
func actionable(d store.Delivery) bool {
	if d.Snapshot != nil {
		if isTerminal(d.Snapshot.Outcome) || d.Snapshot.Available {
			return true
		}
		for _, t := range d.Snapshot.Tasks {
			if t.Available {
				return true
			}
		}
	}
	for _, e := range d.Events {
		switch e.Kind {
		case "completed", "cancelled", "available", "inactive":
			return true
		}
	}
	return false
}
func (a *app) watchSubscription(id, session string) error {
	if err := a.o.allow("session", "exec", "codex", "codex-executable", "once", "timeout", "interval", "limit", "lease", "inactivity"); err != nil {
		return err
	}
	if a.o.has("exec") && a.o.yes("codex") {
		return usage("use either --exec or --codex")
	}
	limit, err := a.o.number("limit", 20)
	if err != nil {
		return err
	}
	lease, err := a.o.duration("lease", 5*time.Minute)
	if err != nil {
		return err
	}
	interval, err := a.o.duration("interval", time.Second)
	if err != nil {
		return err
	}
	if interval < 10*time.Millisecond {
		return usage("interval must be at least 10ms")
	}
	def, _ := time.ParseDuration(a.cfg.Inactivity)
	age, err := a.o.duration("inactivity", def)
	if err != nil {
		return err
	}
	ctx, cancel, err := a.watchContext()
	if err != nil {
		return err
	}
	defer cancel()
	lastScan := time.Time{}
	for {
		if ctx.Err() != nil {
			return contextError(ctx)
		}
		if time.Since(lastScan) >= time.Second {
			if _, err = a.s.DetectInactivity(age); err != nil {
				return err
			}
			lastScan = time.Now()
		}
		var d store.Delivery
		if a.o.yes("codex") {
			d, err = a.s.OfferQueueSummary(id, session, int(limit), lease)
		} else {
			d, err = a.s.Offer(id, session, int(limit), lease)
		}
		if err != nil && store.ErrorCode(err) != "delivery_busy" {
			return err
		}
		if err == nil && d.Token != "" {
			if a.o.yes("codex") {
				if !d.Subscription.ResumeIdle {
					_ = a.s.ReleaseDelivery(id, d.Token)
					return usage("subscription does not permit idle resumption; create it with --resume")
				}
				if actionable(d) && d.Through > d.Subscription.QueuedThrough {
					exe := a.o.get("codex-executable")
					if exe == "" {
						exe = "codex"
					}
					text := fmt.Sprintf("Task updates for subscription %s through E%d. Read them with taskctl --project %s subscriptions pending %s --session %s --json. Acknowledge the returned batch after reading it.", id, d.Through, shellLiteral(a.p.ID), shellLiteral(id), shellLiteral(session))
					cctx, stop := context.WithTimeout(ctx, 30*time.Second)
					cmd := exec.CommandContext(cctx, exe, "queue", "--thread", session, "--message", text, "--resume")
					cmd.Stdout = os.Stderr
					cmd.Stderr = os.Stderr
					err = cmd.Run()
					stop()
					if err != nil {
						_ = a.s.ReleaseDelivery(id, d.Token)
						return fmt.Errorf("Codex queue attempt failed; updates remain pending: %w", err)
					}
					if err = a.s.MarkQueued(id, session, d.Token, d.Through); err != nil {
						return err
					}
					if err = a.ok(map[string]any{"subscription_id": id, "queued_through": d.Through, "acknowledged": false}, id+" queued through E"+strconv.FormatInt(d.Through, 10)+"; acknowledgement pending"); err != nil {
						return err
					}
				} else {
					if err = a.s.AdvanceQueueScan(id, session, d.Token, d.Through); err != nil {
						return err
					}
				}
				if err = a.s.ReleaseDelivery(id, d.Token); err != nil {
					return err
				}
			} else if a.o.has("exec") {
				if err = callback(ctx, a.o.get("exec"), d); err != nil {
					_ = a.s.ReleaseDelivery(id, d.Token)
					return err
				}
				if err = a.s.Acknowledge(id, session, d.Token, d.Through); err != nil {
					return err
				}
				if err = a.ok(map[string]any{"subscription_id": id, "acknowledged_through": d.Through}, id+" callback acknowledged through E"+strconv.FormatInt(d.Through, 10)); err != nil {
					return err
				}
			} else {
				if a.o.yes("json") {
					err = a.json(d)
				} else {
					err = a.printDelivery(d)
				}
				if err != nil {
					return err
				}
			}
		}
		if a.o.yes("once") {
			return nil
		}
		select {
		case <-ctx.Done():
			return contextError(ctx)
		case <-time.After(interval):
		}
	}
}

func shellLiteral(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
