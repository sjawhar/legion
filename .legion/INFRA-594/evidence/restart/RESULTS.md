# INFRA-594: a listener restart gives every session its backlog

Ten runs of `packages/envoy/loadtest/restart-step.sh` at the head of this workspace
(`37cdf6dde92a88bcf78490240f706c18f60ac417`, working copy with this branch's changes, so `meta.json` `build_dirty` is `true`), on this issue's pod: 3 vCPU, gVisor, nothing else running. Each run is one `step-10x` load step under consumer delivery (40 events/s, 1,000 sessions, 600 requests/s, a 300 s window, `LOADTEST_SESSION_DELIVERY=consumer`), with:

- 60 s into the window, one envelope to each session (`loadgen backlog -per 1`), which every session answers `done`, so each holds a resume point. The runs without resume points leave this out and register without one (`LOADTEST_NO_RESUME=1`).
- 120 s into the window, `SIGKILL` to the listener the stack started.
- 2 s after the kill, the backlog: 3 envelopes to each of the 1,000 sessions over 25 s (`loadgen backlog -sessions 1000 -per 3 -over 25s`), 3,000 in all, each on its session's own agent subject over JetStream under its own `Nats-Msg-Id`.
- 30 s after the kill, the listener again, with the same binary, arguments and environment. A start that exits at once (`bind: address already in use`, which gVisor holds for a while after the kill) is started again after 1 s; the `starts` column counts the attempts and `listener down` ends at the exec of the start that stayed up.

**Every session has its backlog** is `backlog_ms_after_restart.max` of `<run>/restart-report.json` (`loadgen restart-report`): over the 1,000 sessions, the last frame of each session's backlog, counted from the exec of the listener that stayed up. A session's backlog is every first frame whose message the stream stored while the listener was down, plus every one stored before the kill and received after it. The commands, the fields and the duplicate rules are `packages/envoy/loadtest/README.md`, "The listener restart". Every number below comes from the files in this directory.

## with resume points (n=5)

| run | every session has its backlog (s) | per-session p50 (s) | per-session p99 (s) | listener down (s) | starts | restart to first answer (s) | backlog frames | envelopes received once | missing | publishes not acknowledged | duplicate frames | `summary.json` duplicates | sessions on consumer |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| restart-resume-1 | 3.98 | 3.70 | 3.98 | 30.0 | 1 | 0.99 | 3,000 | 3,000 of 3,000 | 0 | 1 | 0 | 0 | 1,000 |
| restart-resume-2 | 3.17 | 3.04 | 3.13 | 30.1 | 1 | 1.06 | 3,000 | 3,000 of 3,000 | 0 | 0 | 0 | 0 | 1,000 |
| restart-resume-3 | 4.20 | 3.60 | 4.20 | 36.9 | 7 | 1.11 | 3,000 | 3,000 of 3,000 | 0 | 0 | 0 | 0 | 1,000 |
| restart-resume-4 | 4.78 | 4.70 | 4.78 | 32.3 | 3 | 3.56 | 3,000 | 3,000 of 3,000 | 0 | 1 | 0 | 0 | 1,000 |
| restart-resume-5 | 2.87 | 2.63 | 2.87 | 30.2 | 1 | 0.86 | 3,000 | 3,000 of 3,000 | 0 | 3 | 0 | 0 | 1,000 |

Every session has its backlog in median 3.98 s (best 2.87 s, worst 4.78 s).

## without resume points (`LOADTEST_NO_RESUME=1`) (n=5)

| run | every session has its backlog (s) | per-session p50 (s) | per-session p99 (s) | listener down (s) | starts | restart to first answer (s) | backlog frames | envelopes received once | missing | publishes not acknowledged | duplicate frames | `summary.json` duplicates | sessions on consumer |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| restart-noresume-1 | 4.57 | 4.34 | 4.51 | 31.1 | 2 | 1.83 | 3,000 | 3,000 of 3,000 | 0 | 0 | 0 | 0 | 1,000 |
| restart-noresume-2 | 2.92 | 2.75 | 2.92 | 33.7 | 4 | 0.85 | 3,000 | 3,000 of 3,000 | 0 | 1 | 0 | 0 | 1,000 |
| restart-noresume-3 | 3.60 | 3.45 | 3.60 | 30.1 | 1 | 1.61 | 3,000 | 3,000 of 3,000 | 0 | 0 | 0 | 0 | 1,000 |
| restart-noresume-4 | 3.83 | 3.66 | 3.83 | 34.7 | 5 | 1.51 | 3,000 | 3,000 of 3,000 | 0 | 0 | 0 | 0 | 1,000 |
| restart-noresume-5 | 3.58 | 3.27 | 3.55 | 37.7 | 7 | 0.84 | 3,000 | 3,000 of 3,000 | 0 | 0 | 0 | 0 | 1,000 |

Every session has its backlog in median 3.60 s (best 2.92 s, worst 4.57 s).

## Duplicates

**No duplicate frame in any of the ten runs.** `duplicate_frames` above is the number of relay frames whose stream sequence their session had already received, which `loadgen restart-report` lists one by one with its session, event id, stream sequence, whether it came before or after the kill, whether the session had answered the earlier copy `done`, and what the real plugin would do with it (`ledger_dedupe`, `unwritten_redelivery`, or `shown_twice`, which is a defect and fails the report). Every run has an empty `duplicates` list, so no copy was classified, none reached a session twice, and nothing would reach a transcript twice under the plugin's rules. Each run's own `summary.json` `sessions.delivery.duplicates`, which the sessions count as they answer, carries the same 0.

## What else the runs show

- Every one of the 3,000 backlog envelopes reached its session exactly once in every run, and none was missing (`backlog_envelopes_received_once`, `backlog_envelopes_missing`).
- `publishes not acknowledged` counts the backlog publishes whose JetStream acknowledgement did not come back readable (`nats: invalid jetstream publish response`). The stream stored them all the same: the sessions received 3,000 of 3,000 in those runs too. The old tool counted no such publish at all; this one does.
- Every session was on consumer delivery in every run (1,000 of 1,000, 0 on core), so these runs measure the session-consumer path.
- The step itself fails by design in each run, because its sampler sees the killed listener's pid exit (`summary.json` `stack_exits`); `TABLES.md` carries that mark.

## The bar

The plan's bar is that a listener restart gives every session its backlog within 10 s. The worst of the ten runs is **4.78 s**, and the worst of each set is 4.78 s with resume points and 4.57 s without. **The bar holds in every run.**

## Re-deriving a run's numbers from these files

Every number above comes from the three files each run directory carries, so any reader can derive it again without the pod:

```bash
run=.legion/INFRA-594/evidence/restart/restart-resume-4
d=$(mktemp -d) && cp "$run"/restart.json "$run"/backlog.json "$d"/ \
  && gunzip -c "$run"/relay-frames.csv.gz >"$d"/relay-frames.csv \
  && (cd packages/envoy && go run ./loadtest restart-report -run "$d")
```

It prints what `restart-report.json` of that run holds.

## The files of each run

| File | What it is |
| --- | --- |
| `restart-report.json` | `loadgen restart-report`'s output: the numbers above. |
| `restart.json` | The kill, the first start attempt, the exec that stayed up and its first answer (Unix ns), and the start attempts. |
| `backlog.json`, `warm.json` | The backlog runs' summaries: the event id range and every publish counted. `warm.json` is absent from the runs without resume points. |
| `summary.json`, `TABLES.md`, `meta.json` | What every load step writes: the step's results, its tables, and its arguments, build and window. |
| `relay-frames.csv.gz` | Every relay frame of the run: session, subject, stream sequence, `Envoy-Delivered`, store and receipt time, event id, duplicate, the frame's dedupe identity and whether the session answered it `done`. The report reads this. |
| `listener.log.gz` | The listener's log of the run, the killed listener and the restarted one together. |

