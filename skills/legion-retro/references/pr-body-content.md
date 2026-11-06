# The PR body lines the changed paths require

Read this at step 4 of `skill://legion-retro`. A failure here stops retro as its
*When retro stops* section says.

## Bring the body up to date before you complete

A repository can require body lines derived from the pull request's changed paths (for example one
checklist line when `src/**` changed and another when `docs/**` changed) and have a required check
read them from the body. The body approved earlier in the round may not account for every path the
pull request now changes, so that check fails at the head. The
merger reports a stale body rather than rewriting it, and GitHub will not merge a head whose
required check failed, so the merge waits.

`<owner>/<repo>` and `<n>` are the pull request's repository and number in every command below.

1. Read your system prompt's deployment instructions, the repository's agent guide (`AGENTS.md`,
   `CLAUDE.md`), its pull request template (`.github/pull_request_template.md` or
   `.github/PULL_REQUEST_TEMPLATE/`), and the workflow under `.github/workflows/` that reads the
   body, for lines they require per changed path. When they require none, there is nothing to do.
2. List the paths the pull request changes at `<head>`, the approved head your task names:

   ```bash
   base=$(gh pr view <n> -R <owner>/<repo> --json baseRefName -q '.baseRefName | @json') && cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" git fetch && jj -R "$LEGION_WORKSPACE" diff --from "fork_point(${base}@origin | <head>)" --to <head> --name-only
   ```

   `@json` makes `base` a quoted jj string literal; never paste a branch name between quotes
   yourself, because a name with a quote or revset character then breaks the command. For each
   path pattern the step 1 sources name, write the line they require when a listed path matches
   it. A line that says work was done names work this pull request did: retro commits nothing, so
   a line whose work is undone is a finding for the architect, not work you do here.
3. Make a directory outside `$LEGION_WORKSPACE` with `dir=$(mktemp -d)`, the one place you write
   outside it: jj would commit a file inside it, and a fixed name in a shared `/tmp` can hold
   another session's body. Save the live body as `$dir/before.md` (to restore on failure) and
   `$dir/body.md` (to edit), and stop unless the read succeeded and is not empty:

   ```bash
   cd -- "$LEGION_WORKSPACE" && ( set -o pipefail && gh api repos/<owner>/<repo>/pulls/<n> --jq .body | tee "$dir/before.md" "$dir/body.md" ) && test -s "$dir/before.md"
   ```

   Without `pipefail` a failed read exits with `tee`'s status and leaves two empty files. The
   parentheses keep `pipefail` to this line, since your shell persists and a later `cmd | head`
   would exit 141 under it. In `$dir/body.md`, change only the lines the step 1 sources derive
   from paths, adding a missing one where they place it. Leave every other line byte for byte,
   the `Checks` block included.
4. Write it back:

   ```bash
   cd -- "$LEGION_WORKSPACE" && gh api --method PATCH repos/<owner>/<repo>/pulls/<n> -F body=@"$dir/body.md"
   ```

   A body edit changes no commit, so the reviewer's approval stands, and a check that judges the
   body runs again at once on the head as it is.

When the write back is refused, or the check it feeds fails, report it to the architect as *When
retro stops* says, with the command and its output.
