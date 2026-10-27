-- INFRA-607: the embedding queue (internal/dispatch/embedqueue) scans when something is pending
-- instead of every 5 seconds. embeddings_enqueue, which every embedding trigger (0073-0077) calls,
-- now announces on dispatch_embeddings each time its upsert writes a row: a new item, or a text
-- whose hash changed. An update that leaves the text as it was writes no row and announces
-- nothing. Each process's shared listener (pglisten.Hub) wakes its queue on the announcement.
--
-- The transactions that reach embeddings_enqueue already notify - an event insert's
-- dispatch_events (0107), or a document rebuild's dispatch_doc_changed - so this adds no
-- notify-queue lock at commit to a transaction that took none; Postgres folds repeats within a
-- transaction into one notification. The one exception is the boot-time restore of decided asks'
-- wordings (docs.RestoreDecidedWordings), which writes no event and runs once per boot, only over
-- rows an earlier release reworded.
--
-- The body is 0118's otherwise unchanged. The release before this one calls the function with
-- the same arguments and never listens on the channel, so it keeps working beside this one.
create or replace function embeddings_enqueue(p_kind text, p_id text, p_text text) returns void
language plpgsql as $$
declare
  v_hash text := encode(sha256(convert_to(p_text, 'UTF8')), 'hex');
begin
  insert into embeddings (kind, id, text_snapshot, content_hash)
  values (p_kind, p_id, p_text, v_hash)
  on conflict (kind, id) do update
    set text_snapshot = excluded.text_snapshot,
        content_hash = excluded.content_hash,
        attempt_count = 0,
        confirmed_failures = 0,
        next_attempt_at = now(),
        dead = false,
        dead_model = null,
        pending_since = now()
    where embeddings.content_hash is distinct from excluded.content_hash;
  if found then
    perform pg_notify('dispatch_embeddings', '');
  end if;
end;
$$;
