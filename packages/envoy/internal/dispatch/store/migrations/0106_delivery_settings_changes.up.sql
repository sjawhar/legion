-- 0106: delivery_settings logs its changes and notifies its freshness (0100). Its own migration, so
-- the transaction holds this table's lock alone, for milliseconds, and never while waiting for
-- another table's.
create trigger delivery_settings_changed after insert or update or delete on delivery_settings
  for each row execute function delivery_settings_changed();
