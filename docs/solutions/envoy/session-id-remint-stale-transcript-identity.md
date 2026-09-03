---
title: "OMP re-mints session ids on fork/handoff (and on esc-esc rewind in 18.1.0–18.1.2) — identity baked into a transcript silently rots"
category: envoy
tags:
  - omp
  - pi-envoy
  - envoy
  - session-identity
  - session-branch
  - session-switch
  - rebind
date: 2026-09-03
status: active
module: pi-envoy
problem_type: logic_error
severity: high
symptoms:
  - "Agent identifies itself to peers with a session id that does not match its envelopes' source_session"
  - "Peers flag a mismatch between envelope reply_to (live, correct) and the id the agent names in its message bodies (stale)"
  - "envoy_whoami output from earlier in the transcript names a different id than a fresh envoy_whoami call"
root_cause: "OMP re-mints the session id under a continuing conversation on /fork and /handoff — and, on omp 18.1.0–18.1.2 only, on esc-esc rewind; pi-envoy rebound correctly but nothing told the agent, so identity claims already baked into the conversation kept naming the retired id"
resolution_type: code_fix
resolution: "PR sjawhar/legion#775 — pi-envoy rebind injects a non-turn-triggering steer notice naming the previous and new ids whenever the id changes under a continuing transcript"
---

## Problem

An agent running OMP + the pi-envoy extension sent messages that named a stale session id
in their body text, while the Envoy envelope (`source_session`) stamped the correct live id.
The reporting agent blamed `envoy_whoami` serving "a stale registration for this same cwd
… before my own session had registered with Envoy." That diagnosis was wrong: the registry,
`envoy_whoami`, and pi-envoy's rebind logic behaved correctly the entire time. The real
defect was an omission — nothing told the agent that a background OMP mechanic (a human
rewinding the conversation) had re-minted its session id out from under its transcript.

**Version scope of the rewind trigger:** esc-esc rewinds re-minted session ids only on omp
18.1.0–18.1.2. Upstream commit `cc8c3e97dd` (#10565, released in v18.1.3) reverted rewinds
to in-place `navigateTree` branching — the old path stays reachable in `/tree` and the id
survives — and restored `doubleEscapeAction: tree`. `/fork` and `/handoff` still re-mint on
every version, so the notice remains live for those (and stays correctly silent for
in-place rewinds via its same-id guard).

## Symptoms

- Self-introductions and quoted ids inside message *bodies* did not match the envelope's
  `source_session`; two peers flagged the disagreement.
- One continuous conversation spanned three session ids (`01a06584-9add`, `01a0658e-0f5b`,
  `01a0659a-e041`) over 24 minutes, each new id born of a human rewind, yet the transcript
  kept surfacing text baked in under the first id.
- Peers holding the agent's earlier self-announced id could no longer reach it: the old
  agent topic is intentionally closed on rebind and the old registration expires, so the
  drift is a reachability failure, not just a cosmetic label mismatch.
- `envoy_whoami`, checked at any single instant, always returned the correct current id —
  which is why the stale-registry theory looked plausible but was never true.

## What Didn't Work

- **Blaming `envoy_whoami` / the registry.** Registry rows for the lineage were
  `self_subscribed`/`port 0` throughout, and the installed pi-envoy dist (0.4.0) matched
  repo source — no version skew, no stale-row bug in the rebind path.
- **Trusting transcript-old identity.** Any whoami output the agent had emitted earlier was
  treated as still authoritative. It wasn't: rewinds after that text was written re-minted
  the id with no error and no signal.
- **Treating `session_switch` as one uniform event.** Blanket-notify would spam benign
  `new`/`resume` transitions; blanket-silence misses branch/fork/handoff. The fix required
  discriminating switch reasons.

## Solution

Root cause was established from durable evidence, not point-in-time tool calls:

1. **Decode UUIDv7 mint timestamps.** OMP session ids are UUIDv7; the first 48 bits are
   epoch millis (`date -u -d @$((16#<hex12> / 1000))`). The three ids minted at 04:26:24,
   04:36:43, 04:50:43 — one active conversation, not three unrelated sessions.
2. **Read session JSONL headers** (`~/.omp/agent/sessions/<dir-slug>/*.jsonl`). A
   `parentSession` holding a *file path* is the signature of OMP `createBranchedSession`
   (rewind/branch), which mints a new id while carrying the transcript forward.
3. **Grep the final transcript for every candidate id.** All three ids plus 21
   `envoy_whoami` calls coexisted in the surviving transcript — the first id's whoami
   output was still being echoed after two rewinds.

The fix (PR [sjawhar/legion#775](https://github.com/sjawhar/legion/pull/775),
`packages/pi-envoy/extensions/envoy.ts`) adds an identity-change notice to `rebind`:

```ts
const rebind = async (
  reason: SessionSwitchReason | undefined,
  context: SessionContext
): Promise<void> => {
  if (defaults.natsUrls.length === 0) return;
  const previousID = sessionID;
  try {
    await establishSession(context);
  } catch (error) { /* degrade with a ui.notify warning, unchanged */ }
  if (previousID === "" || previousID === sessionID) return;
  if (reason === "new" || reason === "resume") return;
  pi.sendMessage(
    {
      customType: "envoy-message",
      content: encode({
        envoy: {
          notice: "session id changed",
          previous_session_id: previousID,
          session_id: sessionID,
          detail: "…re-run envoy_whoami and re-announce the new id…",
        },
      }),
      display: true,
    },
    { deliverAs: "steer", triggerTurn: false }
  );
};

pi.on("session_switch", (event, context) => rebind(event.reason, context));
pi.on("session_branch", (_event, context) => rebind(undefined, context));
pi.on("session_tree", (_event, context) => rebind(undefined, context));
```

Design choices, deliberate:

- **Silent** for `new`/`resume` — both install a transcript that already matches its own id.
- **Notifying** for `session_branch` (rewind) and `session_switch` reasons `fork` and
  `handoff` — branch/fork carry the transcript, and handoff its agent-written summary
  (`sessionManager.newSession({parentSession})` in `session-handoff.ts`, which then emits
  `session_switch` with `reason: "handoff"`), forward under a freshly minted id.
- **Fails open** for unknown future reasons: a spurious notice is noise; a missed one is
  this incident again.
- **Non-turn-triggering steer** (`triggerTurn: false`): the human just rewound; the notice
  lands when the agent next runs instead of starting an unsolicited turn.
- **Fires even when the NATS rebind itself failed**: `sessionID` is assigned before any
  await in `establishSession`, and the id change is a transcript fact, not a network fact.

Verified on the real surface: a QA agent drove an actual omp v18.1.2 TUI — `/fork` rendered
the notice with both ids (and persisted exactly one `envoy-message` entry in the forked
JSONL); `/new` stayed silent. Six contract tests pin the branch/fork/handoff/new/resume/
first-id/failed-rebind behaviors.

## Why This Works

The defect was never in identity computation — every component answered correctly at every
instant it was queried. The defect was identity *propagation across time within a
transcript*: OMP's fork/handoff mechanics (and, in 18.1.0–18.1.2, rewinds) may re-mint the session id underneath a
continuing conversation, and nothing invalidated the identity claims the agent had already
written into its own context. An agent reasoning from its transcript cannot know a fact it
recorded ten messages ago has been invalidated by an out-of-band event. The notice hooks
the exact seam where the id changes and tells the one party that cannot otherwise find out.

## Prevention

- **Never trust an old `envoy_whoami` result or self-introduction from your own transcript
  as current.** Re-run `envoy_whoami` after any identity-change notice, and re-announce the
  new id to every peer you introduced yourself to under the old one (their side has no
  other way to learn it changed). `skills/envoy/SKILL.md` now documents this.
- **When debugging "wrong session id" reports, reconstruct lineage from durable evidence**:
  UUIDv7 mint times, session JSONL `parentSession` chains, and transcript greps beat any
  point-in-time tool check.
- **Verify host semantics against the release tag of the running build** (e.g.
  `git show v<pinned-version>:<path>` in the oh-my-pi fork), not a shared workspace's
  working copy — checkouts lag releases, and a lagging copy can silently miss whole enum
  members like a `session_switch` reason.
- **Deployment gotcha:** merging a pi-envoy fix does not change any session's behavior.
  omp v18.1.2 ignores a repo-root `package.json` `omp.extensions` manifest — sessions load
  the *published* `@sjawhar/pi-legion-envoy` dist from the global plugin dir unless an
  extension path is passed explicitly (only the Legion daemon does that). A fix goes live
  via the release-pi-envoy workflow (npm publish on merge) plus a plugin reinstall. See
  `docs/solutions/envoy/omp-extension-mcp-mounting.md`.

## Known residual

Peers who saved the old id still cannot reach the session after a rewind (old topic closed,
old registration expired). The notice mitigates operationally via re-announcement; a real
fix is server-side (grace-period dual subscription vs listener aliasing) and needs a design
decision.
