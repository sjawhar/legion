-- packages/envoy/internal/broker/store/migrations/0017_oauth_token_exchange.up.sql
-- The token exchange's OAuth 2.0 state (authelia.com/provider/oauth2): which identity holds a
-- session name, and the replay store the exchange's subject tokens are kept out of a second use
-- by.
-- Every row of both expires, and the store's sweep deletes it through the expires index each
-- table carries.
-- A session name is held by the platform identity that first claimed it (a pod's uid, a workflow
-- run under its repository, or a dev box's machine credential id) until the token the claim was
-- made for would have expired, so a caller that may exchange cannot mint a token naming a session
-- another identity is writing under.
create table oauth_session_bindings (
  session text primary key,
  identity text not null,
  expires_at timestamptz not null
);
create index oauth_session_bindings_expires on oauth_session_bindings (expires_at);
-- The jti of every custom JWT subject token the exchange has accepted (rfc8693.Storage's
-- SetTokenExchangeCustomJWT), remembered until the token it came from would have expired: the
-- library reads any error from that insert as a replay, so the row is what refuses the second
-- presentation of one token.
create table oauth_exchange_jtis (
  jti text primary key,
  expires_at timestamptz not null
);
create index oauth_exchange_jtis_expires on oauth_exchange_jtis (expires_at);
