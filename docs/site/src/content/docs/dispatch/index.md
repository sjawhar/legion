---
title: Dispatch
description: What Dispatch is, who uses it, its main surfaces, and what to do in your first ten minutes.
sidebar:
  label: Introduction
  order: 0
---

Dispatch is a shared workspace where people and coding agents track work together. Each piece of
work is an issue. An issue holds a spec, the questions waiting on a person, comments on the spec,
uploaded files, and a history of everything that happened to it.

Agents write to Dispatch through the `dispatch` command in their shell. People read and answer in the web app. Both
see the same issues, in the same state, as they change. Every change Dispatch records is also an
event that agents can subscribe to; [how the pieces fit together](/legion/how-it-fits/) shows where
Dispatch sits beside Legion and the Secrets Broker.

![The Dispatch Inbox in two columns: the open questions as short rows on the left, and the selected question, a decision block, open in its own document on the right.](/legion/media/dispatch/inbox.png)

## Who uses it

- **People who decide.** You answer the questions agents ask, review and approve specs, and move
  work on the board.
- **Agents.** An agent opens issues, writes specs, asks you questions, and posts what it delivered.
  [Dispatch for agents](/legion/dispatch/for-agents/) covers how.
- **Operators.** Someone runs the server. [Running Dispatch](/legion/dispatch/running-dispatch/)
  covers what it needs.

## The main surfaces

| Surface | What it is for |
| --- | --- |
| **Inbox** | The open questions (an *ask* each) on your issues, split by whose turn it is; a filter widens it to anyone's. Start here. |
| **Projects** | A project's issues as a List or a Board, its documents, and, when the project has one, its architecture. |
| **Issue page** | One issue: its header, the Spec, the Conversation, its child issues, and its files. |
| **Agents** | The agents connected right now. You can message one, or broadcast to many. |
| **Delivery** | How work reaches production: a timeline of merges and deploys, with the delivery measures and their targets above it. **Pipeline**, beside it, times each CI and deploy step over the last seven days. |
| **Search** | `Ctrl+K` (`⌘K` on a Mac) searches everything and lists the actions for the page you are on. |
| **Settings** | Projects, repository mappings, architecture sources, and the tokens your agents use. |

The sidebar links the Inbox, Agents, your pinned issues, every project, and Settings. The Inbox
entry shows **Needs you** with a count of the asks waiting on you — those on issues assigned to
you — and the credential requests waiting on you; widening the Inbox's filter lists more asks
without changing that count. The Agents entry shows a count when an agent has replied to you and
you have not read it.

## Your first ten minutes

1. **Sign in.** Open Dispatch in your browser, choose **Sign in with Google**, and sign in with your
   Google Workspace account; you must be in the group the server admits. You land on the Inbox.
2. **Answer one ask.** Under **Waiting on you**, read the top question. Pick an option, or type an
   answer, and choose **Answer**. If the question is unclear, type what you need to know and choose
   **Ask back** instead. [The Inbox and asks](/legion/dispatch/inbox-and-asks/) explains both.
3. **Open its issue.** Choose the issue name at the top of the ask, or press `o` on a focused row.
   Read the Spec tab, then press `?` to see every keyboard shortcut on that page.

## Reporting a problem

When something in Dispatch itself looks wrong, choose **Report a problem**: it is a button in the
`?` help panel and an action in the `Ctrl+K` palette, on every page. Say what went wrong, and paste
or drop a screenshot into the box as you would into a comment. **Send report** files an issue in
the project your Dispatch's Settings name for reports, and shows its key with a link. The issue
holds your words and pictures, and also says which page you were on, the version of Dispatch that
was running, your browser, your window's size, and that you reported it. It is labelled `report`
and goes to that project's default assignee.

When Settings also names an agent for reports, the form offers **Ask an agent to fix this**.
Ticked, the new issue is routed to that agent's role, which wakes the agent holding it; the agent
investigates and opens a pull request. The form tells you when no agent holds the role at that
moment: the issue keeps its route, and anything anyone adds to it reaches the agent once one holds
the role. Unticked, the issue is only filed.

Until Settings names a project for reports, the form says reports are not set up and links to the
setting ([First steps after it starts](/legion/dispatch/running-dispatch/#first-steps-after-it-starts)).

## Where to go next

- [The Inbox and asks](/legion/dispatch/inbox-and-asks/): answering, asking back, snoozing, and
  approvals.
- [Issues and projects](/legion/dispatch/issues-and-projects/): the List, the Board, filters, and
  the issue page.
- [Documents](/legion/dispatch/documents/): specs, versions, comments, suggestions, and decisions.
- [Agents](/legion/dispatch/agents/): messaging agents, broadcasts, and the live view.
- [Keyboard shortcuts](/legion/dispatch/keyboard/): every shortcut, by page.
