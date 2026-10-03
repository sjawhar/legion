-- packages/envoy/internal/broker/store/migrations/0008_grants_enrollment_live.up.sql
-- Ending an enrollment revokes its live grants (enroll.endEnrollment's `update grants ... where
-- enrollment_id=$1 and revoked_at is null`), and the sweeper ends every lapsed enrollment in one
-- tick, so without an index each one ended scans the whole grants table. The index holds live
-- grants only, the rows that update reads.
--
-- A plain create index, which holds off writes to grants while it builds: the runner applies
-- every migration in one transaction, where CONCURRENTLY is refused, and the table is small enough
-- that the build is brief.
create index grants_enrollment_live on grants (enrollment_id) where revoked_at is null;
