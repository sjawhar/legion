---
title: "Wait for a subprocess's output file by polling for it, not by watching its directory — and have the writer rename it into place"
category: testing
tags:
  - bun-test
  - fs-watch
  - inotify
  - flake
  - atomic-rename
  - e2e
  - tmux
  - worker-shim
date: 2026-09-13
status: active
module: packages/daemon/src/daemon/__tests__/real-deployment-instructions-e2e.test.ts
related_issues:
  - "LEGION-17"
  - "sjawhar/legion#956"
symptoms:
  - "JSON.parse('') / 'Unexpected end of JSON input' reading a file a subprocess just created, in roughly 5% of runs"
  - "an fs.promises.watch() loop never wakes for a file that is present and complete when the 30 s AbortSignal fires"
  - "a real-tmux E2E that passes 50 times in a row, then fails 9 of 250 under load with the awaited file on disk"
---

# Wait for a subprocess's output file by polling for it, not by watching its directory

The deployment-instructions E2E (`real-deployment-instructions-e2e.test.ts`) launches a real
tmux pane through a real `legion worker-shim`; the process standing in for OMP
(`cli/__tests__/fixtures/argv-recorder-omp.ts`) writes the argv it received to `argv.json` in its
cwd and exits. The test has no promise to await — the writer is a grandchild in another process
tree — so it must wait for the file. Three shapes were tried in PR #956; only the last is right.

## 1. Existence is not content: the writer must rename into place

The first recorder did `Bun.write("argv.json", …)`. That is `openat(O_CREAT)` followed by a
separate `write`, and a waiter that fires on the file's appearance can read it between the two:
the reviewer reproduced 17 empty reads (`JSON.parse("")`) in 300 runs. The fix that must survive
any waiting strategy: write the whole record to a temporary name and `renameSync` it to the final
name. A rename is atomic on the same filesystem, so `existsSync(final)` being true means the
content is complete. `writeFileSync("argv.json.tmp", …); renameSync("argv.json.tmp", "argv.json")`
is the entire recorder body.

## 2. Bun's directory watcher coalesces the rename into the temp file's creation

With the rename in place the test still waited on `for await (const _ of watch(dir, { signal }))`
and checked `existsSync(final)` on each event. It then timed out 9 of 250 runs — with `argv.json`
present and whole at the moment the timeout fired. Isolated probes (300 real subprocess writers
per design, Bun 1.3.14) showed why:

- Bun reports a rename as **one** event, named after the **source** file (`rename:argv.json.tmp`),
  which is the same name it already reported for that file's creation moments earlier.
- When two events for the same directory land close together, the second is coalesced into the
  first. The creation event fires, the check finds no `argv.json` yet, and the rename's event never
  arrives — the loop sleeps until the abort.
- Measured misses out of 300: temp file in the watched directory **22**; a separate completion
  marker file written after the data **45** (the marker's own creation coalesces the same way);
  rename in from the *parent* directory **0**, because the watched directory then sees exactly one
  event. That last design was landed in the corrective round and the loop passed 150 of 150.

The parent-directory trick worked, but it needed a paragraph in the fixture to explain itself and
it depended on a watcher behaviour nobody wants to own. The reviewer asked for the sibling shape
instead.

## 3. Poll for the file, exactly like the directory's other waiters

```ts
async function waitForFile(target: string): Promise<void> {
  for (let attempt = 0; attempt < 1500; attempt += 1) {
    if (existsSync(target)) return;
    await Bun.sleep(20);
  }
  throw new Error(`argv record never appeared at ${target}`);
}
```

This is `waitForSocket` in `real-shutdown-e2e.test.ts` and `waitFor` in `ci-fixtures.ts` with a
different predicate: a 20 ms tick, a 30 s bound, and a failure that names the path. Polling has no
watcher to coalesce, so the temp file goes back beside the record (`argv.json.tmp` → `argv.json`,
still an atomic rename). 50 of 50 loop iterations passed at a one-minute load average of 104–111
on 32 cores, 1.9–3.4 s wall each.

The `ts-no-test-timers` rule flags the `Bun.sleep`. This is the rule's stated exception: an
integration test observing a real subprocess with no in-process signal to await. Say so in the
helper's doc comment, keep the tick small, and never let the sleep stand in for the awaited
condition — poll the observable itself.

## Rules

- A file another process writes is complete only if the writer renamed it into place. Check the
  writer before trusting `existsSync` — a plain `Bun.write`/`writeFile` is never enough.
- Do not wait on `fs.watch`/`fs.promises.watch` for a file's arrival in a test. It reports the
  source name on rename and coalesces near-simultaneous events; both defeat an existence check.
- Poll with the directory's existing helper shape (20 ms tick, explicit bound, path in the error).
  The bound belongs to the *failure*, not to the success path.
- Measure a flake fix with the loop it was found by: the reviewer ran 300 iterations; run at
  least that many under the same test command before calling a race closed, and record the load
  average alongside the count — several of the 300-run failures in this issue were tmux itself
  being killed on timeout at a load average of 245, not the race.

## Related

- [socket-tests-observe-the-peer-not-the-clock](socket-tests-observe-the-peer-not-the-clock.md) —
  the same principle for sockets: wait for the peer's observable, poll the condition, keep any
  real wait tiny and commented.
- [race-regression-tests-that-fail-before-the-fix](race-regression-tests-that-fail-before-the-fix.md)
  — the loop that proves a race is closed must have shown the race first.
