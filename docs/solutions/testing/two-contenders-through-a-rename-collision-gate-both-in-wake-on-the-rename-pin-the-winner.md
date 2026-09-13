---
title: "Two contenders through a rename collision: gate both into the race, wake on the in-process rename, and pin which one won"
category: testing
tags:
  - race-conditions
  - deterministic-reproduction
  - bun-test
  - fs-watch
  - atomic-rename
  - mutation-testing
  - last-writer-wins
  - ensureRepoClone
date: 2026-09-13
status: active
module: packages/workspace/src/workspace.test.ts
applies_when:
  - A unit test must drive two concurrent callers of one function through a "second one yields" branch (rename collision, exclusive create, lock loser)
  - The fake runner controls when each contender's slow step finishes, but the branch under test is reached only after a real filesystem effect the fake does not own
  - A concurrency test asserts only the end state and a reviewer asks which of the two contenders' artifacts survived
related_issues:
  - "LEGION-55"
  - "sjawhar/legion#1014"
  - "LEGION-28"
  - "sjawhar/legion#980"
---

# Two Contenders Through a Rename Collision

`ensureRepoClone` (`packages/workspace/src/workspace.ts`) clones into a temporary sibling
`<repo>.clone-XXXXXX` and renames it into place once `.jj` exists. When two issues provision the
same repository for the first time at once, the caller whose rename lands second gets
`ENOTEMPTY`/`EEXIST`, sees `.jj` at the final path, and returns — the other clone won, ours is
surplus. LEGION-28 landed that branch (`sjawhar/legion#980`) without a unit test; LEGION-55's one
change is the test that pins it, in `workspace.test.ts` ("two issues cloning the same repository
for the first time at once both provision; the clone that lands second yields to the one already
in place"). Three things in that test are reusable; the first two are about reaching the branch
deterministically, the third about proving the test observes the right outcome.

## 1. Gate both contenders *into* the race before either can finish

The obvious fake — first `jj git clone` returns at once, second waits for the first's rename —
is not enough, because the branch has a precondition the fake does not control: both callers
must have passed `ensureRepoClone`'s entry check (`if (existsSync(repoCloneDir)) { if
(existsSync(jjDir)) return; … }`) and hold their own temporary destination. Each
`provisionIssueWorkspace` reaches that check only after several awaited fs operations
(`createProvisioningCredential`'s `mkdir`/`mkdtemp`/`writeFile`/`chmod`, then `mkdir`,
`mkdtemp`). Their completion order across two callers is the thread pool's, not the source
order's. If caller A's clone returns immediately and A's rename lands before B's `existsSync`,
B sees a complete clone, never clones, and the test passes with **one** clone and no collision
— a vacuous green for exactly the branch it claims to cover.

The gate closes that: the *first* clone waits on a `Promise.withResolvers()` that the *second*
clone's invocation resolves.

```ts
const bothCloning = Promise.withResolvers<void>();
// in the fake run, for `jj git clone`:
cloneTargets.push(target);
if (cloneTargets.length === 1) {
  await bothCloning.promise;
} else {
  bothCloning.resolve();
  await whenPathExists(path.dirname(repoCloneDir), jjDir);   // §2
}
await mkdir(path.join(target, ".jj"), { recursive: true });
```

Once the second `jj git clone` has been invoked, both callers are provably past the entry
check with a temporary directory each; two clones always run. This is the "gate the racing
side with promises you own" technique from
[race-regression-tests-that-fail-before-the-fix](race-regression-tests-that-fail-before-the-fix.md)
applied to the *entry* of the race rather than its exit — count the preconditions of the branch
under test and gate each one, not just the interleaving you can see.

## 2. Wake the loser on the winner's rename — an in-process `fs.watch`, within stated bounds

The second clone must not return until the first caller's rename has landed, and that rename
happens in product code after the fake returns — there is no promise to hand back. The
observable is the filesystem: `.jj` under the final path. The test waits on it with

```ts
function whenPathExists(watchedDir: string, target: string): Promise<void> {
  if (existsSync(target)) return Promise.resolve();
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  const watcher = watch(watchedDir, () => {
    if (!existsSync(target)) return;
    watcher.close();
    resolve();
  });
  watcher.on("error", reject);
  if (existsSync(target)) { watcher.close(); resolve(); }   // landed between check and arm
  return promise;
}
```

called as `whenPathExists(path.dirname(repoCloneDir), jjDir)` — the watched directory is the
clone's **parent**, the one whose entries the rename changes. No timer, no sleep, no tick budget.

[wait-for-a-subprocess-file-by-polling-not-by-watching](wait-for-a-subprocess-file-by-polling-not-by-watching.md)
rules `fs.watch` out for waiting on a file another *process* writes, because Bun names a rename
event after its source and coalesces near-simultaneous events in one directory — a creation
event can fire, fail the existence check, and swallow the rename that follows. That rule stands.
This test is the bounded case where none of its failure conditions can occur, and copying
`whenPathExists` anywhere else requires re-checking all three:

1. **The writer is in-process and provably runs after the watcher is armed.** `bothCloning.resolve()`
   only schedules the first caller's continuation as a microtask; the second caller's fake then
   calls `whenPathExists` synchronously — `existsSync`, `watch()` (the inotify watch is created
   in the constructor; a missing directory throws synchronously), the re-check — before its
   `await` yields. JavaScript's single thread orders the arm before the rename. A subprocess
   writer gives no such guarantee.
2. **The rename is the only event the watched directory sees between arming and resolution.**
   Both `.clone-*` temporaries were created by `mkdtemp` *before* either fake ran; the first
   caller's `mkdir(<temp>/.jj)` and `writeFile(<temp>/.jj/clone-origin)` fire inside the
   temporary, not in the watched parent (inotify is not recursive). The first parent event after
   arming is the rename itself, so there is nothing for it to coalesce into. A test that creates
   its temporary *after* arming, inside the watched directory, reintroduces the polling doc's
   22-of-300 miss.
3. **The callback tests the target's existence, never the event's name or type.** Whatever Bun
   calls the event, `existsSync(target)` is what decides.

If any of the three does not hold — a real subprocess, a temporary created after arming, a
multi-step write with no rename — use the polling doc's bounded 20 ms poll instead, and say why
in the helper's doc comment. The architect's brief for this test forbade timers outright
("wait on the observable filesystem condition"); the watcher was the only shape that satisfied
both that and the three bounds.

## 3. End-state assertions admit last-writer-wins; pin the winner with an origin marker

The first version of the test asserted: both specs returned, `.jj` exists at the final path,
`readdir(parent)` is exactly `["widgets"]` (no `.clone-*` leftover), two `jj git clone` commands
with distinct `.clone-` destinations, both workspace directories present. Every one of those is
also true of a **wrong** implementation in which the loser removes the landed clone and renames
its own over it:

```ts
// the reviewer's mutant, replacing `return;` in the collision branch
if ((code === "ENOTEMPTY" || code === "EEXIST") && existsSync(jjDir)) {
  await rm(repoCloneDir, { recursive: true, force: true });
  await rename(tempDir, repoCloneDir);
  return;
}
```

It passed the first version 5 of 5. The test's claim is "the second yields to the one already in
place" — a statement about *which* clone survives — and nothing in it could tell the two clones
apart. The fix is two lines: give each contender's artifact an identity, then assert the
survivor's identity.

```ts
// fake clone, after creating .jj:
await writeFile(path.join(target, ".jj", "clone-origin"), target, "utf8");
// assertions:
expect(await readFile(path.join(jjDir, "clone-origin"), "utf8")).toBe(cloneTargets[0]);
```

Against the mutant the assertion now fails every run (`Expected: …widgets.clone-jyuZqh /
Received: …widgets.clone-p79mgo`); against the real code it passes 13 of 13 in the suite. The
general rule: when a test's title names a winner and a loser, some assertion must read a value
that only the winner's path produces. Presence, absence, counts, and distinctness of inputs never
do that; a marker written by each contender into its own artifact does.

Run the mutant yourself before opening the PR. The recipe and the `jj restore`-by-path revert are in
[mutation-proof-probe-tests](mutation-proof-probe-tests.md) § "How to run the mutation check in
place"; record the mutant's exact `Expected`/`Received` in the PR body, since "fails" alone does
not show the test observes the right thing. LEGION-55's first negative control (making the
collision `return` unreachable → `ENOTEMPTY` propagates) proved the test *reaches* the branch;
only the overwrite mutant proved it *distinguishes the branch's outcome*. Both controls belong in
the body: reachability and discrimination are different claims.

## Related

- [race-regression-tests-that-fail-before-the-fix](race-regression-tests-that-fail-before-the-fix.md)
  — owned gates for the interleaving you control; this document adds gating the race's *entry*
  preconditions and waking on an effect the fake does not own.
- [wait-for-a-subprocess-file-by-polling-not-by-watching](wait-for-a-subprocess-file-by-polling-not-by-watching.md)
  — the general rule against `fs.watch` in tests; §2 above is its bounded in-process exception.
- [mutation-proof-probe-tests](mutation-proof-probe-tests.md) — the mutation method and the
  in-place revert recipe.
- [loop-a-flaky-suite-sequentially-parallel-lanes-of-it-starve-its-timer-tests](loop-a-flaky-suite-sequentially-parallel-lanes-of-it-starve-its-timer-tests.md)
  — how the same issue's tester detected the original race in the daemon suite.
