# Writing a retro learning as a Dispatch comment

Read this at step 3 of `skill://legion-retro`, when you write what the work taught. It is one
`dispatch comment` on the issue, and its reader is the skill librarian, who turns a point into a
rule in a skill, a role prompt or a repository guide. Retro commits nothing:
`docs/solutions/` stays as the history of the rules written before this, and no role writes there.

## What a point must carry

Write each kept point as a rule a worker on another issue could follow, and give it:

- **The rule**, in a few imperative lines. State it in the present tense, about the work, never
  about this pull request.
- **The evidence** below the rule: the file, line or commit of this pull request that shows it,
  and what went wrong before the rule. A reader takes a story as an instruction, so the story
  never stands in the rule's place.
- **Where it belongs**: the skill, role prompt or guide a reader would look in — for example
  `skills/legion-worker/references/tools.md` or `packages/daemon/internal/prompts/roles/tester.md`.
  Name the file; the librarian decides whether to write it there.
- **What it replaces**, when an instruction the repository already carries is wrong: quote that
  instruction and its path, so the librarian can find both.

Drop a point that cites no file, line or commit of this pull request, and a point that only
restates what a skill already says.

## Find what already covers the topic

1. `dispatch search --query '<two or three words naming the topic>'` finds the issues and
   comments that hold it, this project's retros included.
2. Search `docs/solutions/` with the `grep` tool, case-insensitive, for the same words, and read
   each hit's H1 and rule: a rule written before this change may already say it, or may say the
   opposite, which is itself the point to report.
3. Search the skills and role prompts the point would change (`skills/`,
   `packages/daemon/internal/prompts/`). A rule one of them already states is no learning.

## The comment

One comment holds every kept point, each under its own `###` heading, so the issue carries one
record of the retro rather than a thread of them:

```bash
dispatch comment --issue '<KEY>' --body-file - <<'EOF'
## Retro learnings

### <the rule, in one line>

<the rule, a few imperative lines>

Evidence: <file, line or commit of this pull request, and what went wrong>.
Belongs in: <the skill, prompt or guide>.
Replaces: <the instruction and its path>, or nothing.

<!-- legion: {"session":"<session-id>","phase":"retro"} -->
EOF
```

When no point qualifies, write no comment and say so in the retro message (step 5 of
`skill://legion-retro`).
