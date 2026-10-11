-- packages/envoy/internal/broker/store/migrations/0012_one_session_kind.up.sql
-- An agent session on a machine its person logged in is one kind, session, however its launcher
-- holds its key: the broker no longer records a session the machine's helper enrolled for itself
-- (host) apart from one whose key a launcher generated elsewhere and enrolled through the helper
-- (box). A pod stays pod: its runtime id is the UID its projected token proves, and only a service's
-- launcher enrolls one (enroll.authorized). Every stored box and host row becomes session, so a
-- session enrolled before this migration reads as one enrolled after it.
--
-- The rows of credential_requests keep their body verbatim: a record's body, enrollment line
-- included, is the content its id hashes, and the approval chain re-verifies that id on every
-- release, so an old record still names the kind it was decided under.
--
-- The update rewrites every live and ended box and host row, and the constraint change holds off
-- writes to enrollments while it validates: the runner applies every migration in one transaction,
-- and enrollments holds one row per session ever enrolled, small enough that both are brief.
alter table enrollments drop constraint enrollments_kind_check;
update enrollments set kind = 'session' where kind in ('box', 'host');
alter table enrollments add constraint enrollments_kind_check check (kind in ('session', 'pod'));
