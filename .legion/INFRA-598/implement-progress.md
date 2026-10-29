# INFRA-598 implementer: state at 2026-10-29 02:20Z (recovery note; not a phase handoff)

Phase: implementing, not complete. PR sjawhar/legion#2462 (draft). Branch head pushed: 9b8c5a69.

## Architect decisions (INFRA-514 architect, Envoy)
- Browser typing (scope B): only on the two replays and one added step `10x-typing`; the curves stay without typing. If the driver is late, replays run without typing first.
- Results document carries only runs with the fixed sampler; the earlier runs are named as an earlier run distorted by the text heap read (29.1% of Dispatch CPU in r16, 2-3% in the curves).
- The stored-order checker is built from the Dispatch commit under test (renderer must match); r16's one mismatch (doc 6d24d231, v57, 4 bytes) was renderer skew (#2272), not a defect (INFRA-707 implementer, 23:52Z).
- 9b92ac14 is "main before the fixes"; production now runs b9883e6a (no INFRA-514 fix in it).

## Runs (raw outputs on dispatch://INFRA-514, logs and evidence on dispatch://INFRA-598)
- lt-20261028t123018-b2a8: full curves + hour + 1x-db4, 9b92ac14, OLD sampler (distorted 2-3%). Results v1: dispatch://INFRA-514/artifact/cluster-load-test-2026-10-28-md. Knee 8x->8.5x (delivery p99 from arrival); hour at 10x collapses at ~15 min. Architecture review: Cost added to API-2, BG-1, BG-3 (and ranked-table cells).
- lt-20261028t194853-a6a3: r16/r4 replays, OLD sampler (29% distortion). Superseded.
- lt-20261028t223135-7f42: r16/r4 replays, no typing, FIXED sampler, INFRA-639 b1367e2c code. r16 10.69 cores / 7.53 GiB; r4 10.72 / 7.62; Postgres 0.44 cores; profiling 0.6%. Regenerate CODEPATHS with the HeapReleased fix (783dc4b7) from the uploaded replay tar before writing results v2.
- lt-20261028t154844-2eba: quick `all` at 6e7ff365 with the Dispatch pod deleted in 2x (negative control): PR comment and dispatch://INFRA-598/artifact/cluster-load-test-quick-lt-20261028t154844-2eba-log-txt.
- Negative controls (lock, no image, pg major): dispatch://INFRA-598/artifact/cluster-load-test-negative-controls-2026-10-28-txt.

## Remaining
1. Final run at the head (after INFRA-715's soak releases the lock; message INFRA-715's implementer when done). Run from a copy of packages/envoy with GIT_DIR=/legion/repos/github.com/sjawhar/legion/.git:
   up --namespace loadtest --storage-class premium-rwo --dispatch-commit 9b92ac14351a926361c982edb7953e0325fab0de --pg-major 18 --production-repo <deployment repo> --stored-order-source <legion/INFRA-707 head> --browsertype-commit <legion/INFRA-707 head with loadtest/browsertype>;
   seed; series --duration 10m (refines); collect --upload-issue INFRA-514; soak --duration 60m; step 10x-typing 10 --duration 10m --typing; collect; replay-prepare; replay r16-typing --from 0 --to 39m --typing; collect; db-size 4 15Gi; step 1x-db4 1 --duration 10m; replay r4-typing --from 0 --to 39m --typing; collect --upload-issue INFRA-514; down.
2. Results v2 on INFRA-514 (fixed-sampler runs only; replay tables beside 11.1-13.8 cores / 13.8 GiB with residual; stored-order per run with its checker commit; anchor-placement and sign-flipped notes; copy Postgres 18.2 vs production 18.6); update the API-2/BG-1/BG-3 Cost text to the new numbers.
3. implement.json (proof array), PR body (E2E line, Threads, Size), push, handoff_complete.

## Update 2026-10-29 09:48Z
- 08:52Z: started the final run (lt-20261029t085250-7c56), stopped it at once and tore it down so INFRA-715's 1.5 h soak could go first. No cluster run of mine is in progress; namespace held by INFRA-715.
- Regenerated code-path tables of lt-20261028t223135-7f42 (HeapReleased fix): dispatch://INFRA-514/artifact/codepaths-heapreleased-lt-20261028t223135-7f42-tar-gz. r16: 10.69 cores / 7.53 GiB, residual 0.41-3.11 cores, 6.27 GiB; non-Go 1.28 GiB.
- Final run: same command as above with --stored-order-source and --browsertype-commit = a6b64ddb21782dc5e5680433a9934fbd707925c2 (legion/INFRA-707; checker built from 9b92ac14 + INFRA-707's files, renderer_matches true). Starts when INFRA-715 messages that its lock is gone.
- INFRA-707 acceptance (architect, 09:30Z): after my baseline, unless its tester is ready first (next free slot, never interrupting a run): r16 and 10x-typing with Dispatch and checker from the head the tester names (architect dispatches the branch image: run 40850134172 for a6b64ddb), plus the same on the newest main ancestor of that head that has an image, named in the result.
