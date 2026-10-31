# Decomposing a tree

Part of `skill://legion-architect`, section 1. Read it before you read a child's handoff, create or
adopt children, split a change across repositories, or attach architecture components.

## Reading the tree

Read the root issue, its acceptance criteria, its existing children and their current handoffs.
Your issue record is the `legion` tool's `read_record`. A child's handoffs are records on its
Dispatch issue: read the newest of a phase with
`dispatch handoff-read --issue <child> --phase <word>` (`plan`, `implement`, `test` or `review`),
and an earlier one with `--version <n>`. The child's Conversation lists every version.

## Adopting children

Put every adopted child into the first wave and release it with
`legion({ op: "release_children", issues: ["LEGION-41", "LEGION-42"] })`. The daemon runs nothing on
a child until it is released. Once it is released, the daemon moves it to `todo` and, while the
root's design gate is open, starts its phases itself. It starts no sub-architect, and you start no
worker. A release is accepted whole whatever the `--blocked-by` links say: its answer names
`started` and `held` (each held child with the issues it waits on) beside `released`, and a held
child waits in your tree until Legion starts it.

## Creating children

```bash
dispatch issue --project <project> --parent <root issue> --title '<child outcome>' --spec-file - <<'EOF'
<the child's spec, written as skill://dispatch-writing-specs says, scaled to the child>
EOF
```

- Write each child's spec as `skill://dispatch-writing-specs` says, scaled to the child: its
  outline (`Summary` through `Acceptance`, a few sentences a part for a small child), a proposed
  answer to each point, each term a new reader may not know explained, no item number from another
  document (name a requirement of the root by what it says), and no text copied from the root that
  does not apply to this child. The child's workers read only this spec.
- Before `dispatch issue` creates any child, dispatch a fresh red-team subagent
  (`task(agent="oracle")`) with the
  root spec, the tree's open children with their repositories and pull requests, and the proposed
  child list, each child's title and whole spec. Its mandate: *"You did not write this list. Argue
  that each child should not exist. Name every child that is a to-do, a fix found in review, a
  proof of the root's acceptance, or a part of the architect's own work, and say where it folds:
  the root's pull request or an open child's."* A child survives only when it cannot ride in the
  root's pull request or an open child's: its code is in a repository no open pull request of this
  tree is in, or it is its own deploy: it must be applied on its own before other work of the tree
  can build on it.
  A repeated change — the same edit per program, directory or wave of this tree's own work —
  survives as one child, never one per instance, and a deletion of what this tree replaced rides
  in the pull request of the last change it waits on, never a child of its own: a wave is an order
  of merges, not a deliverable.
  Post the verdict, with the folds you made, as one `dispatch comment` on your own issue (the root,
  in a root session): the architect
  who wrote the list reads every child on it as needed, and the comment stops a later session
  filing the same children again.
- Then paste each survivor's whole spec into a fresh `task(agent="plan-gap-analyst")` to check it
  against `skill://dispatch-writing-specs`, and fix what it flags: your own reread misses the terms
  and item numbers you already know. File only the survivors, and reply under the verdict comment
  with each one's key.
- Before `release_children`, check that every child you filed is named under a verdict comment,
  because a child filed without one skipped the check. A child a human created is adopted as it is
  and needs none.
- `--project` is the issue key's prefix before `-<n>` (`LEGSMOKE-3` → `LEGSMOKE`).
- In a root session `$LEGION_ISSUE` is the root (it equals `$LEGION_TREE`): pass it as `--parent`.
  The daemon makes the sub-issue link from `--parent`.
- Order the returned keys into waves with `--blocked-by` links (a later wave's child blocked by the
  children it builds on), then release every child at once with `release_children`: Legion holds a
  released child until its last blocker closes, and a child is inert only until it is released.
- Never release a child `--blocked-by` its own parent: the parent now merges last, so that child
  would wait forever.
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
other one by commit (a lock file or dependency pin), the pin is that commit. Do not hold such a
child with `--blocked-by` instead, because Legion would start it at the blocker's close, before you
name the merged commit. Read the other repositories with your gh's read-only access
(`-R owner/repo`).

## A root with no code of its own

When every change of the tree lands through its children, the root's branch has no commit of its
own, and GitHub opens no pull request for a branch with no commits ahead of its base: implementing
would never end. The root's implementer then pushes one empty commit
(`jj new -m '<KEY>: the root of its children; no code of its own'`, with no trailer) and opens the
pull request on it, as its role prompt says; that push starts the required checks like any other.
Plan no code for the root in that case, and do not create a child only to give the root a commit.

## Architecture components

A project's architecture sources are the repositories whose `.dispatch/architecture/` files
Dispatch imports as its components (`skill://dispatch`, "Architecture components").

- **The components exist:** attach the root before you decompose, so every child inherits it:
  `dispatch issue-update --issue <root> --components-json '{"mode":"explicit","ids":[…]}'`.
- **They do not exist yet (bootstrap):** in the root's first PR (the implement worker pushes it),
  write `.dispatch/architecture/<id>.md` files for the planned components — front matter `title`,
  `parent`, `depends_on`, `external`; no `paths` yet. A project may have several sources, one per
  repository, and its model is all of their components together: the file goes in the branch of
  the source whose repository holds it, `parent` and `depends_on` name components of that same
  source only, and its id must be one no other source of the project uses (a repeated id fails that
  source's import, naming the repository that has it). The importer reads the source branch's
  head, so leave the root on `inherit` until that PR has merged: an attach before then answers
  `400 COMPONENTS_INPUT`, since the component does not exist. After the merge, run
  `dispatch architecture-sync --project <KEY>` (every source; `--repo <owner/name>` syncs that one
  alone) and attach the root; children created before then inherit it.
- Give a child its own `--components-json` only when it changes a narrower set, and
  `{"mode":"none","reason":"…"}` when it changes no code or file a component describes.
