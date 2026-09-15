---
title: "Two branches took daemon API contract 2 for different shapes: the equality gate passed a plugin that would have failed the handshake, the tester's boot-against-main's-release check caught it, and the branch renumbered to 3"
category: legion
tags:
  - legion
  - daemon-api-contract
  - pi-envoy
  - rebase
  - serial-numbers
  - testing
date: 2026-09-13
status: active
module: contracts
related_issues:
  - "LEGION-16"
  - "LEGION-20"
  - "LEGION-52"
  - "sjawhar/legion#961"
  - "sjawhar/legion#975"
  - "sjawhar/legion#1018"
symptoms:
  - "the branch daemon boots against main's released plugin and passes the contract gate, although the branch changed request and response shapes the plugin does not know"
  - "two open pull requests both bump LEGION_DAEMON_API_VERSION 1 -> 2 for different reasons"
---

# Two branches took contract 2 for different shapes; the second to land renumbered to 3

`LEGION_DAEMON_API_VERSION` (`packages/contracts/src/legion-daemon-api.ts`) and the plugin
manifest's `legion.daemonApiVersion` (`packages/pi-envoy/package.json`) are compared for equality
when the daemon boots (`verifyLegionPluginContract`). The gate exists because a plugin built
against different HTTP shapes fails the controller/architect boot handshake with nothing in the
daemon's log to say why. This note records the one way the gate can be wrong — two branches
claiming the same number for different shapes — and the decision rule that came out of it, which
completes the rule `schema-bump-branch-rechecks-mains-version-at-every-rebase.md` and
`../daemon/plugin-daemon-api-contract-version-gate.md` already record for the opposite case.

## What happened

LEGION-16 (pull request #961, the interactive controller) changed four shapes: `ompSessionFile`
on `controllerLocator` in the state response and on `/controller/ready`, the `/grants` request as
a union with a controller form, and `merge: true` on `/gh-token` only. It bumped
the contract 1 → 2 on both sides in its first pass. While the branch was open, LEGION-20 (#975)
merged its own 1 → 2 for the design gate's `stateGate` and `GatesRegister` shapes, and pi-envoy
1.23.0 through 1.24.x were released from `main` declaring 2.

The branch rebased over #975 cleanly for the contract file — both sides said `2`, so jj saw no
conflict — and every local gate stayed green: `daemon-api-version.test.ts` pins the constant and
the manifest *equal*, which they were. The tester's round-nine step zero found it: the branch's
daemon, booted against a copy of `main`'s installed 1.24.0 plugin tree, **passed** the contract
gate. That plugin does not know the controller's session file, the grants union, or the merge
flag; its strict schemas would have rejected the first `/legion/v1/state` response
(`controllerLocator: Unrecognized key "ompSessionFile"`) — the silent handshake failure the gate
was built to prevent, now with the gate saying everything was fine.

The branch renumbered to 3. The tester's round ten then proved the handshake both ways: the
branch daemon refused 1.23.1 and 1.24.0 by name (`speaks daemon API contract 2; this daemon
requires 3`), refused a contract-1 build, booted against the plugin built from the branch, and
`main`'s schema rejected the state that daemon served.

## The decision rule, both halves

`main`'s notes record LEGION-52's case: it had bumped 1 → 2 so a daemon would refuse plugin
releases without the credential file; LEGION-20 took 2 first, and release 1.23.0 — the first to
declare 2 — already wrote the credential file, so LEGION-52 dropped its bump. Renumbering to 3
would have refused a release that already had the behaviour. That note draws the general
conclusion that a contract integer, unlike a migration version, is not renumbered when `main`
takes it first.

LEGION-16 is the other half, and the rule that covers both is one question: **does the release
that first declares the new number already carry the shapes your branch needs?**

- Yes (LEGION-52): drop your bump; the number is a skew detector and one bump serves both.
- No (LEGION-16): renumber above it. A release declaring your number without your shapes would
  pass the gate and fail the handshake — exactly the failure the number exists to name.

The answer is read from the release, not from the branch: `npm view @sjawhar/pi-legion-envoy@<v>
legion` for the number, and the plugin's bundled schemas (or simply booting the branch daemon
against that installed release) for the shapes. The state-version rule is unconditional — two
branches can never share a migration step — and the contract rule is this conditional; the check
before every push is the same for both:

```bash
jj -R "$LEGION_WORKSPACE" file show -r main@origin packages/contracts/src/legion-daemon-api.ts | grep 'LEGION_DAEMON_API_VERSION ='
jj -R "$LEGION_WORKSPACE" file show -r main@origin packages/daemon/src/daemon/legion-state.ts | grep -o 'z.literal([0-9]*)'
jj -R "$LEGION_WORKSPACE" file show -r main@origin packages/pi-envoy/package.json | grep daemonApiVersion
```

Run it at every phase boundary, not only when GitHub says CONFLICTING: both collisions on this
repository produced clean rebases.

## The test that decides it, and why only the tester ran it

Nothing in either package's suite can catch this: the equality test compares two files on the
same branch, and the daemon's gate test builds its manifest from the same constant. The one check
that sees the collision is booting the branch's daemon against the plugin release `main` has
actually shipped — a production-like rig with the installed profile, which is the tester's
surface, not the implementer's unit run. Put "boot against `main`'s current release; a pass is
suspicious when your shapes differ" on the tester's list for every branch that touches
`legion-daemon-api.ts`.

## What renumbering costs, and the text it drags along

Renumbering is a one-line change on each side and a sweep of every sentence that names the old
number: the constant's doc comment, the manifest, the daemon `AGENTS.md` upgrade note and contract
paragraph, `packages/pi-envoy/AGENTS.md`, `docs/solutions/daemon/plugin-daemon-api-contract-version-gate.md`,
and the pull request body's deployment paragraph. `grep -rn 'contract 2\|daemonApiVersion.*2'`
over `packages`, `skills`, `docs`, and the body finds them; a mention that describes `main`,
another issue, or the refusal message stays. The tester found the one this branch missed (the
body's deployment-order sentence) two passes later, so the sweep belongs in the same commit as
the renumber.

Two days later the same doc comment was rewritten on `main` by #1018 (contract history and the
widened rule), which forced a second conflict-only rebase within the hour. Resolving it meant
keeping `main`'s history whole and appending a `3 — LEGION-16` entry after its `2`, in the doc
comment and in both `AGENTS.md` contract paragraphs (`(currently 2)` → 3). A serial's history is
`main`'s text; a branch adds its entry, it does not rewrite the paragraph.

## Related

- `schema-bump-branch-rechecks-mains-version-at-every-rebase.md`: the unconditional rule for the
  state version, and the LEGION-52 half of the contract rule.
- `../daemon/plugin-daemon-api-contract-version-gate.md`: the gate itself and "When another
  branch takes the number first".
- `../daemon/interactive-controller-upgrade-the-headless-pane-probes-alive-and-is-killed-once-after-the-restart.md`:
  the deployment order that installs the contract-3 release before the daemon restarts.
