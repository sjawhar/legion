# INFRA-605 implementing round 5 (review 5689714159 at 3ae385df): progress at the 02:30Z daemon roll

Pushed head d496213b. Done in this round:
- Main merged forward twice (c55e05f0, ad4d0c50; #2481's one-line pmdoc import in the second).
- store.ConnectionsPerProcess counts roomAppendsPoolSize; infra/gcp dispatchConnectionsPerPod 86 -> 94;
  README table, running-dispatch.md and the changie fragment say 94 = 64 + 12 + 8 + 4 + 1 + 1 + 1 + 3.
- Correctness 1 (head notice during a room load kept and re-checked at keepInstance) and 2 (woke/connected decide
  off the listener goroutine), commit 1f6e81b5; tests stale_room_decisions_test.go fail at ad4d0c50 and 5e119d38
  ("timed out waiting for the room that loaded behind the other process's write to be closed";
  "the evictor heard nothing for <doc>").
- Test quality 3-5 and every cleanup, commit d496213b (roomScanned removed; registerConnectionLocked; awaitDecided
  waits on deciding; one finish(); gate takes through at head(); head_liveness.go; store.QueueExec; healthPool();
  append_gate.go, room_instance.go, head_notice.go, head_tracker_test split four ways; marks.go typed slices).
- Lanes at d496213b: docs ok 768.5 s, api ok 872.9 s; rest/multiprocess/infra/race not finished.

Left in this round:
1. Re-run cmd/dispatch, store, events, pglisten, loadtest, -tags multiprocess, infra/gcp, -race docs; read CI at head.
2. If the PR conflicts, merge main forward again (main has #2482/#2484 fixing pmdoc: take main's version).
3. Reply "Fixed in <sha>: …" on the 10 threads (PRRT_kwDORFy7ds6xUlaW, …lad, …lal, …lar, …lav, …la1, …la-, …lbB,
   …lbH, …lbL) and resolve each with resolveReviewThread.
4. PR body: `## Contract change census` (AppendUpdateWithCredit: 0 hits for the old name, implementers = PgVersioned and
   the test wrappers; socket cap refusal 401 -> 503: packages/dispatch/web/src/features/doc/connection.ts reads only
   DOCUMENT_SCHEMA_CLOSE_CODE, a failed upgrade surfaces as close 1006 either way, packages/envoy/infra has no reader;
   dispatch_doc_head/ping: only docs/head_notice.go and head_liveness.go use them, an older build ignores them, rooms do not
   follow an older build's writes during a roll), with each search command; Size, Chain, figures 94, threads list.
5. implement.json round 5, push, handoff_complete.

## 2026-10-29 10:20Z (before the worker-image drain)
- Main (88840d35, with #2449) merged forward a third time: 3e13f14a. No conflicts left; vet/build ok.
- Lanes at 3e13f14a: docs ok 792.8 s; api fails only TestARepeatedSubReplacesAnotherKind, which fails the same way on
  main@origin alone (#2449's own test, a 2-core timing flake; passes alone); cmd/dispatch, store, events, pglisten, loadtest
  ok; multiprocess ok 644.7 s; infra/gcp fails only the two data-disk tests needing mkfs.ext4. Race docs not yet re-run.
- Connection total unchanged: 94 = 64 + 4 + 8 + 1 + 12 + 1 + 1 + 3; #2449 changed the dashboard's live socket, not the
  document socket, so the caps count as before.
- Still left: race docs lane, read CI at the head, answer and resolve the 10 threads, contract change census + Size/Chain
  lines in the PR body, implement.json, handoff_complete.
