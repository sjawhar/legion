# Answering targeted and direct messages

`skill://dispatch` sends you here when a Dispatch frame targets you as BTW, Aside or Steer, or a
human messages you directly from the Agents page.

## Targeted agent messages

A human — or any bearer caller over HTTP, such as a test rig — can target the issue message at a
live Envoy session or role as **BTW**, **Aside**, or **Steer** (the dashboard calls a steer
**Send**). The incoming Dispatch frame names
the issue and includes a `reply_with` hint (`{ tool, args }`, ready to issue on any host); reply on the same open issue with the existing
tool, never a new targeted send:

```ts
dispatch_message({
  issue: "CORE-1",
  in_reply_to: "<targeted-message-id>",
  body: "The requested answer.",
})
```

`in_reply_to` correlates the answer under the asker's message in its Conversation card. A BTW
delivery can post its answer automatically; use this call when the frame asks the primary agent to
reply. A human may reply to your message in turn — the follow-up arrives as a targeted frame whose
`in_reply_to` names your message and whose `reply_body` quotes it; answer it the same way,
`dispatch_message({ issue, in_reply_to: "<their reply id>", body })`, so the exchange reads as one
thread. `dispatch_message` itself never carries `target` or `delivery`: agent-to-agent traffic goes
through Envoy or the hub. A bearer that targets over HTTP names its own session in `actor`
(`{kind: "session", id}`), and the card shows that session as the author. `GET /api/v1/agents`
(any authenticated caller) lists live sessions with their capabilities (`aside`, `btw`, `steer`);
target only a session that advertises the mode you want. Sending to a session with no issue
(`POST /api/v1/agents/{session_id}/messages`) stays human-only.

## Answering a direct message

A human can also message you directly from the **Agents** page, with no issue at all. On Oh My
Pi, a person's **Send** or **Aside** arrives as their own user message, exactly as if they had
typed it at your terminal: your Envoy plugin takes it only once Dispatch accepts it as a person's
own fresh message to you, and injects the text Dispatch stored. Answer it in the conversation as
you would anything typed, with no `dispatch_message`; the Agents page shows your conversation
live, so they read your answer there.

Everything else still arrives as a Dispatch frame: a **BTW**, a broadcast, a direct message on a
host that takes no user turn from its plugin (Claude Code), and one Dispatch did not accept.
That frame names no issue and its `reply_with` hint carries none either; answer it with the
message's bare id in `in_reply_to`, alone:

```ts
dispatch_message({
  in_reply_to: "<the direct message's id>",
  body: "The requested answer.",
})
```

Leave `issue` out — there is no issue to post into, and naming one would file your answer on
unrelated work. Dispatch threads the reply under their message in the same conversation, and the
human sees it in your conversation on the Agents page, where it shows as an unread reply until
they read it. Every other message still names its issue, so keep the `issue`
the frame gave you whenever it gave you one; a `dispatch://KEY/message/<id>` reference names the
issue its message lives on, so that form is a reply on that issue, not a direct message.

Have more to say after you answered? Call it again with the same `in_reply_to` and the new text:
Dispatch threads that follow-up under your first reply, and the tool result names the reply it
follows. The same text again posts nothing, so a retry is safe. Your host may already have
answered a **BTW** automatically before you got here; a second call is then your follow-up to
that answer, so read the result before writing again.

Read the whole conversation back — their message and every reply, yours included — with the
message id alone; it has no issue:

```ts
dispatch_read({ message: "<the direct message's id, or any reply's>" })
```

