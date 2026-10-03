# Legion documentation site

The source of <https://sjawhar.github.io/legion/>: an [Astro Starlight](https://starlight.astro.build/)
site covering Legion, Dispatch and the Secrets Broker. `.github/workflows/docs.yaml` builds it on
every pull request that touches `docs/site/`, `packages/` or `skills/`, and publishes it to GitHub
Pages on every push to `main`.

## Run it

It is a root Bun workspace, so the repository's `bun install` installs it. Building also needs Go
(for the reference generators) and Node.js 22.12 or newer (for Astro). From the repository root:

```sh
bun install
bun run docs:dev     # generate the reference pages, then serve with live reload
bun run docs:build   # generate the reference pages, then build into docs/site/dist/
```

In this directory, `bun run lint` and `bun run typecheck` are the checks CI runs before the build.

## Links between pages

The site is served under `/legion/`, so every link to another page is root-absolute and includes
that base: a sibling page `[Documents](/legion/dispatch/documents/)`, the section's generated
reference `[Tools](/legion/dispatch/reference/tools/)`, the overview
`[How it fits together](/legion/how-it-fits/)`. `starlight-links-validator` fails the build on a
link that leads nowhere, on a missing heading, and on a relative link (`../documents/`): it either
refuses relative links (`errorOnRelativeLinks`, its default) or, with that off, never checks them.

## Layout

| Path | What it holds |
| --- | --- |
| `src/content/docs/` | The pages. Each section (`legion/`, `dispatch/`, `broker/`) builds its sidebar from its directory. |
| `src/content/docs/<section>/reference/` | Generated reference pages, rebuilt on every build and ignored by git. |
| `generators/` | The reference generators: `<section>-<name>.ts` or `.sh`, run by `scripts/generate.ts`. |
| `scripts/generate.ts` | Runs every generator before the build, under the contract it documents. |
| `scripts/build-binaries.sh` | Builds the Go binaries the generators run. The one place module paths are named. |
| `media/` | The scripts that make screenshots and narrated walkthroughs, and the walkthroughs' sources, with a README saying how to run and extend them. A section with a rig of its own keeps its scripts and sources in `media/<section>/` with a README of its own (`media/broker/`). Nothing here is published. |
| `public/media/` | Where those scripts' output goes, served at `/legion/media/`: the screenshots, which CI takes before every build (the broker's are committed), and the finished videos. |
| `astro.config.mjs` | Site URL and base, sidebar, and plugins (Mermaid, the link validator, sidebar labels). |

How to write a page, the generator contract and where media goes are on the published site's
[Contributing to these docs](https://sjawhar.github.io/legion/contributing/) page, whose source is
`src/content/docs/contributing.md`.
