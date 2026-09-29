-- 0052_broadcast_recipient_order.up.sql
-- Each message of one broadcast receives the zero-based position it occupied after Dispatch
-- resolved the sender's requested sessions. The broadcast reader can therefore return recipient
-- cards in the order the human named them, rather than falling back to message UUIDs that happen
-- to sort differently.
--
-- The column stays nullable during a rolling deploy. An old Dispatch server can still create
-- broadcast messages while this migration is live, and it does not write a position; readers put
-- those legacy rows after positioned ones and retain their created_at and id fallback order. The
-- original request order was never stored for those rows, so there is nothing to backfill.
alter table messages add column broadcast_position integer;

create unique index messages_broadcast_position on messages (broadcast_id, broadcast_position)
  where broadcast_id is not null and broadcast_position is not null;
