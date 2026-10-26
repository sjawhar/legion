---
title: Dispatch in Claude
description: Install the dispatch-web skill so Claude, in Claude in Chrome or on claude.ai, can walk you through Dispatch's web app.
sidebar:
  order: 8
  label: Claude skill
---

Claude in Chrome can read a Dispatch page and act on it, but it does not know Dispatch. Ask it how
to comment on a spec and it has to guess at the controls. The `dispatch-web` skill gives Claude
this guide: with it installed, Claude answers with Dispatch's own button names and shortcuts, and
asks you before it does anything other people will see.

In Claude in Chrome, Claude also reads the page you are on. Elsewhere on claude.ai it cannot see
Dispatch, so tell it which page you are looking at.

The skill is for people using the web app. An agent that writes to Dispatch uses the `dispatch`
command and its own skill instead; [Dispatch for agents](/legion/dispatch/for-agents/) covers that.

## What it holds

- `SKILL.md`, which tells Claude when to use the skill, how to answer, and which page covers each
  task.
- `references/`, these pages of the guide: [Introduction](/legion/dispatch/),
  [The Inbox and asks](/legion/dispatch/inbox-and-asks/),
  [Issues and projects](/legion/dispatch/issues-and-projects/),
  [Documents](/legion/dispatch/documents/), [Agents](/legion/dispatch/agents/),
  [Keyboard shortcuts](/legion/dispatch/keyboard/), [Delivery](/legion/dispatch/delivery/) and
  [Dispatch for agents](/legion/dispatch/for-agents/).

Every build of this site makes the zip from those pages, so it says what they say.
[The dispatch-web skill](/legion/dispatch/reference/web-skill/) lists its files and shows its
`SKILL.md`.

## Install it

Skills need code execution. On a Free, Pro or Max plan, check that **Code execution and file
creation** is on under **Settings**, then **Capabilities**. On a Team or Enterprise plan, an owner
turns on code execution and **Skills** under **Organization settings**, then **Plugins & skills**.

1. Download [dispatch-web.zip](/legion/downloads/dispatch-web.zip). Keep it zipped.
2. At [claude.ai](https://claude.ai), open **Customize**, then **Skills**.
3. Choose **+**, then **Create skill**, then **Upload a skill**, and pick the zip. On an older
   claude.ai, the same upload is under **Settings**, then **Capabilities**, then **Skills**, as
   **Upload skill**.
4. Check that `dispatch-web` is switched on in the skills list.

When Claude in Chrome's side panel runs as a Cowork session, your skills work there as they do on
claude.ai, so nothing else needs installing. That is the case on Max and Team plans, on Enterprise
once an admin turns Cowork on, and on Pro as it rolls out; Anthropic's
[Claude in Chrome guide](https://support.claude.com/en/articles/12012173-get-started-with-claude-in-chrome)
says which applies to you. Then open a Dispatch page and ask, for example, "How do I comment on
this spec?"

## Updating it

The zip changes when this guide does. To pick up a newer guide, delete `dispatch-web` from the
skills list and upload a fresh download. `references/SOURCE.md` in the zip names the commit it was
built from.

## Building it yourself

The skill's source is `docs/site/claude-skills/dispatch-web/SKILL.md`, and
`docs/site/generators/dispatch-web-skill.ts` builds it. The build fails when `SKILL.md` cites a
heading these pages lack, or names a control or key that no page shows in bold or as code. From
the repository root, this writes the zip to `docs/site/public/downloads/dispatch-web.zip`, along
with its reference page, without building the rest of the site:

```sh
bun docs/site/generators/dispatch-web-skill.ts docs/site/src/content/docs
```
