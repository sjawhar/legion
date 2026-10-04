-- 0003_launch_uncertain.up.sql — an unrecorded launch remains non-spawnable across restarts.
alter table claims drop constraint claims_state_check;
alter table claims add constraint claims_state_check check (state in (
  'queued', 'launch_uncertain', 'launching', 'shim_connected', 'registered', 'ready',
  'working', 'idle', 'suspended', 'failed', 'retired'));
