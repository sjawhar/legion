-- INFRA-258: the delivery sweep (api/delivery_sweep.go, resumeOwedDeliveries) reads, once a minute
-- under the maintenance loop's lease, every message and comment-mention delivery attempt still
-- pending, oldest first, to resume what a process that died left owed. Pending attempts are the
-- few in flight and those a crash stranded, so a partial index on them keeps that read to their
-- number instead of a scan of every attempt ever made, which is what both tables otherwise hold.
--
-- Two indexes, nothing else: no row is read, refused or rewritten, and the release before this
-- one neither reads nor depends on them, which is what a rolling update requires of a migration
-- (cmd/dispatch/README.md, "Database migrations"). Each build takes its table's SHARE lock, which
-- holds that table's writes - delivery attempts being recorded - for as long as the build reads the
-- table once.
create index message_deliveries_pending on message_deliveries (created_at) where state = 'pending';
create index comment_deliveries_pending on comment_deliveries (created_at) where state = 'pending';
