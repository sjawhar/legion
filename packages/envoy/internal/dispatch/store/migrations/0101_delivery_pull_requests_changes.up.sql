-- 0101: delivery_pull_requests logs its changes (0100). Its own migration, so the transaction holds
-- this table's lock alone, for milliseconds, and never while waiting for another table's.
create trigger delivery_pull_requests_changed after insert or update or delete on delivery_pull_requests
  for each row execute function delivery_pull_requests_changed();
