-- 0122_issue_embedding_spec.up.sql
-- INFRA-568: an issue's vector is its title and its spec, not its title alone, so the related-item
-- check, which embeds a new issue's title and spec, compares like with like, and an issue whose
-- title is terse is still found by what its spec says. The spec is the markdown of the latest
-- version of the issue's primary document, raw, ask-block markup included, as 0074 embeds every
-- document, so an issue's vector and its spec's stay comparable.
--
-- Two triggers enqueue the issue's text: 0073's on issues, still on an insert or a title update,
-- whose function this migration replaces, and a new one on artifact_versions for a new version of
-- an issue's primary document (a spec edit). Replacing a function's body locks no table, so the
-- migration takes one table's lock, artifact_versions', for its trigger, as 0073-0077 each do.
--
-- internal/dispatch/embedqueue's coverage pass builds the same text (coverageSources), so once an
-- embedder runs it re-queues every existing issue, whose stored text is its title alone.

-- The text an issue embeds: its title alone when the spec is null or holds only white space (an
-- issue created without a spec has an empty primary document), else the title, a blank line and
-- the spec. White space is embedqueue's nothingToEmbed test.
create function issue_embedding_text(title text, spec text) returns text
language sql immutable parallel safe as $$
  select case when spec is null or spec !~ '[^[:space:]]' then title else title || E'\n\n' || spec end
$$;

-- The markdown of the latest version of the issue's primary document, null when it has none.
create function issue_embedding_spec(p_issue_key text) returns text
language sql stable as $$
  select v.markdown
  from artifacts a join artifact_versions v on v.artifact_id = a.id
  where a.issue_key = p_issue_key and a.is_primary
  order by v.number desc
  limit 1
$$;

create or replace function embeddings_enqueue_issue() returns trigger language plpgsql as $$
begin
  perform embeddings_enqueue('issue', new.key, issue_embedding_text(new.title, issue_embedding_spec(new.key)));
  return new;
end;
$$;

-- artifacts.is_primary is set when the artifact is created and never changed, so a new version of
-- a primary document is the only write besides the issue's own that changes the issue's text.
create function embeddings_enqueue_issue_spec() returns trigger language plpgsql as $$
declare
  v_key text;
  v_title text;
begin
  select i.key, i.title into v_key, v_title
  from artifacts a join issues i on i.key = a.issue_key
  where a.id = new.artifact_id and a.is_primary;
  if found then
    perform embeddings_enqueue('issue', v_key, issue_embedding_text(v_title, issue_embedding_spec(v_key)));
  end if;
  return new;
end;
$$;
create trigger artifact_versions_issue_embeddings_enqueue after insert on artifact_versions
  for each row execute function embeddings_enqueue_issue_spec();
