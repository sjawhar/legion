-- packages/envoy/internal/broker/store/migrations/0022_signin_status.up.sql
-- What GET /signin/status answers: whether the sign-in provider accepted the broker's client at
-- the last check and when that check was taken, when a sign-in last succeeded, when one last
-- failed and why, and when a decision was last made on the approval page. It is kept here, and
-- not in a process's memory, so every replica and every restart answers the same thing: a check
-- one pod took is the check the next pod reports.
--
-- The table holds one row and can hold no other: its primary key is a boolean that may only be
-- true, so a second row is a duplicate key rather than a row something has to find and sweep.
-- Nothing here grows, and nothing here expires. Each writer upserts that row and sets only the
-- columns of the fact it writes, so two replicas writing different facts at the same moment
-- never lose each other's.
--
-- Both vocabularies are closed in the database and not in Go alone, because this answer is
-- public: a state or a reason outside these words - a provider's own message, which can carry a
-- person's login or a secret - is refused here before it can ever be answered to a caller who
-- showed no credential.
create table signin_status (
  only_row boolean primary key default true check (only_row),
  client_check_state text check (client_check_state in ('ready', 'client_rejected', 'issuer_unreachable')),
  client_checked_at timestamptz,
  last_sign_in_at timestamptz,
  last_failed_sign_in_at timestamptz,
  last_failed_sign_in_reason text check (last_failed_sign_in_reason in (
    'invalid_client', 'unauthorized_client', 'redirect_uri_mismatch', 'invalid_grant',
    'issuer_unreachable', 'bad_issuer', 'bad_audience', 'bad_signature', 'bad_nonce',
    'not_in_domain', 'email_not_verified')),
  last_decision_at timestamptz,
  -- A state with no time, or a reason with no time, would be a fact nobody can date; each pair
  -- is written by one statement, so each is both set or both absent.
  check ((client_check_state is null) = (client_checked_at is null)),
  check ((last_failed_sign_in_reason is null) = (last_failed_sign_in_at is null))
);
