-- 0105: delivery_session_group_members logs its changes (0100). Its own migration, so the
-- transaction holds this table's lock alone, for milliseconds, and never while waiting for another
-- table's.
create trigger delivery_session_group_members_changed after insert or update or delete on delivery_session_group_members
  for each row execute function delivery_session_groups_changed();
