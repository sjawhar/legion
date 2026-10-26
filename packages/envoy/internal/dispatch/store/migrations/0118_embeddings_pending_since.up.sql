-- 0118_embeddings_pending_since.up.sql
-- INFRA-393: the server keeps meaning search's coverage itself (internal/dispatch/embedqueue's
-- coverage pass), and its coverage report says how long the oldest item has waited for a current
-- vector. Two columns serve that:
--
-- pending_since is when a row last became pending: its text changed (embeddings_enqueue below),
-- the coverage pass repaired it, or the pass dropped a vector another model or place made. A row
-- already pending keeps its value, so a retry never resets the age the report shows. A row that is
-- pending at this migration takes its own created_at, the closest record of when it began waiting.
--
-- dead_model is the embedder identity (embed.Embedder.Identity) under which a row was confirmed
-- dead. A row dead under another model or place gets this place's attempts once (the coverage
-- pass re-admits it); a row dead under the configured identity stays dead until its text changes,
-- or, when its text has nothing to embed, until the pass settles it with no vector.
--
-- Both are additive with a default, so the release before this one keeps reading and writing the
-- table: it inserts only through embeddings_enqueue, replaced below, or with an insert that names
-- neither column (the default fills pending_since), and it never reads either.
alter table embeddings
  add column pending_since timestamptz not null default now(),
  add column dead_model text;

update embeddings set pending_since = created_at
  where embedded_hash is distinct from content_hash;

-- Every row already dead here died under us.cohere.embed-v4:0 on Bedrock (embed.BedrockModel), the
-- identity every release before this one embedded with, so it records that identity: a deployment
-- that keeps Bedrock leaves these rows dead rather than spending its attempts on them again, and a
-- switch to Azure still re-admits each of them once, as for any row dead under another identity.
-- During the roll, a row the release before this one confirms dead after this update records no
-- identity, so this release's coverage pass gives it this identity's attempts once.
update embeddings set dead_model = 'us.cohere.embed-v4:0'
  where dead;

-- embeddings_enqueue as 0072 wrote it, and on a text change it also starts the row's wait again
-- and forgets the identity it died under: a row whose content moved on deserves a fresh set of
-- attempts wherever it is embedded.
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
end;
$$;
