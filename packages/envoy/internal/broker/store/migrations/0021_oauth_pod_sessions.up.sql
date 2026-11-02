-- packages/envoy/internal/broker/store/migrations/0021_oauth_pod_sessions.up.sql
-- Which pod may exchange under which session name: the registration a deployment's daemon writes
-- for a pod it starts (POST /v1/oauth/exchange/pod-sessions), which the token exchange reads in
-- its own transaction before it mints a token for that pod, asking the cluster nothing.
-- The exchange binds a session name to a principal — the listed service account — and never to an
-- instance, so a registered successor keeps the name an entry or an earlier pod of that account
-- holds. What separates two pods of one account, which are one principal, is this table and
-- nothing else: a pod of an account whose entry fixes no session writes under a name only while a
-- row here says that pod uid may use it, and an account whose entry fixes one needs no row,
-- because the name is the configuration's rather than the request's.
-- Every row expires, and the store's sweep deletes it through the expires index, as 0018's two
-- tables are swept: a registration is kept oauthstore.PodSessionRetention (24 hours, the longest
-- a presented pod token can live) past its last write, so the authority stays the registrar's —
-- a daemon that stops registering costs its pods their writes within that window.
create table oauth_pod_sessions (
  pod_uid text not null,
  session text not null,
  expires_at timestamptz not null,
  primary key (pod_uid, session)
);
create index oauth_pod_sessions_expires on oauth_pod_sessions (expires_at);
