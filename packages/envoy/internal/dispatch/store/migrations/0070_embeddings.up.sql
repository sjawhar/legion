-- 0070_embeddings.up.sql
-- Meaning search (LEGION-549): one vector per (kind, id) searchable unit - the same grain
-- search.go's keyword legs already key hits by (issue by key, document by artifact id, comment /
-- ask / message by id) - embedded by Cohere after commit through a durable retry queue that
-- mirrors the events outbox (0020_outbox_retry.up.sql): an AFTER INSERT OR UPDATE trigger on
-- each content table upserts this row's current text and a hash of it in the same transaction as
-- the write, so a write never fails because the embedder did. The embedding itself is filled in
-- afterward, out of process, by internal/dispatch/embedqueue, which never runs inside a write's
-- transaction.
--
-- embedded_hash lags content_hash until a row is embedded at that exact text; they differ
-- whenever a row is pending (never embedded, or the text changed since it last was), which is
-- also embeddings_pending's predicate. A failed embed attempt never touches either hash - only
-- attempt_count and next_attempt_at move - so a row stays pending until it succeeds.
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
  next_attempt_at timestamptz not null default now(),
  embedded_at timestamptz,
  created_at timestamptz not null default now(),
  primary key (kind, id)
);

create index embeddings_pending on embeddings (next_attempt_at, kind, id)
  where embedded_hash is distinct from content_hash;

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

-- embeddings_enqueue is the one write path onto embeddings: a trigger below for each content
-- table, and internal/dispatch/embedqueue.Backfill for content that predates this migration.
-- content_hash uses sha256(), built into Postgres core since 11 - no pgcrypto dependency.
-- The WHERE on the conflict update means an UPDATE that leaves the tracked text unchanged (an
-- issue's status, say) costs one no-op upsert rather than resetting the row's retry state.
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
        next_attempt_at = now()
    where embeddings.content_hash is distinct from excluded.content_hash;
end;
$$;

create or replace function embeddings_enqueue_issue() returns trigger language plpgsql as $$
begin
  perform embeddings_enqueue('issue', new.key, new.title);
  return new;
end;
$$;
create trigger issues_embeddings_enqueue after insert or update of title on issues
  for each row execute function embeddings_enqueue_issue();

-- artifact_versions is insert-only (immutable versions - model.go "Version"); each new version
-- re-embeds the artifact it belongs to, at its own id, matching the keyword leg's "only the
-- latest version is searched" (search.go's document leg). An image or file version carries no
-- markdown and enqueues nothing.
create or replace function embeddings_enqueue_document() returns trigger language plpgsql as $$
begin
  if new.markdown is not null then
    perform embeddings_enqueue('document', new.artifact_id::text, new.markdown);
  end if;
  return new;
end;
$$;
create trigger artifact_versions_embeddings_enqueue after insert on artifact_versions
  for each row execute function embeddings_enqueue_document();

create or replace function embeddings_enqueue_comment() returns trigger language plpgsql as $$
begin
  perform embeddings_enqueue('comment', new.id::text, new.body);
  return new;
end;
$$;
create trigger comments_embeddings_enqueue after insert or update of body on comments
  for each row execute function embeddings_enqueue_comment();

-- Mirrors search.go's ask leg text (question, then the answer's text) - not the raw options
-- array, which is option labels rather than meaningful prose.
create or replace function embeddings_enqueue_ask() returns trigger language plpgsql as $$
begin
  perform embeddings_enqueue(
    'ask', new.id::text,
    new.question || ' ' || coalesce(new.answer->>'text', '')
  );
  return new;
end;
$$;
create trigger asks_embeddings_enqueue after insert or update of question, answer on asks
  for each row execute function embeddings_enqueue_ask();

create or replace function embeddings_enqueue_message() returns trigger language plpgsql as $$
begin
  perform embeddings_enqueue('message', new.id::text, new.body);
  return new;
end;
$$;
create trigger messages_embeddings_enqueue after insert on messages
  for each row execute function embeddings_enqueue_message();
