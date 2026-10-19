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
