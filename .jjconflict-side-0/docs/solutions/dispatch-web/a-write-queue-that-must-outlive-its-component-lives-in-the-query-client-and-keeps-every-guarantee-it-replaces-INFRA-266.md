---
title: "A write queue that must outlive its component lives in the query client, and keeps every guarantee it replaces"
category: dispatch-web
tags:
  - tanstack-query
  - optimistic-writes
  - mutation-scope
  - remount
  - coalescing
  - error-paths
date: 2026-10-25
status: active
module: packages/dispatch/web/src/features/settings/ProjectsSection.tsx
related_issues:
  - "INFRA-266"
  - "sjawhar/legion#2178"
---

# A write queue that must outlive its component lives in the query client, and keeps every guarantee it replaces

A control whose save can still be out when the control unmounts (the reader leaves the page,
closes a drawer, changes route) keeps its write state in TanStack's `MutationCache`. Component
state does not work for this.

- **Order and coalesce in the cache.** Give each entity a mutation `scope: { id }`, so its saves
  run one at a time across mounts. Supersede inside `mutationFn` when the save's turn comes: if
  the newest pending mutation for the key is not this one (compare the `variables` object by
  identity, one fresh object per pick), send nothing. Only the newest pick then follows the save
  in flight, whichever mount made it. A `useRef` queue behind `useSubmitGuard` breaks on a
  remount: the hook-level `onSettled` still runs after the unmount and sends the old instance's
  queued pick after the remounted instance's newer one.
- **Read what the reader must see on return from the cache too.** That means the value being
  saved and the refusal. `useMutation().error` and `isPending` belong to the mounted observer and
  are gone after an unmount. Read the newest mutation for the key with `useMutationState`. Set
  `gcTime: Number.POSITIVE_INFINITY`, because the default 5 minutes drops a refusal nobody has
  seen. Prune the entity's older settled mutations in `onSettled` so the cache stays bounded.
- **Before copying a hook, pick the precedent whose lifetime matches yours, not the one whose UI
  looks like yours.** Grep `scope:`, `useMutationState` and `gcTime: Number.POSITIVE_INFINITY`
  under `packages/dispatch/web/src`. `features/project/board-moves.ts` (a per-project `scope`) and
  `features/agents/BroadcastSends.tsx` (infinite `gcTime`, rows from `useMutationState`, a manual
  `cache.remove`) already had the shape this change needed. `features/issue/useIssueFieldWrite.ts`
  keeps its queue in a ref.
- **When you move state from one scope to another, list every property the old scope enforced and
  keep a test for each.** The properties nobody names are the ones that get dropped. Changing the
  expected wire order in an existing test is a behaviour change, not test maintenance. It needs
  the ruling of whoever owns the behaviour (the architect), not the implementer's judgement.
- **For a native `<select>`, coalescing is a requirement, not an optimisation.** Each ArrowDown
  on a closed select fires `change`, so a reader passing through the list makes one pick per key
  press. Here every stored intermediate pick is a project default that an issue created at that
  instant keeps for good.

## Relying on TanStack internals

The final hook relies on three behaviours of `@tanstack/query-core` 5.102.8 that are not public
API. Re-check them on an upgrade. The browser rows below catch a break in each.

- `MutationCache.findAll` lists mutations in creation order, so its last element is the newest
  pick.
- A scoped mutation that is waiting already has status `pending`, with its `variables`, and its
  `onMutate` has already run. `mutate` dispatches both before the scope lets it run.
- The hook-level `onSuccess`/`onSettled` run before the mutation's final status dispatch, so a
  save settling is still `pending` while its `onSettled` prunes the others.

A superseded save ends as `success`, with data `{ superseded: true }`. Every reader of a result
must check for that: today `onSuccess` and `onSettled` do. A future `isSuccess`, a toast, or a
cache-level `onSuccess` would count it as a save.

## Evidence

The Settings default-assignee picker went through four implementations. Each was wrong in a way
the previous round's tests could not see.

1. One shared mutation for every row (88a3d03d). The select snapped back to the stored value
   while the PUT was out and stayed enabled, so a second pick sent a second PUT at the same time.
   The reviewer found it with a probe that held the PUT.
2. A queue per row in component state (bf2e0861), modelled on `useIssueFieldWrite`. It held for
   one mounted row. The reviewer reproduced the remount case: pick bob (held), pick carol, unmount,
   remount, pick dave. The PUT order was `[bob, dave, carol]`, so carol was stored. Browser negative
   control: `settings.e2e.ts:551` expected `alice`, received `carol`.
3. A `scope` per project, with the shown value from `useMutationState` (9dd03557). This survived
   the remount, but the coalescing came out along with the queue. Every intermediate pick was sent
   and stored. Its own unit test had been changed to "expects every pick in order". The tester
   measured it: with 800 ms saves, six picks 100 ms apart stored bob, carol, dave, alice, bob,
   carol over 5.3 s while the control showed carol, and four ArrowDown presses stored four people
   in turn. Separately, a save refused while Settings was closed was never reported, because the
   error lived in the unmounted observer.
4. Scope, plus superseding in `mutationFn`, plus the error read from the cache with infinite
   `gcTime` and pruning (c3620d14). The tester's two red rows passed unmodified:
   `settings.e2e.ts:569` (wire `sent bob, answered bob, sent alice, answered alice`) and `:627`
   (the refusal alert on return). The tester then drove the built page: a refusal still shown
   after 320 s away, past the default `gcTime`; 40 picks against 30 ms saves stored and showed the
   last pick, with one entry left in the project's mutation cache.
