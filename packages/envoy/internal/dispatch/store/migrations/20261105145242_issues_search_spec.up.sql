-- 20261105145242_issues_search_spec.up.sql
-- An issue's keyword vector carries the text of its primary document, not its key and its title
-- alone, so an issue whose title holds none of the words its spec uses is still found by them.
-- 0140 made the issue's embedding its title and its spec for the same reason; this is the keyword
-- half of it, and it changes no embedding text and no embedding trigger.
--
-- issue_search_vector(key, title, spec) is the one definition of the vector: the key and the title
-- at weight A, exactly the vector 0088's expression wrote, joined with the spec's lexemes at
-- weight C.
--
-- C and not B, because ts_rank_cd's default weights are A 1.0, B 0.4, C 0.2 (api/search.go ranks
-- every keyword arm with ts_rank_cd), and a spec is hundreds of times longer than the title it
-- belongs to: it matches far more queries than the issue's own name does, so a weight a fifth of
-- A keeps every key or title match above every spec-only match, where B, two fifths, leaves less
-- room for the lexeme count and the proximity ts_rank_cd scores on. The spec's own words are
-- already searched at full strength on the document arm (20261101182447's document_search), which
-- answers with the document itself; what this weight ranks is the weaker claim that the issue is
-- about them, and B stays free for a field that claims more than a spec's body does.
--
-- The spec is the markdown of the latest version of the issue's primary document, the text 0140's
-- issue_embedding_spec reads, with 0087's ask-block strip over it, so a keyword search reads an
-- issue's spec exactly as artifact_versions_search reads that same document on the document arm,
-- and one definition serves "the issue's spec" for both the keyword vector and the embedding.
--
-- 0087's bound is kept, and kept over the whole vector rather than over each half: the key and the
-- title go through search_vector, which bounds them as before, and the spec's half is halved until
-- the joined vector fits Postgres's 1,048,575-byte limit on one tsvector (program_limit_exceeded,
-- 54000), so no write fails because an issue's spec is large. Each cut falls between two words, as
-- 0087's does. At kept = 0 the spec contributes nothing and the result is the vector 0088 would
-- have written, which already fit, so the loop ends.
--
-- The re-index writes search directly rather than `update issues set title = title`, 0062's form.
-- The trigger that fills search reads an update of key or title (0057), and so does 0073's
-- embedding enqueue, so a no-op write of the title would queue every issue in the database for
-- re-embedding, which this change has no reason to pay for. A direct write of search is seen by
-- 20261101182522's issues_search_term_queue, the one trigger that must see it: those vectors did
-- change, and the related-terms refresh has to count them again.
--
-- Only the issues whose primary document holds text are written. For every other issue the new
-- expression returns the vector the row already holds, so the where clause is what keeps the
-- update from rewriting those rows and queueing a term count for nothing; it reads each issue's
-- latest primary version under ACCESS SHARE, which no write waits behind, and the statement takes
-- issues in ROW EXCLUSIVE and a row lock on each issue it rewrites until the commit. The
-- migration that follows this one keeps the vector current from here on, and a version written in
-- the moment between the two commits is taken by the next write of its issue's document.
create function issue_search_vector(p_key text, p_title text, p_spec text) returns tsvector
language plpgsql immutable as $$
declare
  head tsvector := setweight(search_vector(search_text(p_key), search_text(p_title)), 'A');
  body text := regexp_replace(search_text(coalesce(p_spec, '')), '(?s):::ask\{.*?:::', '', 'g');
  kept integer := length(body);
  part text := body;
begin
  loop
    begin
      return head || setweight(to_tsvector('english', part), 'C');
    exception when program_limit_exceeded then
      if kept = 0 then
        raise;
      end if;
      kept := kept / 2;
      -- One character past the cut, so a word that runs past it is dropped whole (0087).
      part := regexp_replace(left(body, kept + 1), '\S*$', '');
    end;
  end loop;
end $$;

create or replace function issues_search() returns trigger language plpgsql as $$
begin
  new.search := issue_search_vector(new.key, new.title, issue_embedding_spec(new.key));
  new.title_lexemes := title_lexemes(new.title);
  return new;
end $$;

update issues set search = issue_search_vector(key, title, issue_embedding_spec(key))
 where issue_embedding_spec(key) ~ '[^[:space:]]';
