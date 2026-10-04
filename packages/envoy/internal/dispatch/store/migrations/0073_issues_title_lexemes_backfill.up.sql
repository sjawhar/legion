-- 0073_issues_title_lexemes_backfill.up.sql
-- Fills issues.title_lexemes (0072) on every issue stored before it, with title_lexemes(title), as
-- the issues trigger does for every row after it. Setting only that column does not fire the
-- trigger, which reads an update of the key or the title, so search keeps the vector it has.
--
-- It takes issues EXCLUSIVE before it writes a row. The update alone locks every issue's row in the
-- order the rows lie on disk, and a write that locks two issues takes them in another order: a
-- reparent locks the issue and its new parent in key order (lockIssueAndParent,
-- api/issue_parent.go), which is text order, so reparenting LK-9 under LK-18000 locks the later row
-- on disk first. When the reparent held that row while the update, already holding the earlier one,
-- waited for it, and then asked for the earlier one, Postgres's deadlock check killed one side with
-- 40P01: the boot, or the reparent with a 500. Reproduced on 60,000 issues with the reparent 0.3 s
-- into the update, twice in two runs. The table lock is taken while the update holds nothing, so a
-- write it waits for completes, and no write waits for it while it holds a row that write needs:
-- the same reparent waits for this commit and then commits itself. 0066's form, a lock_timeout of
-- 500 ms against the 1 s deadlock_timeout, would have this give up first when it waited first, but
-- not when the reparent had already waited half a second for its row when this asked for the
-- reparent's; the table lock leaves no such case.
--
-- EXCLUSIVE blocks no read. It holds every write of an issue row, and every write of a row whose
-- key names one (its foreign-key check takes the row's key share), until this commits: on 2,905
-- issues holding production's titles the update took 0.58 s at load 85, 0072 3 ms. Such writes
-- already wait for it, since the event each appends locks and updates its issue's row. It waits for
-- the lock at most the runner's lock_timeout (pgmigrate.LockTimeout, 5 s) and fails the boot past
-- that, as 0072 does. Its own migration, so the ACCESS EXCLUSIVE lock 0072 takes is not held while it
-- runs.
lock table issues in exclusive mode;

update issues set title_lexemes = title_lexemes(title);
