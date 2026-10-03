-- packages/envoy/internal/broker/store/migrations/0008_lookup_indexes.up.sql
-- Two lookups that had no index, each a scan of a whole table:
--
-- grants_enrollment_live: ending an enrollment revokes its live grants (enroll.endEnrollment's
-- `update grants ... where enrollment_id=$1 and revoked_at is null`), and the sweeper ends every
-- lapsed enrollment in one tick, so each one ended scanned all of grants. The index holds live
-- grants only, the rows that update reads.
--
-- requests_record: a credential-request record finds its request by record_id, when an approver
-- decides it (requests.Machine.ApplyDecision's `for update`) and whenever an agent_secret record
-- is read (requests.Machine.ReadRecord, which takes its state from its request), so each of those
-- scanned all of requests.
--
-- Plain create index, which holds off writes to each table while it builds: the runner applies
-- every migration in one transaction, where CONCURRENTLY is refused, and both tables are small
-- enough that the build is brief.
create index grants_enrollment_live on grants (enrollment_id) where revoked_at is null;
create index requests_record on requests (record_id);
