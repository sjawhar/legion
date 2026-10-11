-- 0037_agent_secrets_credential.up.sql — the secrets broker's launcher credential each project's
-- daemon won with a machine login a person approved: the broker's id for it, the private key it is
-- bound to (PKCS #8 DER), and its expiry, so a restarted daemon takes it back instead of asking a
-- person again (agentsecrets.Keeper). Each renewal moves the credential to a new key, and
-- next_private_key holds the key a renewal asked for while its answer is unknown. Keyed by the
-- broker's URL too: a daemon pointed at another broker holds nothing there. This is the one raw
-- secret the daemon's database holds: whoever can read the table can sign as the daemon toward the
-- broker until the next renewal moves the key, or someone revokes the login on Dispatch's
-- machine-login page.
create table agent_secrets_credentials (
  project text not null,
  broker_url text not null,
  credential_id text not null check (credential_id <> ''),
  private_key bytea not null,
  next_private_key bytea,
  expires_at timestamptz,
  saved_at timestamptz not null default now(),
  primary key (project, broker_url));
