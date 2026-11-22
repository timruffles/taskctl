import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import sqlite3
import subprocess


parser = argparse.ArgumentParser()
parser.add_argument("--root", required=True, type=Path)
parser.add_argument("--binary", required=True, type=Path)
parser.add_argument("--measure", required=True, type=Path)
parser.add_argument("--sizes", default="16,128", help="body sizes in MiB")
args = parser.parse_args()
root = args.root.resolve()
root.mkdir(parents=True, exist_ok=True)
binary, measure = args.binary.resolve(), args.measure.resolve()
base = {k: v for k, v in os.environ.items() if not k.startswith("TASKCTL_")}
for key in ("GOMEMLIMIT", "GODEBUG", "GOMAXPROCS"):
    base.pop(key, None)
base["GOGC"] = "100"
block = (b"Markdown evidence.\n" * 4000)[:65536]
results = []


def run(env, directory, *command):
    process = subprocess.run(
        [str(binary), *command], cwd=directory, env=env,
        text=True, capture_output=True, check=True,
    )
    return json.loads(process.stdout)


def fixture(name):
    directory = root / name
    directory.mkdir()
    env = base.copy()
    for key, value in (("XDG_DATA_HOME", "data"), ("XDG_STATE_HOME", "state"), ("XDG_CONFIG_HOME", "config")):
        env[key] = str(directory / value)
    project = run(env, directory, "projects", "create", "--name", name, "--json")
    env["TASKCTL_PROJECT"] = project["id"]
    task = run(env, directory, "tasks", "create", "--title", "Streaming notes", "--brief", "Record evidence", "--json")
    task = run(env, directory, "tasks", "claim", task["id"], "--session", "perf", "--json")
    database = Path(env["XDG_DATA_HOME"]) / "taskctl/projects" / project["id"] / "tasks.sqlite"
    return directory, env, task, database


def measured(directory, env, label, command, discard=False):
    output = Path(os.devnull) if discard else directory / f"{label}.stdout"
    process = subprocess.run(
        [str(measure), "-timeout", "120s", "-output", str(output), "--", str(binary), *command],
        cwd=directory, env=env, text=True, capture_output=True, check=True,
    )
    result = json.loads(process.stdout)
    assert result["exit"] == 0 and not result["timed_out"], result
    result.update(fixture=directory.name, operation=label)
    results.append(result)
    print(json.dumps(result), flush=True)
    (root / "results.json").write_text(json.dumps({
        "method": "Fresh CLI processes, GOGC=100, no race or memory cap; peak RSS from wait4. Diagnostic runs, not a throughput guarantee.",
        "platform": platform.platform(), "runs": results,
    }, indent=2) + "\n")
    return None if discard else json.loads(output.read_text())


for mib in map(int, args.sizes.split(",")):
    size = mib * 1024 * 1024
    directory, env, task, database = fixture(f"stream-{mib}")
    body = directory / "body.md"
    digest = hashlib.sha256()
    with body.open("wb") as out:
        for offset in range(0, size, len(block)):
            chunk = block[:min(len(block), size-offset)]
            out.write(chunk)
            digest.update(chunk)
    note = measured(directory, env, "write", [
        "tasks", "progress", task["id"], "--claim", task["claim_id"],
        "--title", "Evidence saved", "--body-file", str(body), "--json",
    ])
    assert note["body_bytes"] == size and note["sha256"] == digest.hexdigest()
    measured(directory, env, "read", ["notes", "read", note["id"]], discard=True)
    measured(directory, env, "show", ["tasks", "show", task["id"], "--json"])
    body.unlink()

    directory, env, task, database = fixture(f"legacy-{mib}")
    with sqlite3.connect(database) as db:
        stamp = task["updated_at"]
        db.execute("UPDATE tasks SET progress=CAST(zeroblob(?) AS TEXT) WHERE id=1", (size,))
        event = db.execute("INSERT INTO events(task_id,kind,message,data,created_at) VALUES(1,'progress',CAST(zeroblob(?) AS TEXT),'{}',?)", (size, stamp)).lastrowid
        for table, column, row in (("tasks", "progress", 1), ("events", "message", event)):
            with db.blobopen(table, column, row) as blob:
                for offset in range(0, size, len(block)):
                    blob.write(block[:min(len(block), size-offset)])
        db.execute("DROP TABLE notes")
        db.execute("UPDATE store_info SET value='1' WHERE key='schema_version'")
    current = measured(directory, env, "upgrade", ["tasks", "show", task["id"], "--json"])
    note = run(env, directory, "notes", "show", current["progress_note_id"], "--json")
    assert note["body_bytes"] == size and note["sha256"] == digest.hexdigest()
    with sqlite3.connect(database) as db:
        assert db.execute("SELECT count(*) FROM notes").fetchone()[0] == 1
    measured(directory, env, "read", ["notes", "read", note["id"]], discard=True)
