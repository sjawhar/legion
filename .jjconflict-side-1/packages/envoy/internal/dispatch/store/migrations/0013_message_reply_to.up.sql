-- 0013_message_reply_to.up.sql
alter table messages add column reply_to uuid references messages(id);
create index messages_reply_to on messages (reply_to);
