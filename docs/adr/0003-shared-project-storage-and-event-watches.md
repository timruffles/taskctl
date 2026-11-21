# 0003: Share a local project store and watch persisted events

Status: Accepted, 2026-11-21

Each project has one SQLite database outside its repositories, shared by its associated worktrees. A stable project identity, local configuration and an explicit project override select the store. Moving a checkout does not create a new backlog. Locations follow XDG conventions. Task briefs contain Markdown. Backups are consistent SQLite snapshots that can be stored in Git or another backup system, without a runtime Git dependency. As amended for 0.2.0, JSON archives are removed; ADR 0006 records the format and memory decisions.

Normal commands operate independently of a notification process. A foreground watch command observes persisted events, follows replacement tasks, provides progress and lifecycle updates, and can invoke a callback. A cursor supports resumption. State changes and their events are committed together. Immediate external notification requires a running observer.

This keeps a small local deployment independent of a task-server lifecycle while preserving events across watcher restarts. It also keeps worktree-local state from creating conflicting task ownership. Codex integration builds on this event stream. ADR 0004 specifies durable delivery claims, acknowledgements and opt-in idle resumption.
