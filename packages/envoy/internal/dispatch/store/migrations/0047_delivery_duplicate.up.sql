-- A delivery attempt's idempotency key is now stable across the attempts of one message in one
-- mode, so a retry of a send that already landed is a JetStream duplicate and is never published
-- again (LEGION-271). The listener answers such a send with duplicate: true.

-- duplicate records that the stream already held this message when the attempt was sent, so the
-- recipient gained nothing from it. The attempt is still 'sent': it reached the listener and the
-- message is on the agent's subject. Its envelope_id is null, because the envelope this request
-- minted is the one JetStream discarded.
alter table message_deliveries add column duplicate boolean not null default false;
alter table comment_deliveries add column duplicate boolean not null default false;
