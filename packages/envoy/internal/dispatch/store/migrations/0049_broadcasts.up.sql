-- 0049_broadcasts.up.sql
-- One message a human sent to many sessions at once. A broadcast is a grouping, not a new
-- delivery mechanism: each recipient gets an ordinary issue-less targeted message of its own,
-- carrying this id, so every recipient's thread, retry and reply behave exactly as they do for
-- a message sent from one agent card. The row keeps what the recipients share - who sent it,
-- the body they were all sent, the delivery mode chosen for all of them, and when - so the
-- broadcast view can name the send without reading one recipient's copy and calling it the
-- original.
--
-- Recipients are the messages that point here; there is no membership table. A session that
-- was excluded before sending (it does not advertise the chosen mode, or it was no longer
-- live) never receives a message and so is not a recipient: the exclusion is reported to the
-- sender in the create response and has no life after that request.
create table broadcasts (
  id uuid primary key default gen_random_uuid(),
  author jsonb not null,
  body text not null,
  delivery text not null,
  created_at timestamptz not null default now()
);

alter table messages add column broadcast_id uuid references broadcasts(id);

create index messages_broadcast on messages (broadcast_id) where broadcast_id is not null;
