# Dispatch for every session

**Date:** 2026-09-04
**Status:** approved, in implementation

## Problem

Agents ask Sami questions he cannot answer: "still yours? these seven things" with no
context, buried mid-transcript, across a dozen sessions. He needs one dashboard listing
every question an agent is waiting on, each self-contained enough to answer without the
transcript, with a handle to jump back to the session.

Dispatch exists for this (GitHub-issue threads + dashboard + reply routed to the asking
session) but today it is Legion-shaped and half-broken:

- `parent` is mandatory: every thread must be a sub-issue of an existing issue. Interactive
  sessions almost never have one.
- Nobody tells interactive agents to use it; all guidance lives in Legion roles.
- The dashboard is dead (`GET /` → `dashboard build not found`): the SPA is bind-mounted
  from a checkout path that no longer exists. The image does not contain it.
- Replies reach the asking session only through per-repo GitHub webhooks, which exist on two
  repos (`sjawhar/legion`, `acme-org/example-repo`) and already drift (`legion` lacks
  `pull_request_review`). Every other repo is deaf.
- The running container carries `legion-implementer`'s OAuth client, not the dispatch App's.

## Decisions (settled with Sami)

- **Explicit only.** Agents call `dispatch` when appropriate; the built-in `ask` is not
  mirrored or replaced. Agents that know the tool exists use it.
- **Repo routing:** thread lands in the cwd's GitHub repo by default; an explicit `repo`
  argument overrides. No configured fallback repo. Cwd not in a GitHub repo and no `repo`
  → the tool errors and names the fix.
- **`parent` optional.** Given → sub-issue link + breadcrumb as today. Absent → top-level
  issue.
- **Context is forced at the contract:** `body` splits into required `context` and
  `question`. Descriptions say the reader has not seen the transcript.
- **Provenance travels with the thread:** host, machine, cwd, tmux target — enough for a
  human to jump back. Session id omitted (tmux target is the handle).
- **Webhook coverage moves to the App** (tier 1, below). Ingress consolidation of all
  Legion events onto the App hook is a separate, deferred decision (tier 2).
- **The dispatch App is `legion-envoy`** (id <app-id>, client `<client-id>`),
  transferred to `acme-org` on 2026-09-04. `legion-implementer` /
  `legion-reviewer` remain write/verdict identities only; untouched.

## Design

### 1. Tool contract (`packages/envoy/internal/dispatch/{mcp,core}`)

```
dispatch({
  subject:   string            // one line, the decision
  context:   string  REQUIRED  // what you are doing, what you found, why you are stuck.
                               // The reader has NOT seen your transcript.
  question:  string  REQUIRED  // the ask: current → desired → proposed change,
                               // options with tradeoffs, your recommendation
  ask?:      Question[]        // structured options → buttons on the dashboard
  urgency?:  low|med|high|blocking
  repo?:     "owner/name"      // explicit target
  parent?:   "<n>" | "owner/name#<n>[#<commentId>]"
})
```

Repo resolution, first hit wins: qualified `parent` → `repo` → error. The shim (below)
fills `repo` from cwd before the call reaches the server, so the server itself never
guesses. Bare-number `parent` resolves against the resolved repo.

Issue body: frontmatter marker, `## Context`, `## Question`. Marker gains `origin`.
Request id (dedupe) covers repo + subject + context + question + urgency + ask.

`dispatch.defaultRepo` and `dispatch.appClientId` are removed from the config contract
(Go loader, `envoy-client/src/dispatch-config.ts`, `envoy-plugin/src/config/schema.ts`)
and from `~/.config/opencode/envoy.json`.

### 2. Shim (`packages/envoy-client/src/dispatch-mcp-shim.ts`)

The shim runs in the session cwd and is the path shared by all three hosts (OMP,
OpenCode, Claude), so it owns cwd knowledge. On `tools/call` for `dispatch`, when the
arguments lack `repo` and a qualified `parent`, it injects:

- `repo`: from `jj git remote list` (`origin`), falling back to `git remote get-url origin`;
  GitHub `owner/name` parsed from the URL. `origin` is the user's fork, never `upstream`.
- `origin`: `{ host: omp|opencode|claude, machine, cwd, tmux: "session:window.pane" }`
  from the environment. Best effort; absent fields are omitted.

No remote and no `repo` → the shim returns a tool error naming the cwd and the fix.
Writer identity is unchanged: whatever `gh auth token` resolves for that cwd.

### 3. Dashboard (`packages/dispatch/web`)

- **Discovery:** owner-scoped search replaces the manual watched-repos list:
  `is:issue is:open label:dispatch-thread user:<owner>` for every distinct account (user or
  org) the signer's App installations cover, from `/user/installations`. The frontmatter
  parse remains the authenticity filter. New repos need no configuration. SSE events
  trigger a refetch.
- **Thread view:** origin line with a copyable `tmux select-window -t …`; `## Context` /
  `## Question` sections; existing ask form, reply, urgency, close, addressed.
- **Build:** the SPA compiles into the envoy image (bun stage in `docker/Dockerfile`, CI
  build context → repo root). Dispatch deploys like the listener: pinned
  `ghcr.io/sjawhar/legion/envoy` tag, no host bind mount.

### 4. Webhook coverage — tier 1 (this change)

`legion-envoy` App-level webhook → `https://webhooks.example.com/webhook/github`,
secret = `ENVOY_GITHUB_WEBHOOK_SECRET`, events `issues`, `issue_comment`. Installed
all-repos on `sjawhar` and `acme-org`. Same signature header and secret →
listener untouched. The two existing per-repo hooks drop `issues` and `issue_comment` so
nothing is delivered twice. App private key stored as `ENVOY_APP_PRIVATE_KEY_B64` with a
`gh-app-token` profile `envoy` so hook config and installations are API-managed.

Accepted risk, named in the skill: a dispatch into a repo the App is not installed on
creates a thread whose replies never route.

### Tier 2 — deferred decision, not work

Carry the remaining Legion events (`pull_request*`, `check_run`, `workflow_run`, `push`,
`installation*`) on the App hook with `checks:read` / `actions:read`; delete both per-repo
hooks; retire `trajectory-s-legion` (3060279) if dead. Belongs with a Legion-running
session where the daemon's event path can be verified end to end.

### 5. Skill and guidance

`skills/dispatch/SKILL.md` ships in the plugin (`prepack.sh` copies `skills/`). Use
`dispatch` for any question not answerable at the keyboard in seconds, anything
decision-shaped, anything that would otherwise be an end-of-message block. Self-contained
rule; `ask` for option sets; urgency semantics; after dispatching, keep every non-blocked
lane moving — the reply arrives as a steer. Legion skills and roles updated for the new
arguments. One pointer line in Sami's CLAUDE.md "Don't Outsource to the User".

### 6. Testing

- Go: parent-less create, resolution precedence, marker round-trip with `origin`,
  `context`/`question` body rendering.
- Shim: repo/origin injection for jj and git remotes, `upstream`-only ignored, no-remote
  error, explicit args untouched.
- SPA: owner-scoped query, origin rendering, marker parse with new keys.
- E2E on the devbox: dispatch from a private-repo cwd → dashboard shows it → reply from the
  dashboard → asking session receives the steer.

## Rollout

1. App configured (tier 1). 2. Code merged; envoy image published; `pi-legion-envoy`
released and bumped in dotfiles. 3. Container recreated on the pinned image with
`DISPATCH_APP_CLIENT_ID` / `DISPATCH_APP_CLIENT_SECRET` from the secret store;
`~/.config/opencode/envoy.json` trimmed. 4. E2E above.
