---
title: Contributing to these docs
description: How pages are laid out, how to add one, how generated reference pages are built, and where media goes.
---

This site is an [Astro Starlight](https://starlight.astro.build/) project in `docs/site/` of the
[Legion repository](https://github.com/sjawhar/legion). Every push to `main` publishes it to GitHub
Pages, and every pull request that touches it builds it, so a broken page or link fails the pull
request before it can merge.

## Building it locally

From the repository root, with Bun, Go and Node.js 22.12 or newer installed:

```sh
bun install --cwd docs/site
bun run docs:dev     # generate the reference pages, then serve the site with live reload
bun run docs:build   # generate the reference pages, then build the site into docs/site/dist/
```

## Page layout

Pages live in `docs/site/src/content/docs/`, one Markdown (`.md`) or MDX (`.mdx`) file per page.
The file's path is its URL: `dispatch/inbox.md` is published at `/legion/dispatch/inbox/`.

| Path | What it holds |
| --- | --- |
| `index.mdx`, `how-it-fits.md` | The Overview: the landing page and how the components fit together. |
| `legion/` | Legion, the coordinator and its agents. |
| `dispatch/` | Dispatch, issues and the decisions people make there. |
| `broker/` | The Secrets Broker. |
| `<section>/reference/` | Generated reference pages. Never edit or commit these; see below. |

Every page starts with frontmatter:

```yaml
---
title: The Inbox # the page heading and its sidebar label
description: One sentence saying what the page covers. # used for search results and link previews
sidebar:
  order: 2 # optional: lower numbers sort first within the section
  label: Inbox # optional: a shorter sidebar label than the title
---
```

Write the body as if the title were already its first heading: start with `##` headings. A diagram
is a fenced code block whose language is `mermaid`; it renders in the page and follows the light or
dark theme.

## Adding a page to a section

Create the file in the section's directory. Each section's sidebar is built from its directory, so
the page appears there without any configuration; set `sidebar.order` to place it. A section's
`index.md` is its introduction and sorts first (`order: 0`). A subdirectory becomes a collapsible
group in the sidebar.

### Linking between pages

Every link to another page on this site is root-absolute and starts with the site's `/legion` base,
because the site is served under `/legion/` and nothing adds the base for you:

- a page linking a sibling in its section: `[Documents](/legion/dispatch/documents/)`;
- a page linking its section's generated reference: `[Tools](/legion/dispatch/reference/tools/)`;
- a section page linking the overview: `[How it fits together](/legion/how-it-fits/)`.

The build checks every internal link and fails on one that leads nowhere, and on a link to a heading
that does not exist (`/legion/how-it-fits/#no-such-heading`). It also fails on a relative link such
as `../documents/`: the link validator either refuses relative links (its default,
`errorOnRelativeLinks`) or, with that turned off, never checks them, so a broken one would ship.

## Generated reference pages

Reference pages that describe code, such as an HTTP API, a CLI or a tool list, are generated from
that code on every build and never committed, so they cannot drift from it. They come from
generators, and `bun run generate` (part of `dev` and `build`) runs them under this contract:

- **Name and place.** A generator is an executable file directly in `docs/site/generators/` named
  `<section>-<name>.ts` or `<section>-<name>.sh`, where `<section>` is one of the content
  directories above (`legion`, `dispatch`, `broker`). Subdirectories of `generators/` hold shared
  helpers and are never run. A file directly in `generators/` that breaks these rules fails the
  build.
- **Invocation.** It runs from the repository root as `<generator> <absolute content dir>`, where
  the content directory is `docs/site/src/content/docs`. A `.ts` generator runs through Bun.
- **Binaries.** Before any generator runs, `docs/site/scripts/build-binaries.sh` builds the Go
  binaries from the same commit and puts them first on the generator's `PATH`: `legion`,
  `envoy-dispatch`, `envoy-broker` and `agent-secrets`.
- **Output.** It writes its pages, with frontmatter like any other page, under
  `<content dir>/<section>/reference/`. That directory is emptied before the generators run and is
  ignored by git. The sidebar shows it as the section's **Reference** group.
- **Failure.** A generator that exits non-zero, or exits zero without writing a page, fails the
  build.

`generators/legion-skills.ts` is a small example: it lists every skill in `skills/` by the name and
description in its `SKILL.md`, as [Skills](/legion/legion/reference/skills/).

## Media

Screenshots and narrated walkthroughs are produced by scripts in `docs/site/media/`, and what they
produce is served from `docs/site/public/media/`. A file at `public/media/<path>` is published at
`/legion/media/<path>`, so a page embeds a screenshot as `![The Inbox](/legion/media/<path>.png)`.
Media shows example data only: this repository is public, so no real hostname, account, token or
private URL appears in a page, image or video.
