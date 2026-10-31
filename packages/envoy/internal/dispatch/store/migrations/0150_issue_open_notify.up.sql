-- 0150: every change of an issue's closed_at, closing it or reopening it, wakes every envoy-dispatch
-- process. Each process holds its own room for each document it serves, and a room's
-- browsers are read-only exactly while its issue is closed, a scope ygo fixes when a browser
-- connects; so each process closes its rooms of the issue's documents when it hears this, and
-- their browsers reconnect with the scope the issue now allows (docs.Service.issueOpenChanged, on
-- the room-sync lane of the channel). The payload is the issue's key; the listener reads the
-- issue's state itself rather than trusting the notification's order.
--
-- A trigger rather than a statement in the route that closes an issue, so every writer of
-- closed_at reaches every listening process, the release before this one's included while it still
-- serves during the roll: that release does not listen on the channel, so its own rooms behave as
-- they did, but an issue it closes still closes this release's rooms. Postgres delivers the
-- notification only when the writing transaction commits, and folds repeats of one key within it.
create function issues_open_notify() returns trigger language plpgsql as $$
begin
  perform pg_notify('dispatch_issue_open_changed', new.key);
  return null;
end $$;
-- Its own migration, so the transaction holds issues' SHARE ROW EXCLUSIVE lock (create trigger's)
-- alone, for milliseconds, and never while waiting for another table's: a write to issues waits
-- behind it, and a read does not. The migration adds this function and this trigger and nothing
-- else, so the release before this one runs over the new schema unchanged and the census
-- answers 0.
create trigger issues_open_notify after update of closed_at on issues
  for each row when (old.closed_at is distinct from new.closed_at)
  execute function issues_open_notify();
