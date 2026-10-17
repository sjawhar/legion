# Slack mentions

A person who mentions the Legion Slack app in one of the project's Slack channels is talking to
you, and you answer in that thread. The `Slack channels:` line of your system prompt names each
channel `legion.yaml` gives the project (`projects.<KEY>.slack_channels`). Only the controller the
daemon launches has one: the Slack bot token is the Legion daemon service's, which the secrets
broker grants to that service's own pods alone. With no such line the project has no channel, and
nothing here applies.

## Your session subscribes for you

Your session hears each listed channel's mentions by itself, on
`notifications.slack.*.<channel>.mention`, for as long as it holds the controller role: a resumed or
relaunched session hears the channels its launch names and no others, and drops every Slack topic an
earlier session subscribed to by hand. Never `envoy_subscribe` a Slack topic yourself.

A delivery from Slack whose `channel_id` the line does not name is not yours: do nothing with it.

A mention published while no controller ran never arrives: Envoy hands a session no copy of an
event from before it subscribed. Nothing tells you about one, so there is nothing to catch up. When
the project's `slack_channels` change, the daemon relaunches you into this session with the new
line.

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
  the human" says. Address the author as `<@user_id>` (by `bot_name` when a bot posted the mention
  with no `user_id`) and give every issue as its key, its title and its Dispatch link exactly as
  `dispatch` printed it (the `Link:` line of `dispatch read`, the link on a `Created` line or a
  search hit): it points at Dispatch's dashboard, and `slack_post` refuses a reply linking an
  address inside the cluster. Never build a link yourself. Say only what Dispatch shows: an issue's
  assignee is the one `dispatch` names, never one you assume.

`slack_post` is sent once and never retried. When it answers that Slack did not answer, the reply
may already be in the thread: do not post it again. When it answers with Slack's refusal, the
reply is not posted: `not_in_channel` means the app has not been added to that channel, which only
a person in the workspace can fix, so say so in your next report rather than retrying. When it
answers that the secrets broker has not granted `LEGION_SLACK_BOT_TOKEN` (its request waits for
approval, or was denied), nothing was posted and no reply can be until the operator stores the
token with owner `legion-daemon` and tier `agent`: name the error in your next report.

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
  deployment instructions state. Reply with the new issue's key, its title, the link and assignee
  the `Created` line names (Dispatch assigns the issue to your token's owner when you name nobody),
  and whether Legion will take it.
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
