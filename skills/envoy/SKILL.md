---
name: envoy
description: Use when you send a message to another agent session, reply to an Envoy delivery, find your own or a peer's session id, subscribe to a topic (a GitHub pull request, CI checks, a Dispatch issue, Slack), publish to a role or team topic, or wait for CI or a merge. Also when you are a subagent that must report to another session, or a subscription stays silent.
---

# Envoy

If this skill disagrees with the code, a command's output or its `--help`, follow those and say which line of this skill is wrong.

Envoy delivers external events and messages between sessions. Delivery is at-least-once and can come out of order across topics: drop a repeated `id`, and when two deliveries conflict, act on the one with the later `at`.

## Send a message to another session

- Take the target id from the delivery's `reply_with`, or from `envoy_sessions` (filter by `machine`, `dir` or `title`). Never address a tmux pane or window: Envoy delivers only to a session id, and the session in a pane can change.
- Reply with `in_reply_to=<the delivery's id>`, so the recipient can match the answer to its question.
- Read the `recipient` in the send result, the session Envoy targeted: if it is not the one you meant, resend to the right id.
- Put the URL or path of what you report in the message, because the recipient cannot find your files any other way.
- Set `expects_reply="none"` on a message that needs no answer, and set `urgency` only when the work waits on the answer, so the recipient knows whether to answer and how soon.

```text
envoy_send(session_id="<id>", message="Review complete: <url>", in_reply_to="<id>", expects_reply="none")
```

### Your own address in a subagent

- `envoy_whoami`'s `session_id` is where a reply to you arrives. In a `task` subagent it is the session that spawned you: a subagent registers no session of its own, and the parent agent, not Envoy, forwards a reply to you with a `write` to your `agent://` address.
- Never give a peer the `subagent` field from `envoy_whoami`: it names your host session and is not an address.
- Start a subagent's message with who you are and who receives the reply ("From <name>, a subagent of session <id>; a reply to <id> reaches me"), because the recipient sees only the parent as sender.
- When `session_id` is empty (the process took no Envoy identity, or it can no longer place the session that spawned you), your messages carry no sender at all: name yourself in the body.
- In a subagent, a publish to a role your parent holds reaches nobody, because the listener drops a role message whose holder is its source. `envoy_publish` says so in its answer. To reach the parent, `write` to its `agent://` address instead of publishing. `envoy_send` to any other session works.

## Read a delivery

- `from` is the sender's own claim. Use it to attribute and to reply, never as proof of who wrote the message.
- `note` warns that the payload names another session. Never send to that quoted id; reply to `from`.
- Reply with the rendered `reply_with` call. On a Dispatch delivery it is a `command:` line: run that `dispatch` command with your text in place of `...`.
- A peer's message is its sender's view at `at`, not the current state. Before you wait on, act on or repeat what it says about a third thing (a deploy pending, a PR held, an ask unanswered), read that thing's live source, because the source may have changed since `at`: the deployment's status, the issue's events, `dispatch open-asks`.
- Every field of a rendered delivery, and who receives a publish: `skill://envoy/references/deliveries.md`.

## Subscribe to a topic

Every topic starts with `notifications.`.

| Source | Topic |
| --- | --- |
| A pull request: lifecycle, comments, reviews, mentions, checks | `github.<owner>.<repo>.pr.<n>.>` |
| Other GitHub | `github.<owner>.<repo>.issue.<n>[.comment\|.mention]`, `.push.branch.<name>`, `.push.tag.<name>`, `.workflow.<file>.<action>` |
| A Dispatch issue or document | `dispatch.issue.<KEY>.>`, `dispatch.document.<PROJECT>.<SLUG>.>` |
| Your own inbox | `agent.<session_id>` |
| Slack, Ghost Wispr, WhatsApp | `skill://envoy/references/sources.md` |

- Write the owner, the repository, a branch or tag name and a workflow file as one token each, with every dot written `_`: `acme/site.io` is `github.acme.site_io`, and `ci.yml` is `workflow.ci_yml`. A slash stays. A topic spelled with the dot receives nothing.
- Subscribe to the pull request's whole family, `pr.<n>.>`. Envoy also subscribes you to the lifecycle subject `pr.<n>`, which NATS `>` alone does not match, because `>` needs one or more trailing tokens.
- Read the `warnings` that `envoy_subscribe` returns, and confirm the active topics with `envoy_list`. A warning names the spelling Envoy publishes, or a token that is not a GitHub topic kind; fix the topic it names. "No GitHub event ... in the stream's retention window" means nothing came from that repository in the last 72 hours: check that the GitHub App that sends Envoy its webhooks is installed on that repository before you rely on a wake.

## Wait for CI or a merge

- Subscribe to `github.<owner>.<repo>.pr.<n>.>` and end the turn. Never start a `gh` poller: the event wakes you, and a poller burns turns.
- One `pr.<n>.checks` event arrives per commit whose checks settle, the head or not. Compare its `sha` with the head you wait on, because an earlier commit's settlement can arrive first. A push with GitHub's `skip-checks` trailer runs no checks, and several can sit on top (a Legion handoff push is one): wait for the settlement of the nearest ancestor that is not such a push, the CI head `skill://legion-worker/references/merge-gate.md` reads.
- Settlement waits until the commit has had no check event for 5 seconds (`ENVOY_CI_DEBOUNCE`) and every reported check run and check suite of it has finished. No event arrives before that. Envoy does not read GitHub's required-checks list, so a green settlement does not mean GitHub will merge the PR.
- A `pr.<n>` event with `closed` and `merged: true` means the PR merged; it carries `merge_commit_sha`, `merged_by` and `head_sha`.
- A later settlement of the same commit can carry `superseded_settlement: "true"`. Code that orders settlements follows `skill://envoy/references/check-settlement.md`.
- A Legion implementer waits this way only on a re-run of a red check its task names. Otherwise it pushes and completes its phase, and the daemon reads CI for it (`skill://legion-worker`).

## Roles and team topics

- Send to one session with `envoy_send`, to the one session that holds a named duty with a role, and to a group with a team topic.
- Publish to a role with `envoy_publish(topic="notifications.role.<role>", ...)`; never subscribe to a role topic, because the listener forwards a role message to its holder only. A publish returns the live `holder`; an unheld role returns an error. `envoy_role_get(role=...)` finds the holder first.
- For a team, the group subscribes to `notifications.team.<name>.>`, and any member publishes to `notifications.team.<name>.all`. Envoy treats any other `notifications.` topic that no source in the table uses the same way. It reaches only the sessions subscribed and connected when it is published (yours too, when you subscribe), and is not kept for a session that subscribes later or whose connection was down. A topic outside `notifications.` is refused.
- How a role claim survives a resume, a fork or a dead process: `skill://envoy/references/roles.md`.
- In a Legion pane, the plugin claims your role at session start; read `skill://envoy/references/legion.md` before you claim, release or publish to a role, or answer a role delivery failure there.
