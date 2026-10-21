# Answering targeted and direct messages

Read this when a Dispatch frame targets you as BTW, Aside or Steer, or a human messages you from the Agents page.

## Targeted agent messages

- A human (or a bearer over HTTP) can target an issue message at your session or role. The modes are **BTW**, **Aside** or **Steer** (the dashboard says **Send**). The frame names the issue and a `reply_with` hint.
- Reply on that issue with that command, never a new targeted send:

```bash
dispatch message --issue CORE-1 --in-reply-to <targeted-message-id> --body 'The requested answer.'
```

- A BTW can post its answer automatically. Reply yourself when the frame asks the primary agent to.
- A human's reply to you arrives as a frame with `in_reply_to` and `reply_body`. Answer it the same way, with their reply's id.
- `dispatch message` never carries `target` or `delivery`. Agent-to-agent traffic goes over Envoy or `agent://`.
- A bearer that targets over HTTP names its session in `actor` (`{kind: "session", id}`). Target only a session that `GET /api/v1/agents` lists with that mode. Sending to a session with no issue is human-only.

## Answering a direct message

- On Oh My Pi, an Agents-page **Send** or **Aside** arrives as a user message. Answer in the conversation, with no `dispatch message`.
- A text that starts with `/` runs as that command. Answer what it expands to as typed. A command that never reaches you (`/session`, `/compact`, `/new`) is answered by your plugin: write nothing. For `/force:<tool> <prompt>`, answer only the prompt.
- Everything else arrives as a Dispatch frame with no issue. That is a BTW, a broadcast, a direct message on Claude Code, or one Dispatch did not accept. Answer with the bare id and no `--issue`:

```bash
dispatch message --in-reply-to <the direct message's id> --body 'The requested answer.'
```

- Naming an issue would file your answer on unrelated work. Keep `--issue` whenever the frame gave one. A `dispatch://KEY/message/<id>` names its issue.
- More to say: run it again with the same `--in-reply-to`. The same text posts nothing, so a retry is safe. Your host may have answered a BTW already: read the output before you write again.
- Read the whole conversation back with `dispatch read --message <id>`.
