---
title: Issues and projects
description: Projects, the List and the Board, priority, assignees, labels, filters, the issue page, and pins.
sidebar:
  order: 2
---

Work in Dispatch is a set of **issues**, grouped into **projects**. A project has a short key, such
as `CORE`, and its issues are numbered under it: `CORE-1`, `CORE-2`.

## Projects

The sidebar lists every project with its count of open asks. You create a project in
**Settings → Projects** with a key and a name, and an agent creates one with `dispatch project`.
A key that is already taken is refused, whoever asks.

A project page has up to three tabs:

- **Issues**: the project's issues, as a List or a Board.
- **Documents**: the project's documents that belong to no single issue. Pick or drop a file here to
  upload it.
- **Architecture**: the project's components and how much tracked work each has. This tab appears
  only when the project has an architecture source, which you add in **Settings → Architecture
  sources**.

Opening a project from the sidebar takes you to Architecture when the project has a source, and to
Issues otherwise. When an ask in the project is waiting on you — its next move a person's, on an
issue assigned to you — the header shows a **Blocked on you · N** pill that links to the Inbox.
It counts no credential request: a request belongs to no project.

A project's components can come from more than one repository. Each architecture source is one
repository and the branch whose `.dispatch/architecture/` folder describes components, and the
project's model is every source's components together. In **Settings → Architecture sources**,
**Add source** takes the project, the repository and the branch; saving a repository the project
already has changes its branch, and each source's row has its own **Refresh** and **Delete**.
Deleting a source removes its components and no other source's. A component id must not repeat
across a project's sources: an import that brings an id another source already has fails for that
source, naming the id and the repository that holds it.

The Architecture tab's header has a line for each source: its repository and branch, the commit
it shows, when it was last checked, and its own **Refresh**. When a source's import fails, its line
shows the error and the commit it is still showing; its components keep their last good version
and wear a hatched outline, as a stale source's do, and every other source is untouched. A
component's details name the repository it comes from.

## Statuses

Every issue has one of nine statuses, in this order:

Triage → Icebox → Backlog → Todo → In progress → Testing → Needs review → Retro → Done

An open issue's **Status** menu offers every status except Done. **Close issue** is the way into
Done, and a closed issue offers **Reopen**, which puts it back in Backlog.

A closed issue still takes comments and messages, from people and agents: a comment, a reply, a
reply on an ask, a comment on a quote of one of its documents, and a message to an agent all post,
and a mentioned agent is still told and can answer. Everything else waits for **Reopen**: a new
ask, an answer to an ask, a suggestion, a document edit or named version, an upload, a review or
approval request, a claim, and resolving, reopening, editing, accepting or rejecting a comment
already there. Its priority, labels and other fields wait for it too, except its place on the
Board and its components; a linked GitHub issue or pull request can be taken off it, not added.

Legion, when it works an issue, moves that issue's status itself.

## The List and the Board

The **List** and **Board** buttons switch between two views of the same issues. Press `v` to
toggle. Dispatch remembers your choice.

Both views use one order. Issues are grouped by status, and within a status they keep the order
people set on the Board. A card you drag to the top of its column is first in the List too.
Priority is shown as a badge and can be filtered on, but it does not change the order.

![The Board, with a column per status and cards showing priority, labels, and open asks](/legion/media/dispatch/board.png)

### Moving cards

The Board has a column per status. Drag a card to reorder it or to change its status:

- With a mouse, press and move the card.
- On a touch screen, hold the card briefly, then drag. A quick tap opens the issue, and a swipe
  scrolls.
- With the keyboard, focus a card and press `Shift+J` or `Shift+K` to move it down or up, and
  `Shift+L` or `Shift+H` to move it to the next or previous status.

Dropping a card in Done closes the issue. Moving it out of Done reopens it.

Icebox and Done start as narrow folded columns. They still take drops. **Show Icebox & Done**
opens them in full, and Dispatch remembers that choice.

If someone else moved cards while you were dragging, the move fails with "The board changed while
you were moving this card - refreshed, try again." The Board shows the fresh order.

### Lanes

**Show lanes**, beside **Show Icebox & Done**, splits the Board into horizontal bands, one per
priority: P0 down to P3, then a band for issues with no priority. Cards keep their rank order
within each band. The choice persists per signed-in user, not in the page address, so it survives
a reload.

Dragging a card across lanes sets its priority, in the same drop that a drag across columns sets
its status. Keyboard roving (`j`/`k`, the arrow keys) stays within the focused lane.

### Priority

Priority runs from P0, the highest, to P3. An issue with no priority shows a muted **Priority**
tag. The badge is a picker wherever it appears: the issue header, a Board card, a List row, and an
Inbox row. Choose it and pick a value. Closed issues cannot change priority.

### Assignee

The assignee is the person who answers the issue's asks, and whose Inbox lists them by default.
Change it from the issue header. The choices are **Unassigned** and every login allowed to sign in.
An agent that creates an issue without naming one gets the person its token acts for, else the
parent's assignee, else the project's default assignee: the person you name in the projects table
in **Settings → Projects**, who takes every issue the chain leaves without an owner. Only a person
sets a project's default; no agent command does.

### Labels

Choose **Labels** in the issue header to edit an issue's labels. The list offers every label used
in the project, and you can create a new one from the search box. Your changes save when the list
closes.

### Filters

Choose **Filters** above the List or the Board to open the filter strip. It holds:

- **Status** (List only), to keep issues in the statuses you pick.
- **Labels**, to keep issues that carry every label you pick.
- **Search**, to match issue keys and titles.
- **Needs you**, to keep issues with open asks.
- **Unread**, to keep issues with activity you have not seen.
- **Unclaimed**, to keep issues no agent is implementing.

Each active filter shows as a chip next to **Filters · N active**. Choose a chip to remove it.
Filters live in the page address, so a filtered view survives a reload and can be shared as a
link.

An issue an agent is implementing shows **Claimed by** and the agent's name. When that agent is no
longer running, the chip says **not running**, and the issue counts as unclaimed.

## Creating an issue

Press `c` anywhere. Pick the project, type a title of up to 1,000 characters, and optionally the
first line of the spec. If the title closely matches an existing issue in the project, Dispatch
shows that issue and offers **Create anyway**. A new issue opens as soon as it is created.

## The issue page

![An issue page: the header, the Spec tab with its document, and the margin](/legion/media/dispatch/issue-spec.png)

### The header

The header holds the issue's key, title, and pin, then its controls:

- **Status**, priority, and assignee.
- An approval chip once someone has asked for the spec to be approved.
- **Close issue**, or **Reopen** on a closed issue.
- Whose turn it is: `Waiting on you (N)` or `Waiting on agents (N)`, counting the issue's open
  asks.

A line of details follows: labels, subscribers, the parent issue, linked GitHub issues and pull
requests, and **Referenced by**. Choose the title, or press `e`, to edit it.

A title shows in full, wrapped at any width, rather than cut off after two lines. A list that
still clamps a title shows the whole of it on hover.

- **Parent** shows the parent issue or **None**. You can edit it to move the issue under another.
- **Subscribers: N** lists the agent sessions subscribed to the issue. **Unsubscribe** stops
  delivery to one and tells it so.
- **Referenced by (N)** lists every issue, document, ask, comment, and message that links here.
- **Components** appears when the project has an architecture source. It shows which parts of the
  architecture the issue belongs to, and lets you change them, picking from the components of
  every source of the project.

### The tabs

- **Spec** is the issue's main document. [Documents](/legion/dispatch/documents/) covers it.
- **Conversation** is the issue's conversations, one card each: every ask with its replies and
  its answer, every comment thread with its replies, every message thread with its replies. The
  conversation with the newest reply is at the top, so a thread rises when someone answers it,
  and the whole list is there however old a conversation is. **All**, **Open** and **Closed**
  filter it: an ask is Closed once it is answered, resolved or retracted, a comment thread once
  it is resolved or its suggestion is accepted or rejected, and a message thread is never
  closed. The filter opens on **All** every visit. A conversation holding something you have
  not read is marked **New**, and **Jump to latest** counts those and takes you to the top.
- **Children** lists the issue's child issues, each with its status, how much of its own tree is
  done, and its latest activity.
- **Artifacts** lists the issue's documents and files, with a count. Pick a file, or drop one on the
  tab, add an optional summary, and choose **Upload**. Uploading a file with the same name adds a
  new version.
- **Activity** is the issue's whole history, one line each, newest first: every comment, ask and
  message as it was posted, and everything else that happened to the issue — status moves,
  claims, document versions, uploads, subscriptions, changes to child issues. It has **Load
  older** at the bottom, a divider for each day, and a mark where your last read ends.

Press `t` and then `s`, `c`, `h`, `a`, or `v` to switch to Spec, Conversation, Children,
Artifacts, or Activity.

### Writing in the Conversation

The box at the foot of the Conversation posts a comment on the issue. Type `@` to mention an agent
role or a live agent session; the comment is then delivered to it. Start the comment with `/btw `
or `/aside ` to choose how a mentioned agent receives it. [Agents](/legion/dispatch/agents/#sending-to-one-agent)
explains the delivery modes.

Press `Ctrl+Enter` (`⌘Enter` on a Mac) to send. `Enter` alone starts a new line.

## Pins

Pins keep things you return to within reach.

- **An issue.** Choose the pin next to the title, or press `Shift+P`. Pinned issues appear under
  **Pinned** in the sidebar, with their open-ask counts.
- **A Conversation entry.** Every entry has a pin at its top right. Pinned entries appear in the
  margin's **Pinned** tab beside the document, each with **Unpin**.
- **An agent.** On the Agents page, a pinned agent stays at the top of the list.
