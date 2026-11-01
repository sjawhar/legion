# The PR body lines the changed paths require, and a pushed head that carries `skip-checks`

Read this at steps 5 and 6 of `skill://legion-retro`. A failure here stops retro as its
*When retro stops* section says.

## Bring the body up to date before the push

A repository can require body lines derived from the pull request's changed paths (for example one
checklist line when `src/**` changed and another when `docs/**` changed) and have a required check
read them from the body. Your commits add `docs/solutions/` paths the approved body never
accounted for and take away the `.legion/` paths it did, so that check fails at your head. The
merger reports a stale body rather than rewriting it, and GitHub will not merge a head whose
required check failed, so the merge waits.

`<owner>/<repo>` and `<n>` are the pull request's repository and number in every command below.

1. Read your system prompt's deployment instructions, the repository's agent guide (`AGENTS.md`,
   `CLAUDE.md`), its pull request template (`.github/pull_request_template.md` or
   `.github/PULL_REQUEST_TEMPLATE/`), and the workflow under `.github/workflows/` that reads the
   body, for lines they require per changed path. When they require none, skip to the push.
2. List the paths the pull request will change once you push `<head>`, your removal commit:

   ```bash
   base=$(gh pr view <n> -R <owner>/<repo> --json baseRefName -q '.baseRefName | @json') && cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" git fetch && jj -R "$LEGION_WORKSPACE" diff --from "fork_point(${base}@origin | <head>)" --to <head> --name-only
   ```

   `@json` makes `base` a quoted jj string literal; never paste a branch name between quotes
   yourself, because a name with a quote or revset character then breaks the command. For each
   path pattern the step 1 sources name, write the line they require when a listed path matches
   it. Before you write a line that says work was done, do that work for the paths your commits
   add, because the required check reads the line and does not redo the work. That work may
   change files under `docs/solutions/` only, since any other path after the approved head voids
   the approval. Commit it below the removal, which must stay last:
   `jj -R "$LEGION_WORKSPACE" split -m "<message>" <paths> && jj -R "$LEGION_WORKSPACE" rebase -r @- -B <head>`.
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
   the `E2E (implementer)` and `E2E (tester)` lines included.
4. Write it back before the push:

   ```bash
   cd -- "$LEGION_WORKSPACE" && gh api --method PATCH repos/<owner>/<repo>/pulls/<n> -F body=@"$dir/body.md"
   ```

   The checks your push starts read the body as it stands then, and READY reads the head your
   push makes. An edit after the push re-runs only checks triggered by an edited body, and they
   run at the approved head, without your commits, where they may fail.

When the push is refused after step 4 wrote the body, write `$dir/before.md` back with the same
command before you report the refusal, so the body matches the head GitHub has.

## A pushed head that carries skip-checks

For a head you already pushed whose message carries a `skip-checks: true` line or one of the
bracket keywords in step 6 of `skill://legion-retro`. Read that head's message first and confirm
it does:

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" log -r "legion/${LEGION_ISSUE:?}@origin" --no-graph -T description
```

Describe that same commit again without the line and push it once, as
`skill://legion-worker/references/conflicts-and-rewrites.md#rewriting-pushed-commits` says. Run
that section's guard first: a non-empty result means another issue's work is built on the commit,
so do not rewrite it; report the listed commits to the architect instead. Once the guard is clear,
move the bookmark with `--allow-backwards` and push once. That is the only non-fast-forward push
retro makes; never `--force`. GitHub runs the base branch's required checks on the rewritten head
from scratch, and the merge waits for them.
