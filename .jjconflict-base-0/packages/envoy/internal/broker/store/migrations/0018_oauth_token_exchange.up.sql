-- packages/envoy/internal/broker/store/migrations/0018_oauth_token_exchange.up.sql
-- The token exchange's OAuth 2.0 state (authelia.com/provider/oauth2): which principal holds a
-- session name, and the replay store the exchange's subject tokens are kept out of a second use
-- by.
-- Every row of both expires but a session name the configuration fixes, and the store's sweep
-- deletes the rest through the expires index each table carries.
-- A session name belongs to the principal that first claimed it: a listed service account (every
-- pod of it), a listed workflow (every run of it) or a person (every machine of theirs), never
-- the one pod, run or machine that asked. The principal holds it until
-- oauthstore.SessionRetention (90 days) after the last token minted under it expires, and a name
-- an entry of the configuration fixes is held for that entry's principal with an expires_at of
-- 'infinity' for as long as the configuration fixes it, so a caller that may exchange cannot mint
-- a token naming a session another principal writes under, and no lapse of a token hands a name
-- to another principal while its holder has used it within the retention.
create table oauth_session_bindings (
  session text primary key,
  principal text not null,
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
