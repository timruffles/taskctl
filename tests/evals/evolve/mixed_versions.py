#!/usr/bin/env python3
"""Verify an already-running 0.3 watcher while 0.4 evolves its task."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import tempfile
import time


def verify(old, new):
    with tempfile.TemporaryDirectory(prefix="taskctl-mixed-") as directory:
        root = Path(directory)
        env = {k: v for k, v in os.environ.items() if not k.startswith("TASKCTL_")}
        for key, part in (("XDG_DATA_HOME", "data"), ("XDG_STATE_HOME", "state"),
                          ("XDG_CONFIG_HOME", "config")):
            env[key] = str(root / part)
        project = ""

        def call(binary, *args):
            command = [str(binary)] + (["--project", project] if project else []) + list(args) + ["--json"]
            result = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True, timeout=10)
            if result.returncode:
                raise RuntimeError(f"{args}: {result.stderr}")
            return json.loads(result.stdout)

        project = call(old, "projects", "create", "--name", "mixed-version-check")["id"]
        task = call(old, "tasks", "create", "--title", "Deliver parser", "--brief", "Implement and verify the parser.")
        task = call(old, "tasks", "claim", task["id"], "--session", "worker")
        watch_path, err_path = root / "watch.jsonl", root / "watch.stderr"
        with watch_path.open("w") as out, err_path.open("w") as err:
            watcher = subprocess.Popen([str(old), "--project", project, "tasks", "watch", task["id"],
                                        "--timeout", "15s", "--interval", "10ms", "--json"],
                                       cwd=root, env=env, stdout=out, stderr=err)
            try:
                def records():
                    lines = watch_path.read_text().splitlines(keepends=True)
                    return [json.loads(line) for line in lines if line.endswith("\n")]

                def observed(predicate):
                    deadline = time.monotonic() + 5
                    while time.monotonic() < deadline:
                        found = records()
                        if predicate(found):
                            return found
                        if watcher.poll() is not None:
                            raise RuntimeError(f"watcher exited early: {watcher.returncode}: {err_path.read_text()}")
                        time.sleep(0.01)
                    raise TimeoutError("old watcher did not observe the expected event")

                observed(lambda rows: any(r.get("type") == "snapshot" and r["snapshot"]["outcome"] == "pending" for r in rows))
                plan = root / "plan.json"
                plan.write_text(json.dumps({
                    "summary": "Parser implemented; error handling and integration remain.",
                    "replacements": [
                        {"key": "errors", "title": "Verify errors", "brief": "Add error tests and run them."},
                        {"key": "integration", "title": "Verify integration", "brief": "Add integration tests and run them."},
                    ],
                    "continue_with": "errors",
                }))
                children = call(new, "tasks", "evolve", task["id"], "--claim", task["claim_id"],
                                "--revision", str(task["revision"]), "--file", str(plan))
                observed(lambda rows: any(r.get("event", {}).get("kind") == "superseded" for r in rows))
                call(new, "tasks", "complete", children[0]["id"], "--claim", children[0]["claim_id"], "--summary", "Error tests passed.")
                observed(lambda rows: any(r.get("event", {}).get("kind") == "completed" and r["event"]["task_id"] == children[0]["id"] for r in rows))
                partial = {}
                for label, binary in (("old", old), ("new", new)):
                    partial[label] = call(binary, "tasks", "watch", task["id"], "--once")["snapshot"]["outcome"]
                if partial != {"old": "pending", "new": "pending"} or watcher.poll() is not None:
                    raise AssertionError(f"unfinished work completed early: {partial}")
                second = call(new, "tasks", "claim", children[1]["id"], "--session", "tester")
                call(new, "tasks", "complete", second["id"], "--claim", second["claim_id"], "--summary", "Integration tests passed.")
                if watcher.wait(timeout=5) != 0:
                    raise RuntimeError(err_path.read_text())
                events = records()
                if events[-1].get("outcome") != "completed":
                    raise AssertionError(events[-1])
                states = {label: call(binary, "tasks", "show", task["id"])["status"]
                          for label, binary in (("old", old), ("new", new))}
                if states != {"old": "superseded", "new": "evolved"}:
                    raise AssertionError(states)
                db_path = Path(env["XDG_DATA_HOME"]) / "taskctl/projects" / project / "tasks.sqlite"
                with sqlite3.connect(db_path) as db:
                    schema = db.execute("SELECT value FROM store_info WHERE key='schema_version'").fetchone()[0]
                if schema != "2":
                    raise AssertionError(schema)
                return {"result": "passed", "partial_outcomes": partial, "final_outcome": "completed",
                        "public_states": states, "schema": schema, "old_watcher_records": events}
            finally:
                if watcher.poll() is None:
                    watcher.kill()
                    watcher.wait()


def binary_info(path):
    with path.open("rb") as stream:
        digest = hashlib.file_digest(stream, "sha256").hexdigest()
    return {"version": subprocess.check_output([str(path), "version"], text=True).strip(), "sha256": digest}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--old", type=Path, required=True)
    parser.add_argument("--new", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    old, new = args.old.resolve(), args.new.resolve()
    result = {"old_binary": binary_info(old), "new_binary": binary_info(new), **verify(old, new)}
    args.output.write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps({k: v for k, v in result.items() if k != "old_watcher_records"}, indent=2))


if __name__ == "__main__":
    main()
