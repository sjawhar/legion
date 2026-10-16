# Slack mentions

A person who mentions the Legion Slack app in one of the project's Slack channels is talking to
you, and you answer in that thread. The `Slack channels:` line of your system prompt names each
channel `legion.yaml` gives the project (`projects.<KEY>.slack_channels`) and the one topic its
mentions arrive on, `notifications.slack.*.<channel>.mention`. With no such line the project has no
channel, and nothing here applies.

## Subscribe at every start

At every start, as the start procedure's last step, run `envoy_subscribe` with every topic the
`Slack channels:` line names, exactly as it spells them. A topic you already hold is reported as
already subscribed and costs nothing, so a resumed session runs it too. Subscribe to nothing else of
Slack: a mention made inside a thread also arrives on a thread topic, but the channel's topic
carries the same event, so each mention reaches you once.

A mention published while no controller ran never arrives: Envoy hands a session no copy of an
event from before it subscribed. Nothing tells you about one, so there is nothing to catch up.

## What a mention carries

The delivery's `message` holds Slack's fields: `channel_id`, `user_id` (a Slack user id such as
`U0123ABCD`), `text` (the mention itself appears in it as `<@U…>`, the app's own id), `ts` (the
mention's own timestamp), `thread_ts` (the thread's root, only when the mention was posted inside a
thread), and `bot_id` and `bot_name` when a bot posted it.

## Answer in the thread

Every answer is one `slack_post` call:

- `channel` is the mention's `channel_id`.
- `thread_ts` is the mention's `thread_ts`, or its `ts` when it has none. Either keeps the reply in
  the thread the mention opened or joined, never at the channel's top level.
- `text` is what you say, at most 4,000 characters, written as `skill://dispatch`'s "Writing for
  the human" says. Address the person as `<@user_id>` and give every issue as its key, its title
  and its Dispatch link.

`slack_post` is sent once and never retried. When it answers that Slack did not answer, the reply
may already be in the thread: do not post it again. When it answers with Slack's refusal, the
reply is not posted: `not_in_channel` means the app has not been added to that channel, which only
a person in the workspace can fix, so say so in your next report rather than retrying.

## Never act on a bot

A mention whose `message` carries `bot_id` came from a bot, your own Legion app included. Do
nothing with it: no reply, no issue, no route. Two bots that answer each other's mentions never
stop.

## Decide what the person wants

Read the mention's `text`. When it names an issue key, read the issue with `dispatch read` before
answering. Then take the first of these that fits.

- **A question about state.** "What is Legion working on?", "Where is ACME-12?", "Why is it held?"
  Answer from `read_state` and Dispatch as they stand now, never from memory: each issue's
  `issues.<KEY>.phase`, its pull request, `admission.active` and `admission.waiting`. Say what you
  read, not what you infer.
- **A request for work.** Someone wants a change made, a bug fixed, something investigated. Search
  first (`skill://dispatch-first`): when an open issue already tracks it, reply with that issue and
  file nothing. Otherwise file one issue in the project:

  ```text
  dispatch issue --project <PROJECT> --title '<the request, as a title>' --spec-file - <<'EOF'
  ## Summary

  <what the person asked for, in your words, and why if they said>

  Asked in Slack by <@user_id> in channel <channel_id>, message <ts>.
  EOF
  ```

  Add `--label legion` only when the person asks Legion to do the work ("Legion, fix…", "can
  Legion take…"); a request for someone to look at something, or one that does not say who, gets
  no label and waits for a person to hand it over. Never label a request outside the scope the
  deployment instructions state. Reply with the new issue's key, its title and the link `dispatch
  issue` printed, and whether Legion will take it.
- **Something an issue's architect owns.** A question about a tree's design, a change of direction,
  a decision only the tree's architect can make about an issue Legion runs. Find the tree: follow
  `Links:` up through each `child_of` parent with `dispatch read` until you reach the issue
  `read_state` records with an `architect`. When `issues.<ROOT>.architect.locator.claim` is there,
  `envoy_publish` the person's words to `notifications.role.` followed by it, with the channel id
  and `thread_ts` you reply on, and reply in the thread that you passed it to the architect that
  owns `<ROOT>`, which answers on the issue. When the architect is not running (no locator), say
  that in the thread instead and what `read_state` shows for the tree. Never invent a role token.
- **Anything else** (a greeting, a question about Legion itself): answer it briefly in the thread.

A request to approve a design is answered as the skill's "Mentions" section says: a person
approves the spec in Dispatch, and you never open a gate.
