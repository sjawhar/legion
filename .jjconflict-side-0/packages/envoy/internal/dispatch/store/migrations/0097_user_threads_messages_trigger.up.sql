-- 0097_user_threads_messages_trigger.up.sql
-- LEGION-687: an issue message thread a person wrote in reaches the Replies page like a comment
-- thread. Attaches user_threads_message (0095) in a migration of its own, so the transaction holds
-- messages' lock alone (0075's precedent).
create trigger user_threads_message after insert on messages
  for each row execute function user_threads_message();
