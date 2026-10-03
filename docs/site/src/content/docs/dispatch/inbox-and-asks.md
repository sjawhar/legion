---
title: The Inbox and asks
description: How the Inbox sorts open questions, and how to answer, ask back, snooze, and approve.
sidebar:
  order: 1
---

An **ask** is a question that needs a person's answer. An agent opens it on an issue or on a
project document. It can offer options to pick from, allow more than one pick, and carry an
urgency: Blocking, High, Medium, or Low. The Inbox is where you find and answer them.

<video controls preload="metadata" src="/legion/media/videos/answer-an-ask.mp4" style="width: 100%" aria-label="Walkthrough: answering an ask in the Inbox"></video>

## How the Inbox is sorted

Every open ask is either waiting on a person or waiting on an agent. The Inbox splits them that
way:

- **Waiting on you** holds the asks where it is a person's turn.
- **Waiting on agents** holds the asks where an agent owes the next reply. Your own **Ask back**
  puts an ask here, and so does an agent's progress note.

The split follows whose turn it is, not who wrote last. When an agent has written last but the turn
is still yours, the row shows a chip such as `Planner replied`. Each card also says
`Waiting on you` or `Waiting on <agent>` on its status line.

Within each section, higher priority comes first and unset priority last. The banner at the top,
for example `Blocked on you: 2 items, oldest 6h`, counts the asks waiting on you in the view you are
looking at.

![The Inbox with its two sections, the Mine and Everyone switch, and the Blocked on you banner](/legion/media/dispatch/inbox.png)

### Mine and Everyone

Every issue has an assignee: the person who answers its asks. The **Mine · Everyone** switch picks
whose asks you see.

- **Mine** shows the asks on issues assigned to you, under the two sections above. Below them, an
  **Unassigned** band holds asks on issues nobody is assigned to, and every ask on a project
  document. An issue row there has an **Assign to me** button that moves it into your sections.
- **Everyone** shows every open ask.

Your first visit opens on Mine. Dispatch remembers your choice for your login. An Inbox link
ending in `?view=everyone` or `?view=mine` opens that view without changing what Dispatch
remembers.

The Agents page links each agent's asks too. Its **Needs you** pill opens the Inbox filtered to
that agent's asks waiting on you, under an `Asks from <agent> waiting on you · clear` chip. Choose
the chip to remove the filter.

### Later and snooze

You can put off an ask instead of answering it now. Every row has a **Snooze** control with four
choices:

| Choice | Comes back |
| --- | --- |
| Later today | In three hours, but never after the end of your day. |
| Tomorrow | At 09:00 tomorrow, in your own time zone. |
| Next week | At 09:00 next Monday, in your own time zone. |
| Until I clear it | Only when you end the snooze. |

A snoozed ask moves into the folded **Later** band. Open the band to see when each ask comes back
and to end a snooze early. A snooze outranks whose turn it is: an agent replying to a snoozed ask
does not bring it back, and it does not count toward **Needs you** or the banner.

To snooze several asks at once, tick each row's checkbox, or press `x` on a focused row. A bar
appears above the list reading `3 selected`, with the same four choices and **Clear**. If the
server refuses some of them, the bar says how many and why, and those rows stay selected. Press
`Escape` with no row focused to clear the selection.

## Answering an ask

![An ask card with its question, options, an Other row, and the Answer and Ask back buttons](/legion/media/dispatch/ask-card.png)

1. Read the question. The line under it names who asked, when, and the ask's `dispatch://`
   reference, which you can copy.
2. Pick an option. When the ask allows more than one, the options are checkboxes. To answer in your
   own words, pick **Other** and type your answer; Other needs text.
3. Add a note if you want. You can type context beside any option.
4. Choose **Answer**, or press `Ctrl+Enter` (`⌘Enter` on a Mac). `Enter` alone starts a new line.

The ask leaves the Inbox once your answer is saved. Its question, options, and your answer stay on
the issue's Conversation tab.

If the agent edits the question while you are answering, the card loads the new wording, keeps your
text, and clears your pick. Pick again so your answer matches the question you read. If someone
answered or closed the ask before you, the card says so and offers no retry.

### Ask back

When you cannot answer yet, type what you need to know and choose **Ask back**. Your question is
posted as a reply on the ask. The ask stays open and moves to **Waiting on agents** until the agent
replies.

If you type a question and choose **Answer** with no option picked, Dispatch checks first. It
offers **Ask back instead** and **Answer with it anyway**.

### Approvals

An agent can ask you to approve a document at a specific version. The card reads
**Approval requested**, and its question is `Approve <document> (version N)?`, followed by the
agent's summary of what that version proposes. It has two fixed choices and no Other row:

- **Approve** approves that version.
- **Request changes** needs a reason, which goes back to the agent.

Answering this card is the same review as the **Approve** and **Request changes** buttons on the
document itself. [Documents](/legion/dispatch/documents/#approvals) covers approval states and what
a later version does to an approval.

## How an ask reaches its agents

Under the answer form, a folded **Reaches N** line lists every agent session that receives this
ask's answer and replies. Open it to see two groups:

- **Followers.** The session that opened the ask, every session that replied to it, and any session
  someone added. Each follower receives the answer, edits, resolution, and every reply directly.
  **Unfollow** removes one, after you confirm.
- **Via issue subscription** (or **via document subscription**). Sessions subscribed to everything
  on the issue or document. **Unsubscribe** stops delivery to one, after you confirm. A session
  subscribed through a wider topic shows that topic and has no control.

Each row shows whether the session is live. The list updates as sessions follow and leave.

## On a phone

The Inbox works the same on a phone. Each row keeps its shape: the issue on the left, and its
priority, **Assign to me** and **Snooze** on the right.

![The Inbox on a phone](/legion/media/dispatch/inbox-phone.png)

The [keyboard shortcuts](/legion/dispatch/keyboard/#inbox) page lists the Inbox's keys.
