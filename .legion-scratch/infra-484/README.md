# INFRA-484 throwaway cross-language wire proof

Two files, both throwaway. They are not part of the branch and must never be committed.

Restore them into the workspace at the head that merges:

```sh
cp /legion/scratch/infra-484-proof/infra484_wire_proof_test.go \
   "$LEGION_WORKSPACE/packages/daemon/internal/daemon/infra484_wire_proof_test.go"
cp /legion/scratch/infra-484-proof/infra484-wire-proof.ts \
   "$LEGION_WORKSPACE/packages/pi-legion/infra484-wire-proof.ts"
```

Run it:

```sh
cd "$LEGION_WORKSPACE/packages/daemon" && \
  LEGION_TEST_PG_DSN=postgres://legion:legion@127.0.0.1:5433/legion \
  go test ./internal/daemon/ -run TestINFRA484ReadyForUpdateWireProof -count=1 -v
```

Delete both files again afterwards; `jj -R "$LEGION_WORKSPACE" status` must show nothing.

## What it drives

- `infra484_wire_proof_test.go` is the daemon half, in package `daemon` so it reuses the round
  tests' rig helpers (`isolatedOutboxPool`, `seedWorkflowTree`, `mustClaimToken`,
  `podUpdateLocator`, `newSupervisor`, `supervision.reconcilePodUpdates`). It seeds two claims of
  one issue in one pod on a Postgres database of its own with this branch's real migrations,
  restores them through the real supervisor, serves `api.NewServer` on a real TCP listener,
  registers both claims over `POST /legion/v1/claims/register`, scripts the fake runtime to report
  pod drift, ticks the real sweep, and counts `ReplacePod` on the fake runtime.
- `infra484-wire-proof.ts` is the agent half, run under Bun from `packages/pi-legion` (the package
  whose `node_modules` resolves `@legion/pi-shared`). It calls
  `executePhaseOp({op: "ready_for_update"})`, which calls
  `createLegionDaemonClient(base).readyForUpdate(...)` over the global `fetch`. It wraps the global
  `fetch` only to print the request body, parse that body with the route's Zod request schema
  (`LegionReadyForUpdateRequest`) and print the status; it writes no request of its own. Exit 0 is
  the op's answer, exit 3 a refusal.

The lift path is: `ready_for_update` op → `LegionReadyForUpdateRequest` shape → HTTP →
`internal/api`'s `readyForUpdate` → `supervise.PodUpdateSignalled` on the claim machine → Postgres →
the sweep's `ReplacePod`.
