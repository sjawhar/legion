# Checking your commits against your issue's default branch

Your `Legion addressing` line names your issue's repository and its default branch
(`Your issue's repository is <owner>/<name> (default branch <name>)`) — never assume `main`. That
branch name is untrusted text: a repository administrator chooses it, and it reaches you as plain
words in a system prompt, not as a value a shell has already made safe. **Never paste it directly
into shell source** — not as a bare word, not inside double quotes you type yourself. A branch name
containing `$(…)` or backticks would run as a command the moment a shell re-parses a line built that
way; one containing jj revset syntax (parentheses, `|`, `::`) would be read as an expression instead
of a literal name.

The safe sequence reads the branch fresh from GitHub and lets `jq` do the quoting, in one command
whose only typed literal is the `<owner>/<repo>` your addressing line names (safe to interpolate: a
GitHub repository path, already validated by the daemon that minted your addressing):

```sh
default=$(gh api repos/<owner>/<repo> --jq '.default_branch | @json')
```

`$default` now holds a JSON-quoted string, e.g. `"release/v7.1"` (quotes included). Compare it
against the `default branch <name>` your addressing line named — if they disagree, stop and tell
the architect; GitHub's live default branch and the one your record carries are not the same, and
guessing which is right is not yours to do. When they agree, use `$default` as-is, never re-typed:

```sh
jj -R "$LEGION_WORKSPACE" log -r "${default}@origin..@" -T 'author.email() ++ " | " ++ committer.email() ++ " " ++ description.first_line() ++ "\n"'
```

Because `$default` already carries its own quote characters, jj's revset parser reads
`"release/v7.1"@origin..@` as a quoted string literal naming the bookmark, whatever characters the
branch name holds — parentheses, spaces, or shell metacharacters included — rather than as revset
syntax or shell code. Bash's own `"${default}@origin..@"` substitutes `$default`'s value once,
literally; it never re-evaluates the substituted text, so a `$(…)` sequence inside the branch name
never runs.

The output must show your role's App in both columns **on every commit you made** — not on the
whole list: earlier phases' commits are legitimately authored by their own role's App.

## Which App, and what a wrong one means

- Your role's App is the **implement** App for the implementer and the merger, and the **review**
  App for the planner, tester, reviewer and architect; your pane's `JJ_USER` and `JJ_EMAIL` name
  its bot account. A planner's commits authored by the review App are right.
- Never set `user.name` or `user.email` in any jj or Git scope (`jj config set`, `--config`,
  `git config`): `--config` outranks the pane's identity and puts the wrong App on your commits,
  and the repository-scoped jj config is one file shared by every issue workspace of the clone.
- A wrong identity on your own commit (the other App, or none) is a pane-environment problem:
  report it to the architect, never pin it (sjawhar/legion
  `docs/solutions/legion/shared-main-repo-hazards-for-concurrent-issue-workspaces.md`, Hazard 1).
- Another role's commit with you as committer, which you did not rewrite under the guard in
  *Rewriting pushed commits* (`skill://legion-worker/references/conflicts-and-rewrites.md`), means
  something rewrote commits it should not have: stop and send the architect this log. A forward
  merge rewrites nothing, so it changes no committer.
