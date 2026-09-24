# Classification fixtures

The shipped Bun reducer suite records these fixtures when `LEGION_RECORD_FIXTURES=1`. The preloaded recorder is `packages/daemon/src/daemon/__tests__/fixture-recorder.ts`; it writes one canonical, SHA-256-named JSON record for each classifier input.

To re-record, run the daemon suite from `packages/daemon`:

```sh
LEGION_RECORD_FIXTURES=1 LEGION_E2E=1 LEGION_TMUX_LIVE=1 bun test
```

Stage 7 deletes this recorder, its preload, and these fixtures after the Go classifier replay replaces them.
