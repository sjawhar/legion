---
title: The Inbox and asks
description: How the Inbox lists your questions and threads, and how to answer, reply, ask back, snooze, and approve.
sidebar:
  order: 1
---

An **ask** is a question that needs a person's answer. An agent opens it on an issue or on a
project document. It can offer options to pick from, allow more than one pick, and carry an
urgency: Blocking, High, Medium, or Low.

The Inbox is where you find and answer them, and it holds your threads as well: a thread you
wrote in that somebody has written in since, and a message you sent an agent that it has
answered. One list, one bar over it, one place to look.

<video controls preload="metadata" poster="/legion/media/videos/answer-an-ask.jpg" style="width: 100%" aria-label="Walkthrough: answering an ask in the Inbox">
  <source src="/legion/media/videos/answer-an-ask.mp4" type="video/mp4">
  <track kind="captions" src="/legion/media/videos/answer-an-ask.vtt" srclang="en" label="English" default>
</video>

## The two columns

From a window 1280 px wide or wider, the Inbox page is two columns. The left column lists
everything waiting for you, each as a short row: what kind of row it is, what it is about, when
it was last written in, and the issue or document it belongs to. The right column holds the row
you selected, whole.

![The Dispatch Inbox in two columns: the list of open questions and unread threads on the left, grouped by priority, and the selected question, a decision block, open in its own document on the right.](/legion/media/dispatch/inbox.png)

Drag the line between the two columns to change how wide each one is. It is also a slider: give
it the keyboard focus and the arrow keys move it one step at a time, and double-click it to put
it back where it started. Dispatch keeps the width you leave it at, in this browser.

What the right column shows follows the row:

- A question an agent wrote as a decision block opens as its own document, at the section the
  block sits in. **Skip to the answer** brings the block itself into view.
- A request to approve a document opens as that document's latest version, with **Approve** and
  **Request changes** at the head of the column.
- A question anchored on a quote, and a question on no document, open as the card described in
  [Answering an ask](#answering-an-ask).
- A thread opens whole — every message in it, and the box to reply at the end. A thread anchored
  in a document opens inside that document, under the text it is anchored to.

The Inbox selects its first row when it opens. Click a row to select it, or press `j` or `k`.
The arrow keys do the same while the focus is in the list. The selected row is in the page's
address as `?item=<id>`, so a link or a reload keeps your place. A question the agent withdraws,
or that somebody answers elsewhere, leaves the list at once, and the next row is selected.

Three things stay in the column until you select another row: a question you had typed an answer
into, which is shown closed with your text to copy; a question whose answer you sent and the
server has not decided yet; and a thread you have just read, which is drawn as read where it
stands rather than disappearing under you.

Below 1280 px, and in the [Inbox drawer](#opening-the-inbox-as-a-drawer) at any width, there is
no second column. Each row holds its own card: a question with its controls, a thread with its
messages, **Open**, **Mark read** and **Dismiss**.

## How the list is ordered and grouped

The Inbox opens newest first, grouped by priority: one band per priority, P0 at the top, rows
whose issue has no priority last, and everything you snoozed in a folded **Later** band at the
end.

A band draws 50 rows. Where it holds more, **Show more** under it draws the next 50.

**View**, at the right end of the bar, changes all of that:

- **Sort by**: **Newest first**, the default, or Priority, Urgency, Type, Project, or Assignee.
  Whichever you pick, rows that match on it are newest first within it.
- **Group by**: the same five fields, plus **Waiting on** and **None**. **None** is one list with
  no headings. A band is drawn only when it holds a row, and a snoozed ask is in **Later**
  whatever you group by.
- **Show**: **Inbox**, the default, or **Dismissed**, the threads you have taken out of the
  Inbox. A thread stays in the Inbox, read or not, until you dismiss it.
- **Thread order**: whether a thread's replies read **Oldest first**, the default, or **Newest
  first**. This choice is yours everywhere: it sets the order of every thread in Dispatch, not
  only the ones in the Inbox.

The sort, the grouping and the **Show** switch are part of the page's address, so a link
carries the view you are looking at. The thread order, the column width and the bands you folded
are kept for your browser instead.

### Waiting on

**Group by → Waiting on** bands the list by whose turn it is:

- **Waiting on you** holds the asks where it is a person's turn, and every thread, since a thread
  is waiting for you to read it.
- **Waiting on agents** holds the asks where an agent owes the next reply. Your own **Ask back**
  puts an ask here, and so does an agent's progress note.

The split follows whose turn it is, not who wrote last. Where a row holds its own card, the row
shows a chip such as `Planner replied` when an agent wrote last and the turn is still yours.
Each card also says `Waiting on you` or `Waiting on <agent>` on its status line.

### Filter

**Filter**, at the left of the bar, narrows the list by the same five fields: Priority, Urgency,
Type, Project and Assignee. Each opens a list you can search, and every value in it carries how
many rows it holds, counted inside what the other four fields have already narrowed — so a value
that reads 3 has three rows behind it, and picking it never empties the list.

Type is the field that tells the kinds apart: Question, Decision, Approval, Reply and Agent
message.

Each value you pick becomes a chip under the bar. Remove one with its ×, or all of them with
**Clear filters**. The filters are in the address as well, so a narrowed list is a link.

The banner at the top, for example `Blocked on you: 2 items, oldest 6h`, counts the asks waiting
on you — the ones on issues assigned to you that you have not snoozed — and every credential
request waiting on you, whatever the filter below it is set to.

A credential request is an agent asking you to let it use a secret, or a machine asking to start
agent sessions as you. Pending ones are listed under **Credential requests**, above the list.
A **Secret request** links to its page on the Secrets Broker, where you approve or deny it, signed
in to the broker ([approve a request](/legion/broker/guides/approve-a-request/)); Dispatch itself
decides no secret request. A **Machine login** links to Dispatch's machine-login page, where you
type the code the machine shows and approve or deny it
([log a machine in](/legion/broker/guides/log-a-machine-in/)). They count toward the sidebar's
**Inbox** number and the banner like an ask that waits on you. The Inbox says `Nothing needs you`
only when your own list holds no ask and no credential request; with the filter on someone else it
says `Nothing matches this filter`, and with the filter cleared, `Nothing needs anyone`. An open
Inbox picks up a new request, or one that is no longer pending, within moments on its own - no
reload needed.

The list does not move under a moving pointer. If the Inbox refreshes while your pointer travels
across it, the rows stay put until the pointer rests, leaves the list, or you click.

### Whose asks you see

Every issue has an assignee: the person who answers its asks. The **Assignee** field inside
**Filter** picks whose asks the Inbox holds. Its button reads `Assignee · you` while it is on you, and
the list it opens offers **Unassigned** and everyone who has signed in to Dispatch. Pick as many
people as you like; the button then names the one you picked, or how many.

- The Inbox opens on your own asks every visit: the asks on the issues assigned to you. Dispatch
  remembers nothing between visits.
- Add **Unassigned** to see the asks nobody holds — those on issues with no assignee, and every
  ask on a project document.
- Clear the filter to see every open ask.

An ask whose issue nobody is assigned has an **Assign to me** button: at the head of the right
column, and on the ask's own row where there is no second column. Choosing it makes the issue
yours, so the ask is in your own list, and your counts, from then on. An ask on a project
document belongs to no issue and has no such button.

The filter is part of the page's address: one `?assignee=` value per person, each a login or the
word `unassigned`, so a link carries the list you are looking at. `?assignee=everyone` is the
cleared filter, and an address with no `assignee` is your own asks.

Beside the filter is a **Search asks** box. Type into it and the list keeps the asks that hold
every word you typed, wherever each word sits: in the issue key, in the issue's title, in the
question itself, or in who asked it — a person's login, and for an agent the session it wrote
from, the title that session started under and the person who runs it. An ask on a project
document, which belongs to no issue, matches the document's name too. Capitals make no
difference. The `f` key puts the focus in the box from anywhere on the page, and `Escape` there
puts it back on the selected row, or on the first row when no row is selected.

The box narrows the list the Inbox already holds, as the **Assignee** filter does: nothing is
asked of the server again, so the sections, their counts and the keys that walk the list all
follow what you typed. An ask the text hides drops out of the count of ticked asks and out of the
snooze that acts on them, and keeps its tick for when it comes back. The credential requests above
the list and the **Blocked on you** summary are not narrowed. When no ask holds every word, the
list reads **Nothing matches this text**.

The text is part of the address too, as `?q=`, so a link carries what you searched for as well as
whose asks you were looking at.

While the filter names anybody but you alone, the **Waiting on you** band reads **Waiting on a
person** instead, since those asks wait on whoever holds them.

The banner, a project's **Blocked on you** pill and the Agents page's **Needs you** pill mean
one thing wherever they appear: the open asks whose turn is a person's, on issues assigned to
you, that you have not snoozed. The banner adds the credential requests waiting on you, and so
does the sidebar's **Inbox** number, which adds your unread threads on top of both; a project's
pill counts no credential request, because a request belongs to no project. None of them follows
the filter, so a widened filter lists more asks than the number beside the Inbox.

The Agents page links each agent's asks too. Its **Needs you** pill opens the Inbox filtered to
that agent's asks waiting on you, under an `Asks from <agent> waiting on you · clear` chip. Choose
the chip to remove the filter, or the **↗** beside it to open the agent's
[live view](/legion/dispatch/agents/#the-live-view).

### Later and snooze

You can put off an ask instead of answering it now. Every ask has a **Snooze** control with four
choices, at the head of the right column, or on the ask's own row where there is no second
column:

| Choice | Comes back |
| --- | --- |
| Later today | In three hours, but never after the end of your day. |
| Tomorrow | At 09:00 tomorrow, in your own time zone. |
| Next week | At 09:00 next Monday, in your own time zone. |
| Until I clear it | Only when you end the snooze. |

A snoozed ask moves into the folded **Later** band, which is there whatever you group the list
by. Open the band to see when each ask comes back and to end a snooze early. A snooze outranks
whose turn it is: an agent replying to a snoozed ask does not bring it back, and it does not
count toward the sidebar's **Inbox** number or the banner. A thread cannot be snoozed: it is
read or it is not.

To snooze several asks at once, tick each row's checkbox, or press `x` on the row the right
column is showing (on the focused row where there is no second column). A bar appears above the
list reading `3 selected`, with the same four choices and **Clear**. If the server refuses some
of them, the bar says how many and why, and those rows stay ticked. Press `Escape` with no row
focused to drop every tick.

## Replies and agent messages

Two kinds of row in the Inbox are threads rather than questions:

- A **Reply** row is a thread you wrote in — an ask, a comment thread, a message thread on an
  issue — that somebody else has written in since you last read it.
- An **Agent message** row is a one-off message you sent an agent session that the session has
  answered.

A thread row shows what the thread is about, who it belongs to, and when it was last written in,
with a dot while it holds something you have not read. Open the row and the thread is drawn
whole: every message, each one you have not read marked **New**, and the box to reply at the end.
A reply you write here goes exactly where one written on the thread's own page goes.

Reading a thread marks it read. The row stays where it is, drawn as read: a thread stays in your
Inbox, read or not, until you dismiss it, the way an email stays in its inbox until it is
archived. Where you would rather not read them one at a time, **Mark all read**, in the bar
between **Filter** and **View**, marks every thread the list is showing. It asks nothing first:
every row is redrawn as read and every count drops at once, and if the server refuses the press
the marks come back and the bar says `Nothing was marked read.` A filtered list marks only what
it is showing, and a thread somebody wrote in after the list was read stays unread.

**Dismiss** is what takes a thread out of the Inbox. It is on the row itself — while your pointer
is on the row, and always on the row you are reading — in the right column's head, and on the
card where there is no second column. The key `e` dismisses the row you are on, and with rows
selected the bar above them dismisses all of them at once. A question has no **Dismiss**: you
answer or snooze an ask instead.

What you dismissed is behind **View → Show → Dismissed**, newest dismissal first, where the same
control reads **Move to Inbox** and one press puts the thread back. Somebody else writing in a
thread you dismissed brings it back to the Inbox, unread, on its own; a reply you send from the
**Dismissed** view leaves the thread where it is.

The **Type** filter has a value for each kind, so **Reply** narrows the list to those threads
alone, which is also what the address `/replies` opens.

Where there is no second column each thread row is a card, with **Open**, which goes to the
thread's own page, **Mark read** and **Dismiss**.

## Answering an ask

An ask card holds the question and the answer. The Inbox draws it in the right column for a
question anchored on a quote and a question on no document, in the document itself for a
decision block, and in the ask's own row where there is no second column.

![An ask card in the Inbox: the question, its two options and Other, a note field, and Answer.](/legion/media/dispatch/ask-card.png)

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

### Changing your answer

An answer you gave stays yours to change. Its card shows **Change answer**, to you and nobody
else. Choose it and the form opens with your current pick and note filled in; change them and
choose **Save answer**, or **Cancel** to keep the answer you had. Approvals have no Change answer:
record a new review on the document instead.

The agent that asked receives the new answer as it received the first, with the answer it
replaces. Every earlier answer stays on the ask: the card reads `Changed <time>` and **Show 1
earlier answer** lists the earlier ones, oldest first. In a document decision, the decision block
is rewritten with the new answer.

If someone changed the answer after you opened the card, Dispatch keeps theirs and shows it, so
you can decide again from the current answer.

### Answered by you

**Answered by you**, beside the Inbox heading, opens a page of every answer you gave and every
reply you wrote on an ask, newest first. Each row shows when, the issue or document, the question,
and your answer or reply. **Open** takes you to the ask: an ask on a document opens that document
with the ask selected, and any other opens its turn in the issue's Conversation. An answer that is
no longer the ask's current one reads **Changed since**. Your current answers have **Change
answer**, which opens the ask's card under the row; after you save, the new answer heads the list
and the card stays open with both answers.

### Ask back

When you cannot answer yet, type what you need to know and choose **Ask back**. Your question is
posted as a reply on the ask. The ask stays open and waits on the agent until it replies.

If you type a question and choose **Answer** with no option picked, Dispatch checks first. It
offers **Ask back instead** and **Answer with it anyway**.

### Reply thread

An ask's replies read in the order you chose in **View → Thread order**: **Oldest first**, the
default, or **Newest first**. That order is the same for every thread in Dispatch — the Inbox,
the margin, a document's decision block, the Conversation tab — and the reply box sits next to
the newest message, so it is under the root in **Newest first** and at the end in **Oldest
first**.

The root and the two newest replies always show. Older ones fold behind **Show N earlier
replies**, and **Show fewer replies** folds them back; a reply you have not read never folds.
After an ask is answered, write a follow-up in **Reply**; in the Conversation tab and in document
decisions, choose **Write a reply** first. A reply you are still typing survives if its card
remounts elsewhere, such as switching margin tabs away and back.

### Approvals

An agent can ask you to approve a document at a specific version.

In the right column the Inbox opens the document's latest version, with **Approve** and
**Request changes** at the head of the column. Where you approved the document before, every
block that changed since the version you approved is marked, and **Next change** moves the
column to the next one.

Where there is no second column the request is a card. The card reads **Approval requested**,
and its question is `Approve <document> (version N)?`, followed by the agent's summary of what
that version proposes. The column and the card both offer two fixed choices and no Other row:

- **Approve** approves that version.
- **Request changes** needs a reason, which goes back to the agent.

The card also names the document and the version it asks about as a link (everywhere except the
document's own margin, where you're already on it): opening the ask from the Inbox, the drawer, or
its `dispatch://.../ask/<id>` reference takes you to that document with the approve controls in
view, instead of the issue's Conversation turn. If the document has moved past the version the
request named, the link still opens the current document, and the card keeps saying which version
the request was for.

When you approved the document before, the card says so under the question:
`Approved at version N on <date> by <who>.` with a **Show diff** control. Pressing it shows what
changed between the version you approved and the one you are asked to approve, with every
decision block left out of both, since the answers in those blocks are your own. A first request,
on a document nobody has approved, shows neither.

Answering the request is the same review as the **Approve** and **Request changes** buttons on the
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

Each row shows the session as Dispatch shows every agent: its name, which opens its live view, and
its [running mark](/legion/dispatch/agents/#how-dispatch-shows-an-agent). A session that reads
`unresponsive` — past its registration window, inside the gone horizon — still receives: the answer
is stored for it and arrives when it answers again, so do not unfollow it as ended. The list
updates as sessions follow and leave.

## Opening the Inbox as a drawer

You don't have to leave a page to answer an ask. The `i` key opens the Inbox as a drawer over
whatever page you're on, and so does a header button, which differs by screen width:

- **On a phone or a narrow window**, the sidebar collapses into a top header whose **Inbox**
  badge (or **Inbox N**, once something is waiting) opens the drawer instead of navigating.
- **On a wide screen**, the sidebar's **Inbox** link still navigates to the Inbox page and keeps
  its own count; a separate **Peek** button beside it, with no count of its own, opens the drawer
  instead.

The drawer holds no second column at any width: every row is a card, as it is on the narrow
Inbox page. The actions are the same: answer, ask back, snooze, read a thread or open a row
without losing your place. The page underneath keeps its state, a half-typed message included,
so a reply you were drafting is still there when you close the drawer.

The drawer carries the same **Filter** and **View**, with choices of its own. It opens on your
own work, and what you pick there changes the drawer alone: the address of the page underneath
never moves. The **Search asks** box is the drawer's own in the same way: the drawer keeps what
you type there, and it narrows the drawer alone. The two settings that are yours rather than the
list's — the thread order and the bands you folded — are shared with the page, since they
belong to your browser.

`Escape` closes the drawer before it closes anything else behind it, and returns focus to the
button that opened it. Clicking outside the drawer closes it too. The button isn't offered on the
Inbox page itself, since the page already shows everything the drawer would.

## On a phone

The Inbox works the same on a phone: the same bar, the same bands, and each row a card with its
own controls — a question with its options, **Assign to me** and **Snooze**, a thread with its
messages, **Open** and **Mark read**.

![The Inbox on a phone](/legion/media/dispatch/inbox-phone.png)

The [keyboard shortcuts](/legion/dispatch/keyboard/#inbox) page lists the Inbox's keys.
