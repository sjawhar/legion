-- 0051_issue_suspend_release.up.sql — the `release` key of an issue_suspend payload goes. An
-- issue's close now deletes its Sandbox in every case (runtime.IssueSuspender): the issue's disk
-- is its pod's own and goes with the pod, so a close that kept the Sandbox kept nothing. The
-- payload decodes strictly (record.DecodeOutboxPayload) and record.IssueSuspend no longer has the
-- field, so a row the previous release queued with it would fail to decode, and its issue's
-- lifecycle lane would stall on a row no retry can finish. This strips the key from every row
-- still queued, which leaves each row the close it already was; one written without it is
-- untouched.
update outbox set payload = payload - 'release' where kind = 'issue_suspend' and payload ? 'release';
