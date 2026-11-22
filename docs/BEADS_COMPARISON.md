# taskctl 0.3.0 versus Beads 1.3.4

Measured on 22 November 2026. [Harness](../tests/perf/beads_compare.py) · [All raw samples and provenance](../tests/perf/beads_compare_results.json).

For a small local agent team with a small current frontier and growing completed history, taskctl had substantially less CLI latency and memory use in this test. Beads' server mode was much faster than its embedded mode. **All three configurations passed the measured concurrent-write and claim-uniqueness checks.**

These are same-host diagnostic measurements, not a claim of feature parity. Beads includes Dolt version history and broader collaboration features. This test exercises a narrow local workflow and retains each tool's normal storage settings.

**Server mode is the primary Beads comparison for taskctl's intended concurrent-agent workload.** Beads recommends it for multiple writers. Embedded mode is included because plain `bd init` selects it by default; the agent invokes the same `bd` CLI in either mode. The default does not establish how frequently either mode is used in practice.

## Versions and settings

| Configuration | Exact version / source | Storage |
| --- | --- | --- |
| taskctl | 0.3.0, clean commit `51ceee073bf59424ad30aebb5a3f9ef1bcde1ea7`; Go 1.27.4 | modernc SQLite package 1.62.0; WAL; `synchronous=FULL`; 5 s busy timeout |
| Beads embedded | Official Linux amd64 1.3.4 binary, commit `7ef2fd05eb0cd3bdbde051f93cfc14ccf270cdde`; Go 1.26.10 | Default in-process Dolt engine |
| Beads server | Same `bd` binary; standalone Dolt 2.2.0 | Local TCP SQL server, isolated database, same host |

[Beads 1.3.4](https://github.com/gastownhall/beads/releases/tag/v1.3.4) was the latest stable release checked; the 1.4 release candidates were excluded. Its [pinned architecture documentation](https://github.com/gastownhall/beads/blob/v1.3.4/docs/architecture/dolt.md) and [CI installer](https://github.com/gastownhall/beads/blob/v1.3.4/scripts/ci/install-dolt.sh) specify Dolt 2.2.0 for the standalone server. This comparison does **not** measure historical SQLite versions of Beads.

Both Beads modes used their actual default `dolt.auto-commit=on`; auto-export remained off. The architecture page still describes the older server default, but the [release's command source](https://github.com/gastownhall/beads/blob/v1.3.4/cmd/bd/main.go) explicitly defaults both modes to on, and `bd config get dolt.auto-commit` confirmed it. No batching, durability relaxation, or deletion of imported history was used. Telemetry was disabled with the supported `BD_DISABLE_METRICS=1`; Dolt's version-check notice was disabled in an isolated Dolt configuration directory. No remote was configured.

The official Beads archive checksum and Dolt release-asset digest were verified. Binary hashes, compiler metadata and all settings needed to identify the measured builds are in the raw results. The final taskctl binary SHA-256 is `2f4e9f6c4bbbc4b67dd505ea270fa23883a1890fafb830f8518e261002202321`.

## CLI latency at 10,000 tasks

Median milliseconds over seven fresh CLI processes, after two excluded warmups. Parentheses show the observed minimum–maximum; they are not confidence intervals.

| Operation | taskctl | Beads embedded | Beads server |
| --- | ---: | ---: | ---: |
| Show one running task | 4.7 (4.3–5.5) | 1,655.5 (1,416.4–1,763.8) | 83.1 (70.8–585.9) |
| Available / ready, 20 rows | 5.8 (4.8–10.4) | 1,255.0 (1,102.7–1,428.8) | 194.4 (181.6–226.0) |
| In progress, 8 rows | 4.9 (4.0–5.4) | 1,627.6 (1,312.6–1,841.5) | 118.9 (104.7–128.0) |
| Claim an available task | 6.8 (5.4–7.5) | 1,263.8 (1,147.0–1,552.4) | 157.2 (138.2–392.5) |
| Record a short checkpoint | 7.1 (6.0–10.7) | 1,373.7 (1,339.2–1,625.7) | 139.1 (126.7–162.7) |
| Complete a claimed task | 7.9 (7.2–10.0) | 1,597.7 (1,521.9–1,656.8) | 182.6 (153.8–331.2) |

The operation mapping was:

| Intent | taskctl | Beads |
| --- | --- | --- |
| Detail | `tasks show ID --json` | `show ID --json` |
| Available work | `tasks available --limit 20 --json` | `ready --limit 20 --brief --json` |
| Running work | `tasks in-progress --limit 20 --json` | `list --status in_progress --limit 20 --brief --json` |
| Claim | `tasks claim ID --session ACTOR --json` | `update ID --claim --actor ACTOR --json` |
| Checkpoint | `tasks progress ID --claim TOKEN --title TEXT --json` | `update ID --notes TEXT --actor ACTOR --if-assignee ACTOR --if-status in_progress --json` |
| Complete | `tasks complete ID --claim TOKEN --summary TEXT --json` | `close ID --reason TEXT --actor ACTOR --json` |

Checkpoint operations are analogous, not identical. taskctl appends a title-only progress note and changes its latest checkpoint; Beads replaces the current `notes` field while retaining its normal audit/version history. This does not exercise long note bodies or taskctl's separate note-body database. Claim-token fencing and Beads' actor/status guards are also different protocols; this benchmark does not establish equivalent stale-worker protection.

## Scaling with completed history

Every fixture had **40 pending tasks, 8 running tasks and the rest completed**. Each task had the same title pattern, a 1 KiB description and one tag. There were no dependency edges, child tasks or replacement chains. Increasing total task count therefore measured completed-history scaling, not a growing active frontier.

Available-list medians:

| Total tasks | taskctl | Beads embedded | Beads server |
| --- | ---: | ---: | ---: |
| 100 | 6.8 ms | 235.2 ms | 99.8 ms |
| 1,000 | 5.4 ms | 330.3 ms | 135.5 ms |
| 10,000 | 5.8 ms | 1,255.0 ms | 194.4 ms |

taskctl remained roughly flat in this fixture. Beads embedded latency and memory increased substantially; server mode reduced that cost. Task count and import-generated history grew together, so these numbers do not identify the internal cause. No Beads profile was collected.

The 10,000-task Beads fixtures took approximately 115 s and 143 s to initialise/import in embedded and server modes. Setup was excluded from timings and is **not** compared against taskctl's direct database seeding. The comparison stopped at 10,000 because larger imports would dominate the exercise; there is no extrapolated 100,000-task Beads result.

## Memory and concurrent agents

Peak RSS in MiB. Each CLI entry is the largest individual-process peak among measured sequential samples and the concurrency runs. Server peaks cover the post-restart workload and verification; they exclude the preceding bulk import. These are process RSS figures, not Go heap allocation or total system memory.

| Total tasks | taskctl CLI | Beads embedded CLI | Beads server CLI | Additional Dolt server |
| --- | ---: | ---: | ---: | ---: |
| 100 | 9.5 | 158.5 | 90.3 | 179.5 |
| 1,000 | 9.5 | 180.6 | 88.1 | 196.9 |
| 10,000 | 9.5 | 398.0 | 88.5 | 335.7 |

The server was restarted after import so its import memory high-water mark was excluded. TCP-listener readiness after that restart took 83, 82 and 246 ms respectively; this is listener readiness, not complete project initialisation. taskctl and embedded Beads had no resident server. Individual CLI peaks have not been added together and presented as a measured simultaneous memory peak.

Eight workers then wrote checkpoints to eight different claimed tasks, starting through a barrier. Each burst was repeated three times. The table shows median wall time for all eight processes to finish, including the lightweight measurement supervisors; readback verification ran afterwards.

| Total tasks | taskctl | Beads embedded | Beads server |
| --- | ---: | ---: | ---: |
| 100 | 59.0 ms | 3,517.9 ms | 798.0 ms |
| 1,000 | 58.7 ms | 6,109.1 ms | 702.6 ms |
| 10,000 | 39.1 ms | 13,072.9 ms | 689.8 ms |

At 10,000 tasks the three burst ranges were 38.4–57.7 ms, 12,375.1–14,214.6 ms and 640.3–900.3 ms respectively. All **216 of 216** selected writes succeeded and each expected checkpoint was read back. Embedded Beads queued these concurrent CLI writes through its single-writer lock successfully; calling it concurrency-unsafe from this test would be wrong.

At each size, eight different actors also raced to claim one pending task. All nine configuration/size races had exactly one successful claimant, that claimant was the final recorded owner, and the other seven attempts returned the expected already-claimed/conflict error. There were no timeouts or unrelated I/O errors. This verifies the tested claim race, not crash recovery, stale-token fencing, or an arbitrary concurrency history.

## Method and limits

The host was Linux 6.18.57 x86-64 on AMD EPYC 9V74 hardware, exposing nine CPUs through affinity, with an overlay filesystem. Builds and other known CPU-heavy work were paused during each timed window. The shared host still introduces scheduler and storage noise. Processes used `GOGC=100`, no `GOMEMLIMIT`, no forced `GOMAXPROCS`, and no race instrumentation. OS caches were warm; no claim of cold-cache disk performance is made. The existing [Go measurement supervisor](../tests/perf/measure.go) supplied elapsed time and Linux `wait4` peak RSS; stdout went to files.

**Provenance caused a deliberate rerun.** The first run interleaved all three configurations in a fixed random order. Its retained taskctl binary reported 0.3.0 but embedded an older dirty-worktree revision. All of that data remains in `paired_retained_binary`; it is not falsely attributed to the release commit. taskctl was rebuilt from a clean detached `51ceee0` worktree, its clean VCS metadata was checked, and its entire benchmark and event control were repeated. The tables use `clean_taskctl` with the original Beads measurements, so the final cross-tool comparison is **unpaired**, on the same host in separate quiet windows. Original and clean taskctl results support the same broad conclusion, but small differences should not be overinterpreted: the original 10,000-task burst median was 80.4 ms versus 39.1 ms in the clean rerun.

Logical fixture content matched, but stored history was not identical. Beads used its supported JSONL importer, including the events and Dolt history it naturally creates. taskctl has no JSON archive import, so its fixture was seeded directly into a CLI-initialised SQLite database. A separate clean-binary sensitivity check added 10,000 creation events; show/available/in-progress/checkpoint medians remained 4.5–5.6 ms, with at most 9.5 MiB RSS. This checks that the empty initial taskctl event table does not explain the broad result; it does not make the two historical representations identical. Disk sizes are retained in the raw data but are not used for a storage-efficiency claim.

Outputs and final state were checked, rather than counting fast failures as successes. The selected comparison includes 486 successful sequential calls (378 measured and 108 warmups), exact initial/final task counts, SQLite integrity checks, the 216 verified parallel writes, and all nine claim races. Available queries returned exactly 20 rows and running queries exactly 8. At 10,000 tasks, detail output was 1,451 bytes for taskctl and 1,463 for Beads; available lists were 2,446 and 6,783 bytes respectively in each CLI's native JSON format. Beads `--brief` deliberately removed descriptions from lists.

This test says nothing about 10,000 simultaneously active tasks, dense dependency graphs, long note bodies, notification delivery, distributed sync, power-loss durability, or a long-running server memory leak. Those require different workloads. It also compares the official Beads release binary with a local taskctl build using a different Go compiler version, not two binaries rebuilt under identical compiler settings.

## Reproduce

Use Python 3.11+ and the versions above. Build taskctl from the pinned clean commit, not a modified checkout. Download the official [Beads Linux archive](https://github.com/gastownhall/beads/releases/download/v1.3.4/beads_1.3.4_linux_amd64.tar.gz) and [Dolt 2.2.0 Linux archive](https://github.com/dolthub/dolt/releases/download/v2.2.0/dolt-linux-amd64.tar.gz), and check their published digests. The raw artifact records the digests used here.

```sh
git worktree add --detach /tmp/taskctl-0.3-clean 51ceee073bf59424ad30aebb5a3f9ef1bcde1ea7
cd /tmp/taskctl-0.3-clean
go build -o /tmp/taskctl-0.3 ./cmd/taskctl
go build -o /tmp/taskctl-measure ./tests/perf
```

From a checkout containing this harness, with absolute binary paths and a new output directory:

```sh
python3 tests/perf/beads_compare.py \
  --root /tmp/taskctl-beads-run \
  --taskctl /tmp/taskctl-0.3 \
  --beads /absolute/beads/bin/bd \
  --dolt /absolute/dolt/bin/dolt \
  --measure /tmp/taskctl-measure
```

The default sizes are 100, 1,000 and 10,000. Each fixture is isolated; server processes started by the script are stopped on normal completion or a Python exception. Use `--kinds taskctl` for the clean-baseline replay, or add `--history-control --kinds taskctl --sizes 10000` with a different new output directory for the creation-event sensitivity check. The harness writes its raw `results.json` incrementally and checks results as it runs. The checked-in artifact bundles the original paired run, clean replay and both history controls; its `selection` field identifies the data used in the tables.
