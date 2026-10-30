---
title: "A merge that only renumbers migrations is proven in both directions"
category: envoy
tags:
  - migrations
  - forward-merge
  - renumber
  - jj
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2391"
---

# A merge that only renumbers migrations is proven in both directions

- A scripted renumber edits only the branch's own lines. A line that also exists in main's copy of
  the file is main's, and the script leaves it alone. Inside a jj conflict, main's side appears as
  `+` lines of a diff, so the script does not see them as main's text and can rewrite them.
- Check main's side: `jj diff --from main@origin --to @ --git` removes no line of main's. A removed
  line that names a migration number is a renumber that reached main's text.
- Check the branch's side: fingerprint the branch's diff from its fork point twice, before and
  after the merge, with every migration number replaced by one placeholder
  (`sed -E 's/(^|[^0-9])01[45][0-9]([^0-9]|$)/\1NNNN\2/g'`, applied until nothing changes). Equal
  fingerprints show the pull request's own diff is byte-identical apart from the renumbering. The
  plain fingerprint moves with every renumber, so it cannot show this.
- Post both fingerprints on the pull request with each resolution, so the tester and the reviewer
  can confirm the merge without reading the whole diff again.

## Evidence

Pull request sjawhar/legion#2391. Main took migration numbers twice while the branch was open, so
the branch's seven migrations moved from 0141-0147 to 0142-0148 (bc54fc4fdd2e), and then to
0145-0151 (e8337760317b). In the second merge the script also renumbered main's new paragraph about
its 0142-0144 migrations inside the conflict hunk in `packages/envoy/cmd/dispatch/README.md`. The
diff from `main@origin` showed it, and that paragraph was restored from main word for word before
the push. The normalised fingerprint was `6fa0b746c92d` at both bc54fc4f and e8337760, while the
plain one moved from `9c3fe489e68a` to `9854397f0d59`. The reviewer found 0 differing lines of
10,005 (PR comment 6512920542 holds the diff check).
