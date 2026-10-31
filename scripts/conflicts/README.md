# Conflict measurement script

`measure.ts` counts the merge conflicts Legion's pull request branches hit when they merge main, over a window of time, so a change meant to remove them can be measured before and after.
It is the Bun port of the Python script that took the before measurement, `conflict-baseline.py`, kept on this pull request's issue in Dispatch.

It needs git 2.38 or later (`git merge-tree --write-tree`) and a `gh` CLI signed in to GitHub with read access to the repository.

This file uses semantic line breaks: one sentence per line, and a long sentence may break after a clause.
A blank line separates list items.
Add a new item next to the item it belongs with, never at the end of a list unless it belongs there, so two pull requests that add items or change different sentences do not conflict.
`bun scripts/semantic-line-breaks.ts scripts/conflicts/README.md` checks the layout, and `--write` fixes it.

## What it counts

For every pull request it reads, the script takes GitHub's list of the pull request's commits:

- **Forward merges.**
  A commit with two or more parents, one on the pull request and another not, is a forward merge when the outside parent is on main or on the pull request's base branch (`forward`), a merge of some other branch otherwise (`other-branch`), and a merge between two commits of the pull request when both parents are on it (`internal`, such as a stacked branch merged into its parent).
  Each forward merge is re-run with `git merge-tree --write-tree --name-only --no-messages <branch parent> <upstream parent>`;
  the paths it reports are the merge's conflicted files, and none means the merge was clean.
  A merge with a parent the clone lacks and cannot fetch is counted as a `forward` merge whose conflicts are undeterminable (`error: "parent object missing"`):
  without the parent, its kind is not checked by ancestry, so it may be an `other-branch` merge.

- **Force pushes.**
  Each force push's pre-push head is merged with main as it stood at the push time (`git rev-list -1 --first-parent --before=<push time>`).
  A conflict there means the rewrite, usually a rebase, plausibly resolved a conflict.
  Pull requests whose base is not main are undeterminable here.

- **Open-PR snapshot.**
  Each open pull request's head merged with the current tip of its base.

A merge commit carried by several pull requests (a stack) counts once and is credited to each of them.
Only merges and force pushes dated inside the window count.
Each conflicted file gets a category (`changelog`, which includes any file under a `.changes/` directory, `agents-doc`, `generated`, `version-log`, `migration`, `lockfile`, `doc`, `code`), and each conflicting merge is put in the four tiers by whether every one of its conflicted files is a changelog;
a changelog, `AGENTS.md`/`CLAUDE.md` or generated file; those or the daemon API version log; those or any other doc.

Its limits:

- Only textual conflicts count: a merge git completes cleanly but that breaks the build does not.

- A rebase that never reached GitHub does not show: only pushed commits and force pushes are read.

- GitHub's commit list follows a pull request's current head, so a force push after the window drops that pull request's earlier merges from the count.
  Measure soon after the window ends.

- The merges are re-run with git, while Legion workers merge with jj, so a count is git's view of each merge, not the conflict the worker saw.

- A merge is dated by its committer date, which jj rewrites when it rewrites a commit.

## Which pull requests it reads

Two lists, taken together by number.
The first is every pull request, in any state, that GitHub's search says was updated at or after the window's start:
`gh pr list --state all --search "updated:>=<since>"`.
A pull request carrying a merge dated in the window was pushed after that merge, and one merged or closed in or after the window was updated when it closed, so this list holds every one of them.
The second is every pull request open now: `gh pr list --state open`.
It adds those opened before the window and still open that nothing updated since, which the first list misses (the Python run read every open pull request too).
Each list is cut at 1000 pull requests (GitHub's search returns no more), so a list of 1000 stops the run (exit 1):
from the search, with the window too large for one search (measure shorter windows instead); from the open list, with too many open pull requests for one list.

`--branch-prefix` is applied to the two lists before any pull request is fetched, so a prefixed run's raw records hold only the pull requests whose head branch starts with the prefix.

Both lists run to when the run is made, not to the window's end:
the search grows with every day between them, and the open list holds pull requests opened after the window.
The summary therefore narrows them to the pull requests that existed in the window:
one counts when it was opened by the window's end and was neither merged nor closed unmerged (by its `closedAt`, when the record has one) before the window's start, or when it carries a merge or force push dated inside the window (a pull request opened later from a branch that merged main in the window).
Every pull request that existed in the window is open now or was updated since its start, so it is on one of the lists whenever the run is made.
Every section counts only these, so a run made days after its window counts the same pull requests as one made as it ended.
The Python run's records are read as they are.

## Flags

- `--since <bound>`

  The window's start, required for a live run: `YYYY-MM-DD` (from `00:00:00Z`) or an RFC 3339 instant taken exactly.

- `--until <bound>`

  The window's end, inclusive: `YYYY-MM-DD` (through `23:59:59Z`) or an RFC 3339 instant.
  Default: now.

- `--repo <owner/name>`

  The repository measured.
  Default `sjawhar/legion`.

- `--workdir <dir>`

  Where the bare clone (`legion.git`) and the raw records go.
  Default `$TMPDIR/conflicts`, else `./conflicts`.

- `--no-fetch`

  Use the bare clone as it is; it is still cloned when missing.

- `--from-json <file>`

  Summarise an earlier run's raw records instead of reading GitHub.
  `--since` and `--until` do not apply: the file carries its window.

- `--jobs <n>`

  Pull requests fetched and analysed at once.
  Default 8.

- `--branch-prefix <prefix>`

  Count only pull requests whose head branch starts with it (`legion/` for Legion's).
  Default: every pull request.

- `--fixed '<glob>=<commit>'`

  Repeatable.
  The commit on main that fixed the files the glob (`Bun.Glob` syntax) matches; see fix states below.
  The text after the last `=` is the commit.

- `--compare-with <file>`

  An earlier run's raw records (the Python run's included); prints before → after after the report, with the same `--branch-prefix`.

The report is markdown on stdout.
A live run writes its raw records to `<workdir>/conflicts-<since>_<until>.json` and names the file on stderr.
The exit code is 0 for a report, 2 for a usage error (a bad flag or window, `--since` missing from a live run, an unreadable or invalid raw-records file, a `--fixed` commit git cannot resolve or that is not on main) and 1 for a failed run (git, `gh`, a list's cap).
A bad flag, or a live run's bad window, is refused before any git or `gh` call.

## The report's sections

1. **All PRs read**: every pull request read (for a run of this script, every one that existed in the window), merges deduplicated across all of them:
   conflicting forward merges, the pull requests carrying one, the tiers, and the conflicting merges by category and by file.
   This is the section the comparison uses, and the one `--fixed` annotates.

2. **Merged in the window**: the pull requests merged inside the window, as the Python script's "merged PRs" section.

3. **Every other pull request read**: open now, merged after the window, or closed unmerged; merges the merged section already counted are left out.

4. **Force pushes**, over every pull request read.

5. **Open-PR snapshot**, over the pull requests read that are open now.

For the Python run's records, sections 2 and 3 split by `state` as the Python report did:
`MERGED`, then the rest (that run read only merged and open pull requests).

## Fix states

With `--fixed`, each conflicted file a glob matches, in each conflicting merge of the first section, gets a state by ancestry (committer dates are not used, since jj rewrites them):

- `before`: the fix is not in the merge's upstream parent: the conflict predates the fix.

- `transition`: the fix is in the upstream parent but not in the branch parent: the branch still held edits made before the fix.

- `after`: the fix is in both parents: the fix did not prevent this conflict.

The by-file table gains `before`, `transition` and `after` columns for the matched files, and the comparison shows the after run's states.
The first matching glob wins.
Each glob also totals the conflicting merges with a conflicted file it matches, each merge once however many of its files match, with the pull requests carrying them and each merge's state (zeros when it matched nothing);
the comparison shows these totals before and after, the before run's counted without git.
A glob also totals zero when an earlier `--fixed` glob takes every path it matches:
with `packages/*/CHANGELOG.md` given before `packages/envoy/CHANGELOG.md`, the second totals 0, so give narrower globs first.
Fix states need git, so `--fixed` with `--from-json` uses the bare clone in `--workdir`, fetched unless `--no-fetch`.

## Raw records

The raw records keep the Python run's layout and key names, so one format serves both:
top-level `since`, `until` and `origin_main`, and `prs[]`, each with `number`, `title`, `state`, `baseRefName`, `headRefName`, `headRefOid`, `mergedAt`, `createdAt`, `closedAt` (`null` while open;
absent from the Python run's records and earlier runs of this script), `commit_count`, `merges[]` (`oid`, `date`, `headline`, `parents`, `kind`, `branch_parent`, `upstream_parent`, `upstream_on_main`, `upstream_on_base`, `conflicts`, `error`), `force_pushes[]` (`date`, `before`, `after`, `main_at_push`, `already_contains_main`, `conflicts`, `error`), and for open pull requests `open_conflicts_now` and `open_behind_base`.
`conflicts` is the sorted list of conflicted paths, `[]` for a clean merge and `null` when undeterminable.
A run of this script adds `measured_at` (RFC
3339) and `selection` (`"updated-since"`); a file without them is read as the Python run.
The Python run's records for 2026-10-20 to 2026-10-27 are `conflict-baseline-2026-10-20-to-27-json`, kept on this pull request's issue in Dispatch.
They carry no `measured_at`, so a comparison gives their window as running to `2026-10-27T23:59:59Z` (8 days), though that run was made at about 11:21Z that day;
the comparison's shares, not its raw counts, are what compares windows of different lengths.

## Examples

Reproduce the before measurement from the Python run's raw records (no network but Dispatch's):

```bash
dispatch doc-read --issue <KEY> --artifact conflict-baseline-2026-10-20-to-27-json \
  | tail -n +3 > "$TMPDIR/before.json"
bun scripts/conflicts/measure.ts --from-json "$TMPDIR/before.json" --branch-prefix legion/
```

Measure a window live from GitHub:

```bash
bun scripts/conflicts/measure.ts --since 2026-10-20 --until 2026-10-27T11:21:00Z \
  --branch-prefix legion/
```

Measure the 7 days after a fix and compare them with the before run, with each fix's merge commit:

```bash
bun scripts/conflicts/measure.ts --since <merge time> --until <merge time + 7 days> \
  --branch-prefix legion/ --compare-with "$TMPDIR/before.json" \
  --fixed 'packages/*/CHANGELOG.md=<changelog fix commit>' \
  --fixed 'packages/claude-envoy/{dist,legion}/**=<generated files fix commit>'
```
