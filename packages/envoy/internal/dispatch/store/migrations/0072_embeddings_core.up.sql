-- 0072_embeddings_core.up.sql
-- Meaning search (LEGION-549): one vector per (kind, id) searchable unit - the same grain
-- search.go's keyword legs already key hits by (issue by key, document by artifact id, comment /
-- ask / message by id) - embedded by Cohere after commit through a durable retry queue that
-- mirrors the events outbox (0020_outbox_retry.up.sql): an AFTER INSERT OR UPDATE trigger on
-- each content table (0073-0077, one table and one ACCESS EXCLUSIVE lock per migration, exactly
-- as 0057-0061's search triggers are) upserts this row's current text and a hash of it in the
-- same transaction as the write, so a write never fails because the embedder did. The embedding
-- itself is filled in afterward, out of process, by internal/dispatch/embedqueue, which never
-- runs inside a write's transaction.
--
-- This migration creates only new objects (the extension, embeddings,
-- embeddings_backfill_progress, and embeddings_enqueue, which no trigger calls yet) - it takes no
-- lock on a table any other migration or any live write already depends on.
--
-- embedded_hash lags content_hash until a row is embedded at that exact text; they differ
-- whenever a row is pending (never embedded, or the text changed since it last was), which is
-- also embeddings_pending's predicate. A failed embed attempt never touches either hash - only
-- attempt_count, confirmed_failures and next_attempt_at move - so a row stays pending until it
-- succeeds. attempt_count counts every retry, confirmed or not, and paces next_attempt_at's own
-- backoff; confirmed_failures counts only the attempts internal/dispatch/embedqueue's bisection
-- confirmed as this row's own content failing (some other row of the same batch had already
-- embedded, proving the service was up) - a demoted or throttled retry, which proves nothing
-- about this row specifically, advances attempt_count but never confirmed_failures. dead marks a
-- row that reached deadLetterAttempts confirmed failures, never merely attempt_count ones;
-- embeddings_enqueue clears both counters and dead the moment the row's own text next changes,
-- since a dead row whose content moved on deserves a fresh set of attempts, not to stay excluded
-- by its old failure.
create extension if not exists vector;

create table embeddings (
  kind text not null check (kind in ('issue', 'document', 'comment', 'ask', 'message')),
  id text not null,
  text_snapshot text not null,
  content_hash text not null,
  embedded_hash text,
  model text,
  embedding vector(1536),
  attempt_count integer not null default 0,
  confirmed_failures integer not null default 0,
  next_attempt_at timestamptz not null default now(),
  dead boolean not null default false,
  embedded_at timestamptz,
  created_at timestamptz not null default now(),
  primary key (kind, id)
);

create index embeddings_pending on embeddings (next_attempt_at, kind, id)
  where embedded_hash is distinct from content_hash and not dead;

-- HNSW: pgvector's ANN index for cosine distance. Partial over embedded rows only - a row still
-- pending (embedding is null) has nothing to index, and a predicate here keeps the index from
-- growing with rows meaning search cannot yet rank by.
create index embeddings_cosine on embeddings using hnsw (embedding vector_cosine_ops)
  where embedding is not null;

-- A resumable backfill's per-kind progress (internal/dispatch/embedqueue.Backfill): the primary
-- key of the last row enqueued, so a rerun after an interrupt resumes there instead of
-- rescanning every row this kind already enqueued.
create table embeddings_backfill_progress (
  kind text primary key check (kind in ('issue', 'document', 'comment', 'ask', 'message')),
  last_id text,
  done boolean not null default false,
  rows_enqueued bigint not null default 0,
  updated_at timestamptz not null default now()
);

-- embeddings_enqueue is the one write path onto embeddings: a trigger in each of 0073-0077 for
-- each content table, and internal/dispatch/embedqueue.Backfill for content that predates this
-- migration. content_hash uses sha256(), built into Postgres core since 11 - no pgcrypto
-- dependency. The WHERE on the conflict update means an UPDATE that leaves the tracked text
-- unchanged (an issue's status, say) costs one no-op upsert rather than resetting the row's retry
-- state. It never overwrites an embedded row's vector with a stale re-enqueue of the same text:
-- the WHERE already makes an unchanged-hash upsert a no-op, so attempt_count, next_attempt_at and
-- dead - and, left untouched, embedding and embedded_hash - survive it exactly as backfill's
-- INSERT ... ON CONFLICT DO NOTHING (internal/dispatch/embedqueue) relies on.
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
        dead = false
    where embeddings.content_hash is distinct from excluded.content_hash;
end;
$$;
