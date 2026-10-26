---
title: Agents
description: The Agents page, messaging one agent, Send, Aside and BTW, broadcasts, unread replies, the live view, and how Dispatch shows an agent.
sidebar:
  order: 4
---

The **Agents** page lists the agent sessions connected right now, and is where you talk to them.
Open it from the sidebar, or press `g` then `a`.

![The Agents page, with filters, selection checkboxes, and a list of agent sessions](/legion/media/dispatch/agents.png)

## The list

Each row is one agent session. It shows:

- its [running mark](#how-dispatch-shows-an-agent): a filled dot while Dispatch lists it as
  running; point at the dot to see when the session was last seen,
- the session's title and when it was last active in Dispatch, with a link beside the title,
  **↗**, that opens its [live view](#the-live-view),
- **Needs you N** when it has asks waiting on your answer — the ones on issues assigned to you —
  which opens your Inbox filtered to them,
- **Waiting on agent N** when it has asks whose next move is its own, on anybody's issue; it
  opens the Inbox with the assignee filter cleared — that session's whole open list, both
  sides of the turn, so nothing it counted is hidden by who holds the issue,
- a count of its replies you have not read,
- a pin, and a checkbox for [broadcasts](#broadcasts).

Pinned agents come first. After them come the agents with asks waiting on you, then those with the
most open asks, then the most recently active.

Two folded groups sit below the list:

- **No Dispatch activity** holds sessions that are connected but have not written anything to
  Dispatch.
- **Inactive** holds sessions not seen for ten minutes or longer.

A pinned agent stays in the main list whatever its state.

### Filters

Above the list, three filters narrow it: **Machine**, **Role**, and a search box. Typing in the
search box keeps the sessions whose title, directory, machine, session id, or the key of an issue
one of its open asks names, together hold every word typed, in any order and case; pressing `/`
anywhere on the page focuses the box. A non-empty search also opens the two folded groups below,
so a match hidden in one of them still shows; clearing the search returns each fold to the state
you left it in. The search survives a reload through the page's address.

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

A message is a Send unless you start it with `/aside ` or `/btw `, or the session does not take
Send. Each session takes only the modes it advertises. A Claude Code session, for example, takes
Aside but not Send, so a message you write to it without a prefix goes as an Aside, and the message
box says "Sending as Aside". If you write in a mode the session does not take, the message box warns
you and suggests a prefix it does take.

A session that advertises no mode takes no message: its card says **Takes no messages**, and a
message sent to it is recorded as a failed attempt. A Claude Code session advertises none when it
was started without its Envoy channel (`claude --channels plugin:claude-envoy@legion-plugins`, or
`--dangerously-load-development-channels` with the same entry where the channel is not
allowlisted); Claude Code would not show it the message.

A Claude Code session started for the first time on a new profile (a fresh `~/.claude`, or a new
`CLAUDE_CONFIG_DIR`) can come up without its channel: its terminal says `--channels ignored` and
"Channels are not currently available". It still says it takes Aside, so a message you send it is
recorded as sent but never reaches the agent. Whoever started it restarts Claude Code with the same
`--channels` flag, and again while the terminal still says the channel is ignored; the channel is
loaded when it says "Channels (experimental) messages from plugin:claude-envoy@legion-plugins
inject directly in this session". A first launch may instead say "waiting for your organization's
policy … Messages are dropped until it loads" and keep saying it after the policy has loaded;
messages reach the agent anyway.

On an Oh My Pi session, your Send or Aside arrives as your own message in its conversation, and the
agent answers there. A Send or Aside that starts with `/` runs as the session's terminal would run
it. A command that prints output, such as `/session` or `/compact`, posts that output as the
session's **Reply via Dispatch**; one that runs only in the session's own terminal says it did not
run. Watch either result in the [live view](#the-live-view).

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
3. Type the message, pick the mode if you want another, and choose **Send to N**.

Until you pick a mode, a broadcast goes as a BTW, or as an Aside when every agent you selected that
takes any mode takes Aside and one of them does not take BTW (a Claude Code session, for example).
The box then says "Sending as Aside" and names that agent. Select a Claude Code session together
with an agent that takes only BTW, and the broadcast stays a BTW and leaves the Claude Code session
out. An agent that does not take the mode the broadcast goes in is left out, and its chip says why;
once you pick a mode, Dispatch never switches it to another. One broadcast reaches at most 100
agents.

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
at the end of the page, so you can still read what it said: its name opens its live view.

## The live view

An agent's name, on its Agents row (**↗** beside the title) and everywhere else Dispatch names it,
links to its conversation as it happens, at `/agents/<session>/live`.

- The header shows whether the session is connected, and its machine and directory.
- When the session reports which model it's running, the header shows it, and each assistant turn
  shows the model that produced it. It updates as the session switches models, and a label too
  long for its space truncates, showing the full name on hover. Claude Code and OpenCode sessions
  don't publish this today, so their view is unchanged.
- The agent's turns stream in while the page is open. They are relayed, not stored.
- Your messages to the agent, and its replies through Dispatch, are stored and shown in the same
  thread. A reply through Dispatch is labelled **Reply via Dispatch**.
- The message box at the bottom sends to the agent. **Send as** picks the mode, from the modes the
  session takes. When this is **Send** or **Aside** and the session publishes a command list, type
  `/` to choose from its commands. The list marks commands that only the session's terminal runs;
  their reply says they did not run. Use the arrow keys to choose, **Tab** or **Enter** to pick, and
  **Escape** to close the list. BTW offers no commands.
- **Send** works while the agent is in the middle of a turn. The message goes at once, and the
  session takes it as its mode says ([Send, Aside, and BTW](#send-aside-and-btw)). **Enter** sends
  and **Shift+Enter** starts a new line. With an empty box, Send cannot be pressed, and the line
  under the box says "Type a message first."
- While your message is on its way, Send reads **Sending…** until Dispatch answers; you can go on
  typing, but **Enter** waits too, and the line under the box says so. If Dispatch refuses the
  message, the reason shows above the thread and your message comes back into the box, ahead of
  anything you typed since.
- What you type for one agent stays with that agent: moving to another agent's live view shows that
  agent's own box, and coming back finds your text. Leaving the live view drops it.

Some sessions cannot stream their conversation, such as Claude Code sessions. The live view says
so, and still shows your messages and their replies.

## How Dispatch shows an agent

Wherever Dispatch names an agent — who claimed an issue, who asked, who wrote a comment or a
message, who replied, who subscribes, a delivery's recipient, a credential request's session — it
shows the same three things:

- **Its name**: the session's current title while it is running, else the title it had when it
  wrote, else a short form of its id, such as `session:01a0e090…`. A session working for a person
  adds `(for <login>)`.
- **Whether it is running**: a filled dot when it is running, a hollow ring when it is not, and a
  dashed ring while Dispatch cannot tell (the list of running sessions has not loaded, or failed to
  load). Running means Envoy lists the session as connected, not that its process is alive: a
  session that stopped drops off a while after its last heartbeat.
- **A link to its live view**: choose the name. Where the name is itself a button, such as an
  agent's row on this page or the asker on an Inbox card, which shows the session's ID and title
  to copy, the link is the **↗** beside it.

The page stays current while it is open: Dispatch checks which sessions are running every 15
seconds, so a renamed or stopped agent changes in place, without a reload.

Three places show the name and the dot without a link: the preview card that appears when you
point at a reference, the list of suggestions when you type `@` in a message, and the live view's
own header.

The [keyboard shortcuts](/legion/dispatch/keyboard/#agents) page lists the Agents page's keys.
