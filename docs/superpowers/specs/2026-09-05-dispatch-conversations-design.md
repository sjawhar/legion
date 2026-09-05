# Dispatch threads are conversations

**Date:** 2026-09-05
**Status:** draft, awaiting review
**Supersedes:** one decision in `docs/plans/2026-09-04-dispatch-everywhere-design.md` (see Reversal).

## Problem

Dispatch models a thread as one question with one answer. Real decisions are a
back-and-forth: on `acme-org/example-repo#17158` the first reply was a challenge to the
premise, the agent re-asked with a revised option set, and the dashboard rendered that
follow-up as prose because it only parses questions from the issue body and hides the ask
form as soon as any answer exists. The agent had no tool for a follow-up question and no
guidance; it fell back to `gh issue comment` and typed the options as markdown.

Five further defects on the same dashboard, all observed on live threads:

- Every thread's opening message renders as one dense block. The renderer is fine; agents
  write for the transcript, not for a reader who has not seen it, and nothing stops them.
- The human's answer renders as a YAML/code block: the answer marker is front matter, which
  GitHub and the dashboard render as visible text.
- Nothing on a thread says *which session* asked. The tmux target says where, not which.
  The tool cannot know: it runs as a separate MCP process launched by the plugin, so the
  plugin — which has the session — is not the thing making the call. Host detection reads
  `OMP_SESSION_ID` from that process's environment, a variable set by a personal dotfiles
  extension, not by OMP or the plugin; on any other machine the host is unknown.
- Every GitHub event on any covered repo rebuilds the whole page with `innerHTML = render()`, which
  destroys the reply textarea and whatever was being typed.

## Scope

Everything Sami raised, and nothing he did not. Not in scope, with his reasons: removing the
Go dispatch service or changing who calls GitHub (the service is the seed of Legion's future
tracker and stays in Go); the Legion daemon; the Claude plugin's dependency packaging
(approved separately: publish `@legion/envoy-client`, give the plugin its own lockfile).

## Decisions (settled with Sami, 2026-09-05)

- A thread is **one decision as a conversation on one GitHub issue**. Any turn may carry a
  question. The thread closes when the decision is settled, not when the first reply lands.
- Follow-up questions go through **the same `dispatch` tool** with a `thread` argument. The
  marker format is documented so an agent can post one by hand if it must.
- **Every question carries an id; every answer names the question it answers.** Multiple
  questions may be open on one thread.
- **`sessionId` and `sessionTitle` travel with every turn.** The **tmux target stays** as the
  jump handle; session id/title are identity.
- Agents' prose is held to a **reader-who-was-not-there standard** by the skill, with length
  caps enforced by the service. GitHub references (`#N`, `owner/repo#N`, URLs) are
  **unfurled by the dashboard**, so agents may cite them bare.
- **Markers become HTML comments** so no dispatch plumbing is ever visible to a human.

### Reversal

The 2026-09-04 design settled "session id omitted (tmux target is the handle)." Superseded:
a tmux target tells you where a session is running, not which conversation it is; it does
not survive the session being resumed in another pane and means nothing from another
machine. Both are recorded; tmux remains the one-click jump.

## Design

Components: each host plugin — **pi-envoy** (OMP), **envoy-plugin** (OpenCode),
**claude-envoy-bridge** (Claude Code) — registers `dispatch` as one of its own tools, next
to the `envoy_*` tools it already registers from the shared spec in
`envoy-client/src/tool-contract.ts`. The tool runs in the session, reads the session's
identity and cwd from the host, mints the GitHub token, and calls the **dispatch service**
(Go, `packages/envoy/internal/dispatch`) over stateless HTTP; the service writes to GitHub.
The **dashboard** (SPA, `packages/dispatch/web`) reads GitHub through the service's proxy.

The standalone dispatch MCP server (`envoy-client/src/dispatch-mcp-shim.ts`, launched from
each plugin's `.mcp.json`) is deleted. It existed so three hosts could share one process;
they already share the code instead. What remains in `envoy-client` is a library: the
request shape, cwd → repo resolution, tmux origin, token minting, and the HTTP client to the
service (`dispatch-mcp-bridge.ts`, renamed `dispatch-client.ts`).

### 1. Tool contract

Two modes, one tool. Opening a thread creates an issue; continuing one posts a comment.

```
dispatch({                         // open a thread
  subject:   string
  context:   string  REQUIRED  ≤ 1200 chars
  question:  string  REQUIRED  ≤ 800 chars
  ask?:      Question[]
  urgency?:  low|med|high|blocking
  repo?:     "owner/name"
  parent?:   "<n>" | "owner/name#<n>[#<commentId>]"   // sub-issue attachment; unchanged
})

dispatch({                         // continue a thread
  thread:    "<n>" | "owner/name#<n>"
  context:   string  REQUIRED  ≤ 1200 chars
  question:  string  REQUIRED  ≤ 800 chars
  ask?:      Question[]
})
```

- A follow-up is a comment on an existing issue, so the arguments that describe a new issue
  — `subject`, `urgency`, `repo`, `parent` — do not exist in that mode. The schema is the
  contract; a call mixing the two modes is rejected as an invalid call like any other.
- `thread` must be an open issue whose body carries a dispatch marker; otherwise a loud
  error: `#N is not a dispatch thread` / `#N is closed; open a new thread`.
- Length caps return an error naming the limit and the field. No truncation.
- Dedupe. Opening: unchanged (request id over repo+subject+context+question+urgency+ask,
  searched across the repo's issues). Continuing: request id over thread+context+question+
  ask, searched across that issue's comments. Same mechanism, different search scope, so a
  retried follow-up posts once.

### 2. Markers

All dispatch metadata is an HTML comment: `<!-- dispatch:<kind>\n<yaml>\n-->`. GitHub and
the dashboard hide it unconditionally. Kinds:

```yaml
# thread (issue body) — one per thread
<!-- dispatch:thread
requestId: c9e7cf0d6c2786f9
urgency: blocking
origin: {host: omp, machine: m, cwd: /p, tmux: "dev4:4.2", pane: "%15",
         sessionId: 01a05ac6-…, sessionTitle: "pm: e2e submitter identity"}
ask:
  - askId: c9e7cf0d6c2786f9        # opening ask reuses requestId
    question: What value?
    header: E2E_SUBMITTER_EMAIL
    options: [...]
-->

# ask (comment) — a follow-up question
<!-- dispatch:ask
requestId: 7b1e…                    # dedupe id of this follow-up
origin: {...}                       # re-stamped at post time
ask:
  - askId: 7b1e…
    question: ...
-->

# answer (comment) — written by the dashboard
<!-- dispatch:answer
forThread: 17158
forAsk: 7b1e…
answers: [[...]]
-->

# urgency (comment) — unchanged shape, new encoding
<!-- dispatch:urgency
urgency: high
-->
```

The human-readable summary the dashboard already writes below the answer marker is kept;
it is the only thing a GitHub reader sees.

**Compatibility:** parsers (Go `ParseMetaMarker`, SPA `markers.ts`) accept both the new
comment form and legacy front matter. An answer with no `forAsk` answers the thread's
opening ask — the only ask a legacy thread can have. Writers emit only the new form.

**Authenticity filter:** the dashboard's discovery keeps requiring a parsable thread marker
in the body; a plain issue with the `dispatch-thread` label is still not a thread.

### 3. Session identity

Each plugin fills `origin` from its host at call time — no environment variables, no
process boundary:

- **pi-envoy:** `sessionManager.getSessionId()` / `getSessionName()`, read inside the tool
  handler on every call, so a follow-up from a handed-off or renamed session carries the
  current values. `host: omp` is asserted by the plugin, not detected.
- **envoy-plugin (OpenCode):** the session id the plugin already uses for registration;
  title if the OpenCode API exposes one, otherwise omitted. `host: opencode`.
- **claude-envoy-bridge:** Claude Code has no native tool API, so `dispatch` is a tool of the
  plugin's existing `envoy` MCP server (`src/envoy-mcp-server.ts`), which already runs with
  `CLAUDE_CODE_SESSION_ID`; no title is available. `host: claude`.

Cwd, machine, tmux target, and pane come from the shared `resolveOrigin` in `envoy-client`
as today. Anything the host cannot supply is omitted, never invented. `OMP_SESSION_ID` is no
longer read anywhere in Envoy.

### 4. Dashboard

- **Open asks.** A thread's asks are collected from the body and every `dispatch:ask`
  comment; an ask is open when no `dispatch:answer` names its `askId`. The sidebar marks a
  thread "needs you" while any ask is open. The detail view renders a form for each open ask
  (usually one) and, in the conversation, each answer directly beneath the question it
  answered.
- **Origin line** shows `sessionTitle` as the label, `sessionId` in monospace with copy, and
  the existing tmux target with its `tmux switch-client -t %N` copy button.
- **Unfurling.** After markdown rendering, `#N`, `owner/repo#N`, and GitHub issue/PR URLs
  become links whose text is the referenced title, fetched through the existing GitHub proxy
  and cached per page load. Bare `#N` resolves against the thread's repo (GitHub's own rule).
  A failed fetch leaves the plain link.
- **No teardown on events.** Today every GitHub event on any covered repo — including the
  dashboard's own answer arriving back — runs `paint()`, which rebuilds the whole page from
  `controller.render()` via `innerHTML`, destroying the reply textarea and anything typed
  in it. `paint()` stops rebuilding the detail pane: the sidebar, conversation list, ask
  state, and header patch in place; the reply form and any open ask form are created when
  a thread is selected and are never re-created by an event.

### 5. Skill (`skills/dispatch/SKILL.md`)

New section, **Writing for the reader**, with rules the tool enforces where it can:

- The reader has not seen your transcript. No nouns you coined this session; no internal
  identifiers (eval-set ids, lane names, hashes) unless the question is about them. GitHub
  references may be bare — the dashboard unfurls them.
- Structure over paragraphs: `context` is at most three short paragraphs or a bullet list,
  one idea each; `question` is current state → desired state → options with one-line
  tradeoffs → recommendation, as a list.
- Options are buttons. If you are offering choices, put them in `ask`; never enumerate them
  in prose.
- Length caps (1200 / 800) and what to do when you hit them: split the question, or move
  options into `ask`.
- A before/after rewrite of a real wall-of-text thread as the worked example.

New section, **Continuing a thread**: when the reply changes the question, re-ask on the same
thread with `thread: N`; a genuinely new decision is a new thread; how to read a challenge
as an answer. The marker format from §2 is documented as the manual fallback.

### 6. Deletions and rename

Deleted: `envoy-client/src/dispatch-mcp-shim.ts` and its tests; `bin/dispatch-mcp-shim.ts`
in pi-envoy and claude-envoy-bridge; the `dispatch` entry in each plugin's `.mcp.json`; the
shim build step in pi-envoy's `prepack.sh`; `envoy-plugin/src/dispatch-mcp.ts`. Renamed:
`dispatch-mcp-bridge.ts` → `dispatch-client.ts` (it is the HTTP client to the service, not a
bridge between protocols). The 2026-09-04 doc's "Shim" section is retitled and points here.

### 7. Protocol note

Each tool call is one stateless HTTP request to the service (`StreamableHTTPOptions{
Stateless: true}`, bearer read from that call's headers — PR #782). No component holds a
long-lived MCP session to the service. Recorded here so it is not reintroduced.

## Error handling

| Condition | Behaviour |
|---|---|
| `thread` not a dispatch thread / closed | Tool error naming the issue and the fix |
| Call mixes opening and continuing arguments | Schema rejection, as for any invalid call |
| `context`/`question` over cap | Tool error naming field, length, limit |
| Host cannot supply a session id or title | Fields omitted; dispatch proceeds |
| Unfurl fetch fails | Plain link stays |
| Marker in a comment fails to parse | Comment renders as prose; never blocks the thread |
| Answer posted for an `askId` not on the thread | Rejected client-side before posting |

## Testing

- **Go:** follow-up posts a comment with a `dispatch:ask` marker and creates no issue;
  refuses non-thread and closed targets; mixed-mode calls fail schema validation; dedupe over
  comments; caps; both marker encodings parse; legacy answer maps to opening ask.
- **envoy-client:** `dispatch-client` sends one stateless request per call with the caller's
  bearer; `resolveOrigin` no longer reads `OMP_SESSION_ID`; no reference to the deleted MCP
  server remains.
- **pi-envoy / envoy-plugin / claude-envoy-bridge:** the registered `dispatch` tool fills
  `origin.sessionId` / `sessionTitle` from the host and asserts its `host`; a renamed or
  handed-off session's next call carries the new values; a host without a title omits it.
- **SPA:** multi-ask thread renders open ask as form and answered ask as history; legacy
  front-matter thread still discovered; unfurl resolves bare `#N` against the thread repo;
  a synthetic SSE event during typing leaves the textarea's value and selection intact.
- **End to end on the devbox:** a real OMP session opens a thread; answer on the dashboard
  arrives as a steer; the agent posts a follow-up on the same thread; the dashboard shows the
  new buttons, the session title, and an unfurled PR reference; second answer arrives as a
  steer; text typed in the reply box while an event arrives is still there; nothing on GitHub
  shows YAML.

## Rollout

Service and dashboard ship in the envoy image (one PR, `main` → published sha → compose
pin). `pi-legion-envoy`, `opencode-legion-envoy`, and the Claude plugin each release with
the native tool, the deleted `.mcp.json` entry, and the skill; bump in dotfiles. Old plugins
against the new service keep working (no `thread`, front matter still parsed). New plugins
against the old service fail loudly on `thread` (unknown argument) — deploy the service
first.
