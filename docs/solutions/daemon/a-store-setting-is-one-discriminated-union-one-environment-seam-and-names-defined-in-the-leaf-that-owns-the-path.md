---
title: "A store setting is one discriminated union, one environment seam, and variable names defined in the leaf that owns the path — with the module-cycle finding that decided where"
category: daemon
tags:
  - config
  - session-store
  - discriminated-union
  - podEnvironment
  - k8s-manifests
  - module-cycle
  - TDZ
  - DRY
date: 2026-09-15
status: active
module: packages/daemon/src/daemon/config.ts (SessionStore, parseSessionStore), packages/daemon/src/daemon/runtime-kubernetes.ts (podEnvironment, spawnSerialized), packages/daemon/src/daemon/k8s-manifests.ts (SESSION_STORAGE_VARIABLE, SESSION_SQL_DSN_FILE_VARIABLE), packages/daemon/src/daemon/boot-probes.ts
related_issues:
  - "LEGION-81"
  - "sjawhar/legion#1108"
  - "LEGION-80"
---

# A store setting is one discriminated union, one environment seam, and variable names defined in the leaf that owns the path

## Context

`runtime.kubernetes.session_store: pvc | postgres` selects where a pod's Oh My Pi session lives.
Under `postgres` every pod of a tree receives two variables that are Oh My Pi's own
(`OMP_SESSION_STORAGE=sql`, `OMP_SESSION_SQL_DSN_FILE=/var/run/legion/providers/<key>`), and
the init container skips its recorded-session stat. Sami's bar for the change (2026-09-13, on
LEGION-80): "top-quality and DRY". Three shapes met it and are reusable for the next setting of
this kind.

## 1. The config seam is a discriminated union, so a consumer cannot see the mode without its key

```ts
export type SessionStore = { kind: "pvc" } | { kind: "postgres"; dsnSecretKey: string };
export type SessionStoreName = SessionStore["kind"];
```

Two flat fields (`sessionStore: string; sessionDsnSecret?: string`) admit the impossible state
"postgres without a key" and force every reader to re-check it. The union makes `podEnvironment`
read `sessionStore.dsnSecretKey` only inside `kind === "postgres"`, and `VerifyWorkerImageDeps`
takes just the `SessionStoreName`. The parser applies the cross-field rules once, in a fixed
order, each refusal naming the field: enum → postgres-requires-key → key non-empty → key is a
Secret data key (`[-._a-zA-Z0-9]+`, what keeps `<mount>/<key>` one path segment) → key is not one
of the two variable names (the worker shim exports every providers key into Oh My Pi's
environment under the key's own name, so such a key would shadow the daemon's value) →
pvc-refuses-the-key (an inert key is refused, never ignored — the `omp_launch_prefix` rule).

One of those rules is a blocklist, and blocklists rot: the "key must not be one of the two
variable names" check enumerates exactly the variables the daemon itself sets. If the fork gains
a third `OMP_SESSION_*` variable the daemon puts on a pod, the check must grow with it, or an
operator's Secret key of that name silently shadows the daemon's value through the shim's
export. The check imports the two constants rather than retyping them, so grepping their
definition site finds every place a third would have to be added.

State each fact once. The review caught two twice-stated ones: the accepted store names lived in
a tuple beside the union's literal kinds (`SESSION_STORES = ["pvc", "postgres"] as const` and
`{ kind: "pvc" } | { kind: "postgres" }`), and the key's character class was retyped inside its
own error message. The fix derives the names from the union and lets the compiler check the list
both ways — `Object.keys({ pvc: true, postgres: true } satisfies Record<SessionStoreName, true>)`
refuses a missing or a stray key — narrows with `find` (not `.some()` plus a cast) into an
exhaustive `switch`, and names the class once (`SECRET_DATA_KEY_CLASS`) for both the `RegExp`
and the message.

The tmux clause of the acceptance ("`session_store` under `runtime: tmux` fails startup naming
the field") needed no code: the two keys exist only inside the `runtime.kubernetes` mapping, so
a top-level `session_store` is `Unknown config key "session_store"` and
`runtime: { tmux: { … } }` hits the existing mapping rule. Pin it with a test anyway — it is a
property of the schema's shape, and a later schema change could quietly widen it.

## 2. The store adds to a pod's environment in exactly one place

`podEnvironment` (`runtime-kubernetes.ts`) is where the main container's environment is shaped
for every pod of a tree — root, sub-architect, phase worker all pass through `spawn`. The two
variables are added there and nowhere else; `buildPodManifest` keeps mapping `input.env`
verbatim and `PodManifestInput` never learns about stores; volumes, mounts, affinity, and the
init container's environment are byte-identical under both stores (the sessions `subPath` mount
stays: only the transcript moves, artifacts and `models.db` do not).

Say exactly what the seam is, though. The store has a *second* read in `spawnSerialized` —
under postgres the init container is not handed `LEGION_RESUME_SESSION_FILE`, since the
transcript is a row it cannot stat — so "the one place the store shapes a pod" was inexact and
the review flagged it; "the one place the store *adds to a pod's environment*" is true. The
proof that nothing else changed is a test that spawns a pod under each store and asserts the
postgres pod's env equals the pvc pod's env plus exactly the two entries — compare against a
second harness's real pod, not a hand-written fixture (`buildPodManifest` appends
`LEGION_TERMINATION_GRACE_SECONDS`, which a fixture would miss).

`HOME` and `OMP_PROFILE` are the image's (`worker.Dockerfile` `ENV`) and the daemon never sets
them: a replacement pod is identical in both by construction, which is what lets it open the
same row (the row key embeds the home-relative sessions root). Pin the *absence* of an override
on both generations of the resume test rather than adding two variables to every pvc pod to
"make sure".

## 3. Where the variable names live — and the cycle finding that decided it

The plan said: import `SESSION_STORAGE_VARIABLE` from `boot-probes.ts` (LEGION-80 uses that
name for its host probe). On `main` it was a bare `const`, not an export — and exporting it
would not have helped `config.ts`, which needs both names for its reserved-key check.
`boot-probes.ts` computes `IMAGE_PROBE_TIMEOUT_MS = DEFAULT_SLOW_COMMAND_TIMEOUT_SECONDS * 1000`
at module top level from a `config.ts` export, so `config.ts` → `boot-probes.ts` → `config.ts`
is a load-order cycle that throws a TDZ `ReferenceError` whenever `config.ts` loads first. A
two-file Bun reproduction settled it in one minute (`a.ts` imports a name from `b.ts`; `b.ts`
computes a top-level constant from `a.ts`'s export; `bun a.ts` fails) — do that before arguing
about import graphs.

Both names are therefore defined once in `k8s-manifests.ts`, and `boot-probes.ts` imports the
first from there. State the *real* placement reason in the comment: they sit beside
`PROVIDERS_DIR`, the mount the second name composes a path under, and `config.ts` and
`boot-probes.ts` import them from that leaf. The first draft justified the placement with the
cycle alone, which overstates — `config.ts` would also have been cycle-free (it is
`boot-probes.ts` that is not) — and the review asked for the honest reason. A placement comment
that gives a wrong reason is worse than none: the next reader "fixes" it.

## What the reviewer's clean-up list looked like, so the next one is shorter

Every non-blocking item was a fact stated twice or a sentence slightly wider than the code:
the names tuple beside the union; the regex retyped in its message; "the one place the store
shapes a pod"; a comment justifying placement by a cycle that did not apply to the alternative;
two probe-cache tests that duplicated an existing cache-hit test or pinned key-absence (an
implementation choice) over the contract; a docs sentence saying "every relaunch" where the code
says "every relaunch that passes `--resume`". None changed behaviour, all landed in one commit
in the same corrective push. Reading your own diff once for "is this fact already stated
somewhere, and is this sentence exactly as wide as the code" removes most of them before review.
