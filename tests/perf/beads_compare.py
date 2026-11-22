#!/usr/bin/env python3
"""Same-host CLI comparison; setup is excluded, defaults and raw samples retained."""

import argparse
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import platform
import random
import signal
import socket
import sqlite3
import statistics
import subprocess
import threading
import time


def run(argv, directory, env, timeout=120):
    p = subprocess.run(list(map(str, argv)), cwd=directory, env=env,
                       capture_output=True, text=True, timeout=timeout)
    if p.returncode:
        raise RuntimeError(f"{argv}: exit {p.returncode}: {p.stderr[-4000:]}")
    return p.stdout


def rss(pid):
    values = {}
    try:
        for line in Path(f"/proc/{pid}/status").read_text().splitlines():
            if line.startswith(("VmRSS:", "VmHWM:")):
                key, value, _ = line.split()
                values[key.rstrip(":")] = int(value) * 1024
    except FileNotFoundError:
        pass
    return values


def binary_info(path, version_args):
    with path.open("rb") as f:
        digest = hashlib.file_digest(f, "sha256").hexdigest()
    return {"path": str(path), "bytes": path.stat().st_size, "sha256": digest,
            "version": subprocess.check_output([str(path), *version_args], text=True).strip()}


def validate_concurrency(result):
    samples = result["samples"]
    assert len(samples) == 8 and not any(s["timed_out"] for s in samples), result
    successes = [s for s in samples if s["exit"] == 0]
    if result["operation"] == "eight-writes":
        assert len(successes) == result["verified_writes"] == 8, result
        return
    assert len(successes) == 1, result
    assert result["final_owner"] == f"racer-{successes[0]['index'] % 8}", result
    assert result["final_status"] == "in_progress", result
    for sample in samples:
        if sample["exit"] == 0:
            continue
        if result["kind"] == "taskctl":
            error = json.loads(sample["stderr"])["error"]
            assert sample["exit"] == 3 and error["code"] == "conflict", sample
        else:
            error = json.loads(sample["stderr"].splitlines()[-1])
            assert sample["exit"] == 1 and len(error["failed"]) == 1, sample
            assert error["failed"][0]["error"] == f"updating issue: issue already claimed by {result['final_owner']}", sample


class Fixture:
    def __init__(self, bench, kind, n):
        self.bench, self.kind, self.n = bench, kind, n
        self.directory = bench.root / f"{n}-{kind}"
        self.directory.mkdir()
        self.env = bench.env.copy()
        for key, part in (("XDG_DATA_HOME", "data"), ("XDG_STATE_HOME", "state"),
                          ("XDG_CONFIG_HOME", "config")):
            self.env[key] = str(self.directory / part)
        self.env["DOLT_ROOT_PATH"] = str(self.directory / "dolt-root")
        Path(self.env["DOLT_ROOT_PATH"]).mkdir()
        self.server = None
        self.claims = {}
        self.binary = bench.taskctl if kind == "taskctl" else bench.beads
        self.info = {"kind": kind, "tasks": n, "pending": 40, "in_progress": 8,
                     "completed": n - 48, "brief_bytes": 1024, "tags_per_task": 1}
        started = time.monotonic()
        if kind == "taskctl":
            project = json.loads(self.command("projects", "create", "--name", f"bench-{n}", "--json"))
            self.env["TASKCTL_PROJECT"] = project["id"]
            self.database = Path(self.env["XDG_DATA_HOME"]) / "taskctl/projects" / project["id"] / "tasks.sqlite"
            with sqlite3.connect(self.database) as db:
                db.execute("PRAGMA foreign_keys=ON")
                for i in range(1, n + 1):
                    status = "completed" if i <= n - 48 else "pending" if i <= n - 8 else "in_progress"
                    owner = f"worker-{i - (n - 7)}" if status == "in_progress" else ""
                    claim = f"Cfixture{i}" if owner else ""
                    db.execute("""INSERT INTO tasks(id,title,brief,status,origin_id,owner,claim_id,
                                  created_at,updated_at,last_activity_at) VALUES(?,?,?,?,?,?,?,?,?,?)""",
                               (i, f"Benchmark task {i:06}", "b" * 1024, status, i, owner, claim,
                                bench.stamp, bench.stamp, bench.stamp if owner else ""))
                    db.execute("INSERT INTO tags(task_id,tag) VALUES(?,'fixture')", (i,))
                    if claim:
                        self.claims[i] = claim
                db.commit()
                db.execute("PRAGMA wal_checkpoint(TRUNCATE)")
                assert db.execute("PRAGMA quick_check").fetchone()[0] == "ok"
        else:
            run(["git", "init", "-q"], self.directory, self.env)
            run(["git", "config", "user.name", "Benchmark"], self.directory, self.env)
            run(["git", "config", "user.email", "benchmark@example.invalid"], self.directory, self.env)
            init = ["init", "--prefix", "bench", "--skip-hooks", "--skip-agents", "--non-interactive"]
            if kind == "beads-server":
                run([bench.dolt, "config", "--global", "--add", "versioncheck.disabled", "true"],
                    self.directory, self.env)
                with socket.socket() as s:
                    s.bind(("127.0.0.1", 0))
                    self.port = s.getsockname()[1]
                self.start_server()
                init += ["--server", "--external", "--server-host", "127.0.0.1", "--server-port", str(self.port)]
            self.command(*init)
            source = self.directory / "fixture.jsonl"
            with source.open("w") as out:
                for i in range(1, n + 1):
                    status = "closed" if i <= n - 48 else "open" if i <= n - 8 else "in_progress"
                    issue = dict(id=f"bench-{i:06}", title=f"Benchmark task {i:06}", description="b" * 1024,
                                 status=status, priority=2, issue_type="task", labels=["fixture"],
                                 created_at=bench.stamp, updated_at=bench.stamp)
                    if status == "closed":
                        issue["closed_at"] = bench.stamp
                    elif status == "in_progress":
                        issue["assignee"] = f"worker-{i - (n - 7)}"
                    out.write(json.dumps(issue) + "\n")
            imported = json.loads(self.command("import", str(source), "--json", timeout=600))
            assert imported["created"] == n and imported["skipped"] == 0, imported
            self.info["auto_commit"] = self.command("config", "get", "dolt.auto-commit").strip()
            if self.server:
                self.stop_server()
                self.start_server()
                self.info["server_restart"] = dict(self.server_start)
        self.info["setup_seconds"] = time.monotonic() - started
        self.info["initial_counts"] = self.counts()
        assert self.info["initial_counts"] == {"completed": n - 48, "pending": 40, "in_progress": 8}, self.info
        self.info["storage_bytes_before"] = self.storage_bytes()
        bench.results["fixtures"].append(self.info)
        bench.save()
        print(json.dumps({"setup": self.info}), flush=True)

    def start_server(self):
        data = self.directory / "server-data"
        data.mkdir(exist_ok=True)
        self.server_log = (self.directory / "server.log").open("a")
        started = time.monotonic()
        self.server = subprocess.Popen([str(self.bench.dolt), "sql-server", "--host", "127.0.0.1",
                                        "--port", str(self.port), "--data-dir", str(data)],
                                       cwd=self.directory, env=self.env, stdout=self.server_log,
                                       stderr=subprocess.STDOUT, start_new_session=True)
        self.bench.server_fixtures.append(self)
        for _ in range(500):
            if self.server.poll() is not None:
                raise RuntimeError((self.directory / "server.log").read_text()[-4000:])
            try:
                with socket.create_connection(("127.0.0.1", self.port), timeout=0.05):
                    self.server_start = {"listen_ready_ms": (time.monotonic() - started) * 1000,
                                         **rss(self.server.pid)}
                    return
            except OSError:
                time.sleep(0.02)
        raise RuntimeError("Dolt did not listen within 10 seconds")

    def stop_server(self):
        if not self.server:
            return
        self.info["server_final_memory"] = rss(self.server.pid)
        if self.server.poll() is None:
            self.server.send_signal(signal.SIGTERM)
        try:
            self.server.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self.server.kill()
            self.server.wait()
        self.server_log.close()
        self.server = None

    def command(self, *args, timeout=120):
        return run([self.binary, *args], self.directory, self.env, timeout)

    def counts(self):
        if self.kind == "taskctl":
            with sqlite3.connect(self.database) as db:
                return dict(db.execute("SELECT status,count(*) FROM tasks GROUP BY status"))
        value = json.loads(self.command("count", "--by-status", "--json"))
        if isinstance(value, dict) and "groups" in value:
            value = value["groups"]
        if isinstance(value, dict) and "counts" in value:
            value = value["counts"]
        if isinstance(value, list):
            value = {row.get("status", row.get("key", row.get("group"))): row["count"] for row in value}
        return {dict(open="pending", closed="completed").get(k, k): v for k, v in value.items()}

    def storage_bytes(self):
        if self.kind == "taskctl":
            directories = [self.database.parent]
        elif self.kind == "beads-server":
            directories = [self.directory / "server-data", self.directory / ".beads"]
        else:
            directories = [self.directory / ".beads"]
        return sum(p.stat().st_size for d in directories for p in d.rglob("*") if p.is_file())

    def args(self, operation, index):
        n = self.n
        current = n
        claimant = n - 47 + index
        title = f"Verified checkpoint {index}"
        if self.kind == "taskctl":
            common = ["tasks"]
            table = {
                "show": ["show", f"T{current}"],
                "available": ["available", "--limit", "20"],
                "in-progress": ["in-progress", "--limit", "20"],
                "claim": ["claim", f"T{claimant}", "--session", "claimer"],
                "progress": ["progress", f"T{current}", "--claim", self.claims[current], "--title", title],
                "complete": ["complete", f"T{claimant}", "--claim", self.claims.get(claimant, ""),
                             "--summary", "Implemented and verified"],
            }
            return common + table[operation] + ["--json"]
        table = {
            "show": ["show", f"bench-{current:06}"],
            "available": ["ready", "--limit", "20", "--brief"],
            "in-progress": ["list", "--status", "in_progress", "--limit", "20", "--brief"],
            "claim": ["update", f"bench-{claimant:06}", "--claim", "--actor", "claimer"],
            "progress": ["update", f"bench-{current:06}", "--notes", title, "--actor", "worker-7",
                         "--if-assignee", "worker-7", "--if-status", "in_progress"],
            "complete": ["close", f"bench-{claimant:06}", "--reason", "Implemented and verified", "--actor", "claimer"],
        }
        return table[operation] + ["--json"]

    def measured(self, operation, index, args=None, tolerate_error=False):
        argv = self.args(operation, index) if args is None else args
        output = self.directory / f"{operation}-{index}.json"
        measured = run([self.bench.measure, "-timeout", "30s", "-output", output, "--", self.binary, *argv],
                       self.directory, self.env, timeout=40)
        sample = json.loads(measured)
        sample.update(kind=self.kind, tasks=self.n, operation=operation, index=index, argv=argv)
        if self.server:
            sample["server_memory"] = rss(self.server.pid)
        if sample["exit"] != 0 or sample["timed_out"]:
            sample["stdout_excerpt"] = output.read_text()[:1000]
            if not tolerate_error:
                raise RuntimeError(sample)
        else:
            value = json.loads(output.read_text())
            if operation == "claim" and self.kind == "taskctl":
                self.claims[self.n - 47 + index] = value["claim_id"]
            if operation in ("available", "in-progress"):
                rows = value["tasks"] if isinstance(value, dict) and "tasks" in value else value
                if isinstance(rows, dict):
                    rows = rows["items"]
                sample["returned_rows"] = len(rows)
                assert len(rows) == (20 if operation == "available" else 8)
            if operation == "show":
                task = value[0] if isinstance(value, list) else value
                assert len(task["brief"] if self.kind == "taskctl" else task["description"]) == 1024
                assert task["status"] == "in_progress"
        return sample

    def concurrent(self, operation, round_index):
        barrier = threading.Barrier(8)
        def one(worker):
            task = self.n - 7 + worker
            if operation == "claim-race":
                task = self.n - 38
                if self.kind == "taskctl":
                    args = ["tasks", "claim", f"T{task}", "--session", f"racer-{worker}", "--json"]
                else:
                    args = ["update", f"bench-{task:06}", "--claim", "--actor", f"racer-{worker}", "--json"]
            else:
                title = f"Concurrent checkpoint round {round_index} worker {worker}"
                if self.kind == "taskctl":
                    args = ["tasks", "progress", f"T{task}", "--claim", self.claims[task], "--title", title, "--json"]
                else:
                    args = ["update", f"bench-{task:06}", "--notes", title, "--actor", f"worker-{worker}",
                            "--if-assignee", f"worker-{worker}", "--if-status", "in_progress", "--json"]
            barrier.wait()
            return self.measured(operation, round_index * 8 + worker, args, tolerate_error=True)
        started = time.monotonic()
        with ThreadPoolExecutor(max_workers=8) as pool:
            samples = list(pool.map(one, range(8)))
        result = {"kind": self.kind, "tasks": self.n, "operation": operation, "round": round_index,
                  "wall_ms": (time.monotonic() - started) * 1000,
                  "successes": sum(s["exit"] == 0 for s in samples), "samples": samples}
        if operation == "claim-race":
            task = self.n - 38
            args = ["tasks", "show", f"T{task}", "--json"] if self.kind == "taskctl" else ["show", f"bench-{task:06}", "--json"]
            value = json.loads(self.command(*args))
            value = value[0] if isinstance(value, list) else value
            result["final_owner"] = value["owner"] if self.kind == "taskctl" else value["assignee"]
            result["final_status"] = value["status"]
            assert result["successes"] == 1 and result["final_status"] == "in_progress", result
        else:
            verified = []
            for sample in samples:
                if sample["exit"]:
                    continue
                worker = sample["index"] % 8
                task = self.n - 7 + worker
                args = ["tasks", "show", f"T{task}", "--json"] if self.kind == "taskctl" else ["show", f"bench-{task:06}", "--json"]
                value = json.loads(self.command(*args))
                value = value[0] if isinstance(value, list) else value
                actual = value["progress"] if self.kind == "taskctl" else value["notes"]
                assert actual == f"Concurrent checkpoint round {round_index} worker {worker}", value
                verified.append(task)
            result["verified_writes"] = len(verified)
        validate_concurrency(result)
        return result


class Benchmark:
    def __init__(self, args):
        self.root = args.root.resolve()
        self.root.mkdir(parents=True, exist_ok=False)
        self.taskctl, self.beads, self.dolt, self.measure = map(Path.resolve, (args.taskctl, args.beads, args.dolt, args.measure))
        self.env = {k: v for k, v in os.environ.items() if not k.startswith(("TASKCTL_", "BEADS_", "BD_", "DOLT_", "OTEL_"))}
        for key in ("GOMEMLIMIT", "GODEBUG", "GOMAXPROCS", "DOLT_ROOT_PATH"):
            self.env.pop(key, None)
        self.env.update(GOGC="100", BD_DISABLE_METRICS="1", NO_COLOR="1")
        self.stamp = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        self.server_fixtures = []
        self.results = {"date": self.stamp, "method": "Fresh CLI processes; 2 warmups excluded; paired randomised order; warm OS cache; wait4 peak RSS; default commit policy; no race instrumentation",
                        "environment": {"platform": platform.platform(), "cpu_count": os.cpu_count(),
                                        "cpu_affinity": sorted(os.sched_getaffinity(0)), "load_start": os.getloadavg()},
                        "binaries": {"taskctl": binary_info(self.taskctl, ["version"]),
                                     "beads": binary_info(self.beads, ["version"]),
                                     "dolt": binary_info(self.dolt, ["version"])},
                        "repetitions": args.repetitions, "kinds": args.kinds.split(","),
                        "fixtures": [], "samples": [], "concurrency": []}
        self.random = random.Random(1729)
        self.save()

    def save(self):
        target = self.root / "results.json"
        temporary = self.root / "results.tmp"
        temporary.write_text(json.dumps(self.results, indent=2) + "\n")
        temporary.replace(target)

    def execute(self, args):
        for n in map(int, args.sizes.split(",")):
            fixtures = []
            try:
                for kind in args.kinds.split(","):
                    fixtures.append(Fixture(self, kind, n))
                for operation in ("show", "available", "in-progress", "claim", "progress", "complete"):
                    for index in range(args.repetitions + 2):
                        order = fixtures.copy()
                        self.random.shuffle(order)
                        for fixture in order:
                            sample = fixture.measured(operation, index)
                            sample["warmup"] = index < 2
                            self.results["samples"].append(sample)
                            self.save()
                    for fixture in fixtures:
                        selected = [s for s in self.results["samples"] if s["tasks"] == n and s["kind"] == fixture.kind
                                    and s["operation"] == operation and not s["warmup"]]
                        print(json.dumps({"tasks": n, "kind": fixture.kind, "operation": operation,
                                          "median_ms": statistics.median(s["elapsed_ms"] for s in selected),
                                          "max_rss_mib": max(s["peak_rss_bytes"] for s in selected) / 1048576}), flush=True)
                for operation in ("eight-writes", "claim-race"):
                    for round_index in range(3 if operation == "eight-writes" else 1):
                        order = fixtures.copy()
                        self.random.shuffle(order)
                        for fixture in order:
                            result = fixture.concurrent(operation, round_index)
                            self.results["concurrency"].append(result)
                            self.save()
                            print(json.dumps({k: v for k, v in result.items() if k != "samples"}), flush=True)
                for fixture in fixtures:
                    fixture.info["final_counts"] = fixture.counts()
                    assert fixture.info["final_counts"] == {"completed": n - 48 + args.repetitions + 2,
                                                             "pending": 40 - args.repetitions - 3, "in_progress": 9}
                    fixture.info["storage_bytes_after"] = fixture.storage_bytes()
                    if fixture.kind == "taskctl":
                        with sqlite3.connect(fixture.database) as db:
                            fixture.info["integrity"] = db.execute("PRAGMA quick_check").fetchone()[0]
                            assert fixture.info["integrity"] == "ok"
                    self.save()
            finally:
                for fixture in fixtures:
                    fixture.stop_server()
                self.save()

    def history_control(self, n, repetitions):
        fixture = Fixture(self, "taskctl", n)
        for history_events in (0, n):
            if history_events:
                with sqlite3.connect(fixture.database) as db:
                    db.executemany("INSERT INTO events(task_id,kind,message,data,created_at) VALUES(?,'created',?,'{}',?)",
                                   ((i, f"Benchmark task {i:06}", self.stamp) for i in range(1, n + 1)))
                    db.commit()
                    db.execute("PRAGMA wal_checkpoint(TRUNCATE)")
            for operation in ("show", "available", "in-progress", "progress"):
                for index in range(repetitions + 2):
                    sample = fixture.measured(operation, index)
                    sample.update(warmup=index < 2, creation_events=history_events)
                    self.results["samples"].append(sample)
                    self.save()
                selected = [s for s in self.results["samples"] if s["operation"] == operation
                            and s["creation_events"] == history_events and not s["warmup"]]
                print(json.dumps({"control": "creation_events", "events": history_events, "operation": operation,
                                  "median_ms": statistics.median(s["elapsed_ms"] for s in selected),
                                  "max_rss_mib": max(s["peak_rss_bytes"] for s in selected) / 1048576}), flush=True)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--root", type=Path, required=True, help="new directory for isolated disposable fixtures/results")
    for name in ("taskctl", "beads", "dolt", "measure"):
        p.add_argument(f"--{name}", type=Path, required=True)
    p.add_argument("--sizes", default="100,1000,10000")
    p.add_argument("--repetitions", type=int, default=7)
    p.add_argument("--kinds", default="taskctl,beads-embedded,beads-server")
    p.add_argument("--history-control", action="store_true", help="taskctl creation-event sensitivity check at the largest --sizes value")
    args = p.parse_args()
    if not 1 <= args.repetitions <= 7 or min(map(int, args.sizes.split(","))) < 100:
        p.error("fixture reserves support at most 7 measured repetitions and at least 100 tasks")
    if not set(args.kinds.split(",")).issubset({"taskctl", "beads-embedded", "beads-server"}):
        p.error("--kinds must contain taskctl, beads-embedded or beads-server")
    bench = Benchmark(args)
    try:
        if args.history_control:
            bench.history_control(max(map(int, args.sizes.split(","))), args.repetitions)
        else:
            bench.execute(args)
    except Exception as exc:
        bench.results["error"] = repr(exc)
        bench.save()
        raise
    finally:
        for fixture in bench.server_fixtures:
            fixture.stop_server()
        bench.save()


if __name__ == "__main__":
    main()
