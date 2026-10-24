---
name: dispatch-web
description: How to use Legion Dispatch's web app, where people and coding agents track work together through issues, specs, asks, margin comments, approvals and the Agents page. Use this skill whenever the open page is Dispatch (its title ends in "· Dispatch", or its sidebar has Inbox, Agents and project links), or the user asks how to do something in Legion Dispatch, such as commenting on or suggesting changes to a spec, answering or asking back on an ask, approving a spec, uploading an artifact, writing in an issue's Conversation, or messaging agents with Send, Aside or BTW. On a Dispatch page, use it even when the user doesn't name Dispatch. Do not use it for Google Docs, Jira, Linear, GitHub, Redux's dispatch function, or other products called Dispatch.
---

# Using Legion Dispatch in the browser

Dispatch is a shared workspace where people and coding agents track work. Each piece of work is an
*issue* with a *spec* (its main document), *asks* (questions waiting on a person), comments,
uploaded files and documents (its artifacts), and a history. People work in the web app; agents
write through a `dispatch` command in their own shell, which a person in the browser does not
have. So answer a person with the web app's buttons and shortcuts, and mention the `dispatch`
command only when they ask how an agent does something.

## How to answer

1. Look at the page first, when you can see it. In Claude in Chrome you can read the user's tab:
   its URL and headings tell you which surface they are on, such as the Inbox, an issue's Spec tab,
   a project board or the Agents page. Elsewhere, ask which page they are on. The reference files
   describe the UI as documented. If the page shows something different, because the deployed
   version is older or newer than the guide, trust the page and say so.
2. Find the task below and read that reference section before giving steps. Use the exact button
   names and shortcuts the guide gives. People ask because they could not find the control, so
   "select text, then choose **Comment** in the bar that appears" helps more than "add a comment".
3. Ask before changing anything others see. Everything in Dispatch is shared, and agents may act on
   a change the moment it lands. Documents are live: there is no save button, so every keystroke
   typed into a spec reaches the team at once. Before anything that sends, saves, creates, changes,
   approves, resolves, moves, snoozes, unfollows, unsubscribes or uploads, such as typing into a
   document, posting a comment or message, answering an ask, approving a spec, accepting a
   suggestion, changing an issue's status, or creating an issue, say what you are about to do and
   wait for the user's go-ahead. Reading, searching, opening pages and filling a box without sending
   are fine.
4. Leave secrets to the person. Approving or denying a credential request, and making an agent
   token, are theirs to do: show them where the control is. A new token is shown once; never read
   it out, copy it or paste it anywhere.

## Where each task is documented

| The user wants to | Read |
| --- | --- |
| Get oriented: what the Inbox, Projects, Agents, Delivery and Settings pages are for | `references/index.md`, "The main surfaces" |
| Comment on a spec or other document, reply in or resolve a thread | `references/documents.md`, "Comments in the margin" |
| Propose replacement text, or accept or reject someone's suggestion | `references/documents.md`, "Suggestions" |
| Ask a question about a passage (the **Ask** action on a selection) | `references/documents.md`, "Comments in the margin" |
| Understand or answer a decision block inside a spec | `references/documents.md`, "Decision blocks" |
| Edit a document's text, or see and compare earlier versions | `references/documents.md`, "Editing" and "Versions" |
| Approve a spec, or request changes to it | `references/documents.md`, "Approvals"; `references/inbox-and-asks.md`, "Approvals" |
| Answer an ask, change an answer, or ask back when the question is unclear | `references/inbox-and-asks.md`, "Answering an ask", "Changing your answer" and "Ask back" |
| Sort, filter or snooze the Inbox; find asks waiting on agents | `references/inbox-and-asks.md`, "How the Inbox is sorted", "Mine and Everyone" and "Later and snooze" |
| Find the credential requests waiting on them | `references/inbox-and-asks.md`, "How the Inbox is sorted" |
| See which agents an ask or issue reaches, or stop one following it | `references/inbox-and-asks.md`, "How an ask reaches its agents"; `references/issues-and-projects.md`, "The header" |
| Post on an issue's Conversation, or mention an agent there | `references/issues-and-projects.md`, "Writing in the Conversation" |
| Upload a file to an issue, or to a project | `references/issues-and-projects.md`, "The tabs" and "Projects" |
| Create an issue; set status, priority, assignee or labels; use the List or Board; pin issues | `references/issues-and-projects.md` |
| Message one agent, broadcast to several, read replies, open an agent's live view | `references/agents.md` |
| Choose between Send, Aside and BTW | `references/agents.md`, "Send, Aside, and BTW" |
| Search, link with `dispatch://`, or see what references something | `references/documents.md`, "References" and "Search" |
| Find a keyboard shortcut, or open the `?` help or the `Ctrl+K` palette | `references/keyboard.md` |
| Read the Delivery timeline, the delivery measures, or the Pipeline page | `references/delivery.md` |
| Make a token so an agent can use Dispatch | `references/for-agents.md`, "Connecting an agent" |
| Explain what an agent will hear about, or what it can do | `references/for-agents.md`, "What reaches you" and "Ask, comment, suggest, or message" |

`references/for-agents.md` and `references/delivery.md`, "For agents", describe the agent's side:
the `dispatch` command and the HTTP API. Use them to explain what an agent sees or does, never as
steps for the person in the browser. The one exception is making a token: the first two steps of
"Connecting an agent" are the person's, in **Settings**.

## The answer to the most common question

To comment on a document, including an issue's spec, select the text you want to comment on. A bar
appears with **Comment**, **Suggest** and **Ask**. Choose **Comment**, or press `Ctrl+Alt+M`
(`⌘+⌥+M` on a Mac), type, and send with `Ctrl+Enter` (`⌘Enter`); `Enter` alone starts a new line.
The thread appears in the margin beside the highlighted text, and `Shift+M` hides and shows the
margin. An older version picked from **Version** is read-only, so switch back to the current one.
To say something about the issue as a whole rather than one passage, use the box at the foot of the
issue's **Conversation** tab instead.

## Things that trip people up

Each point below rests on the sections it cites.

- Where each kind of write lands. An ask goes to the assignee's Inbox, under **Waiting on you**; an
  ask on an issue nobody is assigned to shows in the **Unassigned** band of **Mine**. A comment on a
  document sits in its margin and also shows on the issue's **Conversation** tab. The box at the
  foot of the Conversation posts a comment on the issue, and an agent mentioned there with `@`
  receives it. A message sent from the Agents page goes straight to that agent, on no issue, unless
  the sender chooses **Choose issue**. Only asks, approval requests and credential requests put
  something in a person's Inbox, so a decision someone must make belongs in an ask.
  (`references/for-agents.md`, "Ask, comment, suggest, or message"; `references/agents.md`,
  "Sending to one agent"; `references/inbox-and-asks.md`, "How the Inbox is sorted" and "Mine and
  Everyone")
- Approvals belong to a version. **Approve** approves the latest version; when an earlier approval
  is out of date, the button names the latest one, such as **Approve v13**. A later change to the
  document, other than the approver's own answer to a decision block, leaves the approval marked
  `changed since`. (`references/documents.md`, "Approvals"; `references/inbox-and-asks.md`,
  "Approvals")
- Each agent session takes only some message modes. A message is a Send unless it starts with
  `/aside ` or `/btw `, and a Claude Code session, for example, takes Aside but not Send. When the
  message box warns that a session does not take the mode, use the prefix it suggests. A Claude Code
  session's live view cannot stream its conversation, but still shows your messages and its
  replies. (`references/agents.md`, "Send, Aside, and BTW" and "The live view")
- What an agent hears about. An agent receives the answer and replies on asks it follows, replies to
  comments it started, new comments on documents it wrote, and messages sent to it from the Agents
  page. A comment on an issue's Conversation reaches it when the comment mentions it. Anything else
  on an issue reaches it only when it subscribed to the issue. (`references/for-agents.md`, "What
  reaches you"; `references/issues-and-projects.md`, "Writing in the Conversation")
- Size limits. The server refuses an ask, comment, message or file over its limit rather than
  cutting it; the cited section lists each limit. Shorten or split a refused comment, or upload the
  long text as a file on the issue's **Artifacts** tab. (`references/for-agents.md`, "Ask, comment,
  suggest, or message")
- Whose name an agent writes under. An agent's posts show its name followed by `(for <login>)`,
  the person whose agent token it uses, and an issue it creates without naming an assignee goes to
  that person. (`references/for-agents.md`, "Connecting an agent")

The reference files are Dispatch's user guide; `references/SOURCE.md` names the commit they come
from and their license.
