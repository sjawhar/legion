---
title: Agents
description: The Agents page, messaging one agent, Send, Aside and BTW, broadcasts, unread replies, and the live view.
sidebar:
  order: 4
---

The **Agents** page lists the agent sessions connected right now, and is where you talk to them.
Open it from the sidebar, or press `g` then `a`.

![The Agents page, with filters, selection checkboxes, and a list of agent sessions](/legion/media/dispatch/agents.png)

## The list

Each row is one agent session. It shows:

- a dot for how recently the session was seen: under two minutes, under ten, or longer,
- the session's title and when it was last active in Dispatch,
- **Needs you N** when it has asks waiting on your answer, which opens your Inbox filtered to
  them,
- **Waiting on agent N** when it has asks where it owes the next move,
- a count of its replies you have not read,
- **Open**, which opens the [live view](#the-live-view),
- a pin, and a checkbox for [broadcasts](#broadcasts).

Pinned agents come first. After them come the agents with asks waiting on you, then those with the
most open asks, then the most recently active.

Two folded groups sit below the list:

- **No Dispatch activity** holds sessions that are connected but have not written anything to
  Dispatch.
- **Inactive** holds sessions not seen for ten minutes or longer.

A pinned agent stays in the main list whatever its state.

### Filters

Above the list, three filters narrow it: **Machine**, **Role**, and **Directory contains**.

## Sending to one agent

Choose an agent's title to open its row. The row shows your recent exchanges with that agent and a
message box.

By default the message goes straight to the agent, on no issue. Choose **Choose issue** to file it
on an open issue instead; it is then posted on that issue as a comment that mentions the agent.

Press `Ctrl+Enter` (`⌘Enter` on a Mac) to send.

### Send, Aside, and BTW

How a message reaches the agent depends on its mode:

| Mode | What the agent does with it |
| --- | --- |
| **Send** | Takes it as if you had typed it into its terminal and pressed Enter. |
| **Aside** | Picks it up at its next step. |
| **BTW** | Answers it in a side turn, without adding it to its main conversation, and the answer is posted back as its reply. |

A message is a Send unless you start it with `/aside ` or `/btw `. Each session takes only the
modes it advertises. A Claude Code session, for example, takes Aside but not Send. If you write in a
mode the session does not take, the message box warns you and suggests a prefix it does take.

On an Oh My Pi session, your Send or Aside arrives as your own message in its conversation, and the
agent answers there. Watch the answer in the [live view](#the-live-view).

## Broadcasts

A broadcast sends one message to many agents.

<video controls preload="metadata" poster="/legion/media/videos/broadcast-and-replies.jpg" style="width: 100%" aria-label="Walkthrough: broadcasting to agents and reading their replies">
  <source src="/legion/media/videos/broadcast-and-replies.mp4" type="video/mp4">
  <track kind="captions" src="/legion/media/videos/broadcast-and-replies.vtt" srclang="en" label="English" default>
</video>

1. Tick the checkbox on each agent you want, or press `x` on a focused row. The checkbox above the
   list, **Select all matching agents**, ticks every agent the filters match, including those in
   the folded groups.
2. A broadcast box appears at the bottom of the page. Its heading counts the recipients, for
   example `Broadcast to 3 of 4 selected`, and each selected agent is a chip you can remove.
3. Pick the mode, type the message, and choose **Send to N**.

An agent that does not take the mode you picked is left out, and its chip says why. Dispatch never
switches it to another mode. One broadcast reaches at most 100 agents.

Your selection is what you ticked, not what the filters show. Narrowing the filters after you tick
does not drop anyone; the count beside the checkbox says how many selected agents are outside the
filter. **Clear selection** empties it.

Each send gets a row above the broadcast box: `Queued`, `Sending to N…`, `Sent to N agents`, or
`Could not send to N` with the reason. A failed row offers **Retry**, which sends the same request
again, and **Restore draft**, which puts the message and the selection back in the box.

![A broadcast page listing each recipient, its delivery state, and its reply](/legion/media/dispatch/broadcast.png)

After a send, Dispatch opens that broadcast's page. It lists every recipient, whether the message
was delivered, and each reply. A recipient whose delivery failed has a retry. The **Broadcasts**
link at the top of the Agents page lists earlier broadcasts.

## Unread replies

When an agent replies to a message you sent it, the reply counts as unread. The count shows on the
**Agents** entry in the sidebar and on the agent's row. Opening the row, or the agent's live view,
marks those replies read, on every device you are signed in on.

When an agent with unread replies disconnects, it moves to a **Replied, no longer connected** group
at the end of the page, so you can still read what it said.

## The live view

**Open** on an agent's row shows its conversation as it happens, at `/agents/<session>/live`.

- The header shows whether the session is connected, and its machine and directory.
- The agent's turns stream in while the page is open. They are relayed, not stored.
- Your messages to the agent, and its replies through Dispatch, are stored and shown in the same
  thread. A reply through Dispatch is labelled **Reply via Dispatch**.
- The message box at the bottom sends to the agent. **Send as** picks the mode, from the modes the
  session takes.

Some sessions cannot stream their conversation, such as Claude Code sessions. The live view says
so, and still shows your messages and their replies.

The [keyboard shortcuts](/legion/dispatch/keyboard/#agents) page lists the Agents page's keys.
