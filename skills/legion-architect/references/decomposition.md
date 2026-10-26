# Decomposing a tree

Part of `skill://legion-architect`, section 1. Read it before you read a child's handoff, create or
adopt children, split a change across repositories, or attach architecture components.

## Reading the tree

Read the root issue, its acceptance criteria, its existing children and their current handoffs.
Your issue record is the `legion` tool's `read_record`. A child's committed handoff is a plain file
on its issue branch: read `.legion/<child>/<phase>.json` from that issue's workspace, or from GitHub:

```bash
gh api repos/<owner>/<repo>/contents/.legion/<child>/<phase>.json?ref=legion/<child> --jq .content | base64 -d
```

## Adopting children

Put every adopted child into the first wave and release it with
`legion({ op: "release_children", issues: ["LEGION-41", "LEGION-42"] })`. The daemon runs nothing on
a child until it is released. Once it is released, the daemon moves it to `todo` and, while the
root's design gate is open, starts its phases itself. It starts no sub-architect, and you start no
worker.

## Creating children

```bash
dispatch issue --project <project> --parent <root issue> --title '<child outcome>' --spec-file - <<'EOF'
<acceptance criteria, scope, and context>
EOF
```

- `--project` is the issue key's prefix before `-<n>` (`LEGSMOKE-3` → `LEGSMOKE`).
- In a root session `$LEGION_ISSUE` is the root (it equals `$LEGION_TREE`): pass it as `--parent`.
  The daemon makes the sub-issue link from `--parent`.
- Group the returned keys into ordered waves: you release each wave by key with
  `release_children`, and a child is inert until then.
- Do not pass `--assignee`, because the default keeps the tree's questions in one Inbox: under the
  shared token a child inherits its parent's assignee (the human who answers the tree's asks), else
  the project's default assignee, which a person sets on the Settings page; under a personal token
  it goes to that token's owner (`dispatch whoami`). Set it only when a human told you a specific
  person owns that child.
- Write into each child's acceptance that its pull request updates
  `.dispatch/architecture/<id>.md` for each component whose code it changes, when the project has
  architecture components (below).

A role token's `<project>` is not the `--project` above: it is the daemon's own project. The daemon
encodes a token as `legion-<project>-<key>-<role>` with the key lower-cased, so project `acme`,
issue `LEGION-41`, role `architect` is `legion-acme-legion-41-architect`.

## Across repositories

A change across repositories gets one child per repository:
`dispatch issue --project <project> --parent <root> --repo <owner/name> …`. Choose the repository at
creation, because the daemon fixes it when it records the child.

Release a child that depends on another repository's change only after that change's pull request
merges, because the child's work must build on the merged change. Name the merged commit in its
task (`gh api repos/<owner>/<repo>/pulls/<n> --jq .merge_commit_sha`); for a repository that pins the
other one by commit (a lock file or dependency pin), the pin is that commit. Read the other
repositories with your gh's read-only access (`-R owner/repo`).

## Architecture components

A project's architecture source is the repository whose `.dispatch/architecture/` files Dispatch
imports as its components (`skill://dispatch`, "Architecture components").

- **The components exist:** attach the root before you decompose, so every child inherits it:
  `dispatch issue-update --issue <root> --components-json '{"mode":"explicit","ids":[…]}'`.
- **They do not exist yet (bootstrap):** in the root's first PR (the implement worker pushes it),
  write `.dispatch/architecture/<id>.md` files for the planned components — front matter `title`,
  `parent`, `depends_on`, `external`; no `paths` yet. The importer reads the source branch's head,
  so leave the root on `inherit` until that PR has merged: an attach before then answers
  `400 COMPONENTS_INPUT`, since the component does not exist. After the merge, run
  `dispatch architecture-sync --project <KEY>` and attach the root; children created before then
  inherit it.
- Give a child its own `--components-json` only when it changes a narrower set, and
  `{"mode":"none","reason":"…"}` when it changes no code or file a component describes.
