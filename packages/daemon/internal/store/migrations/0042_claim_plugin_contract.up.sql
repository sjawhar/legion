-- 0042_claim_plugin_contract.up.sql — the plugin contract each process registers with (LEGION-164):
-- claims.plugin_contract is the current registration's declared contract, 0 meaning none recorded
-- yet, which every claim has before its first registration on this release. claims.plugin_relaunch_for
-- is the daemon contract the last stale-plugin relaunch was made for, 0 meaning none, so a
-- registration that still differs while it equals the daemon's own contract is charged as a launch
-- failure rather than relaunched again uncharged. controllers.plugin_contract is the same, for the
-- controller a session registers with.
alter table claims add column plugin_contract integer not null default 0;
alter table claims add column plugin_relaunch_for integer not null default 0;
alter table controllers add column plugin_contract integer not null default 0;
