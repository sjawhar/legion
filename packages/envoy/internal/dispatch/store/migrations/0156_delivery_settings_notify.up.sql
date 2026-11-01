-- The delivery intakes (internal/dispatch/delivery, SettingsCache) reload
-- delivery_settings when it changes instead of reading it every 30 seconds. delivery_settings_changed
-- now also announces on dispatch_delivery_settings when a column other than the freshness ones
-- changed: the same branch that logs the change, so a GitHub event or a reconcile pass, which
-- rewrites only freshness, announces nothing. Each process's shared listener (pglisten.Hub) wakes
-- its settings cache on the announcement.
--
-- The transaction already notifies: delivery_change_log sends delivery_changes (0100) in this very
-- branch, and every insert or update sends delivery_freshness, so this adds no notify-queue lock at
-- commit to a transaction that took none.
--
-- The body is 0100's otherwise unchanged, and 0106's trigger keeps calling it by name. The release
-- before this one never listens on the channel and keeps polling the row, so it keeps working
-- beside this one.
create or replace function delivery_settings_changed() returns trigger language plpgsql as $$
declare
  freshness constant text[] := array['last_event_at', 'last_reconcile_at', 'last_error', 'updated_at'];
begin
  if tg_op <> 'UPDATE' or (to_jsonb(old) - freshness) is distinct from (to_jsonb(new) - freshness) then
    perform delivery_change_log('settings', null, null, null, null, null);
    perform pg_notify('dispatch_delivery_settings', '');
  end if;
  if tg_op <> 'DELETE' then
    perform pg_notify('delivery_freshness', json_build_object(
      'last_event_at', new.last_event_at, 'last_reconcile_at', new.last_reconcile_at,
      'last_error', left(new.last_error, 1000))::text);
  end if;
  return null;
end $$;
