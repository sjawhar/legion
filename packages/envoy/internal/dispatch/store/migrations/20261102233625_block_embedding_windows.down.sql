drop function embeddings_enqueue_window(text, text, text, text[]);
drop index embeddings_window_owner;
alter table embeddings drop column covers;
-- The narrow check refuses a window row, so the windows go before it comes back.
delete from embeddings where kind in ('document_block', 'comment_block', 'message_block', 'ask_part');
alter table embeddings drop constraint embeddings_kind_check;
alter table embeddings add constraint embeddings_kind_check
  check (kind in ('issue', 'document', 'comment', 'ask', 'message'));
