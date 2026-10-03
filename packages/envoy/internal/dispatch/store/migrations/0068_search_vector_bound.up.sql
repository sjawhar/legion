-- 0068_search_vector_bound.up.sql
-- No write fails because its search vector would pass Postgres's limit on one tsvector (LEGION-505).
--
-- Postgres refuses a tsvector whose lexemes and positions take more than 1,048,575 bytes
-- (SQLSTATE 54000, "string is too long for tsvector"; src/backend/tsearch/to_tsany.c,
-- make_tsvector, MAXSTRPOS). A distinct word costs the vector its own bytes and four or five more,
-- so text made of words no two alike passes the limit inside 1 MiB: past 699,048 bytes of
-- `w000001 w000002 …` and past 475,217 bytes of UUIDs. Every write of such text failed in its
-- table's search trigger (0057-0061) - a document's version, so every settlement of the document,
-- an issue's title, an ask an ask block indexes - while prose stays far below it, its words
-- repeating (the first mebibyte of this repository's markdown makes 194,870 bytes).
--
-- search_vector(head, body) is the vector the triggers built, to_tsvector('english', head) ||
-- to_tsvector('english', body), and where that would pass the limit it is built from the longest
-- of body's first half, quarter, eighth, ... that fits, so the row is still found by the words that
-- open it; the head, an issue's key, is always whole. Where the whole fits, which is every row the
-- triggers ever stored, it is that vector exactly, so no stored row changes and none is re-indexed.
-- issues_search weights the one vector A where it weighted the key's and the title's apiece and
-- joined them, which is the same vector: each of this repository's 77,925 markdown lines of 1 to
-- 1,999 characters and eight edge shapes, as a title under six keys, gives the same vector both
-- ways.
--
-- Measured on Postgres 16.15 at load 60-70: a mebibyte of prose takes 0.12-0.17 s either way. A
-- mebibyte that does not fit pays the parse that failed and the halves it tries: distinct words
-- 0.12-0.14 s, keeping the first half; UUIDs 0.40 s and twenty-part hyphenated words 0.27-0.30 s,
-- each keeping the first quarter. A short text pays the function call and its exception block,
-- about 5.5 us (100,000 titles: 0.44-0.49 s through to_tsvector, 1.00-1.08 s through
-- search_vector), and the duplicate check of a project of 2,000 issues took 54-57 ms against 42-56
-- ms before. The function is parallel unsafe, Postgres's default: a parallel query, in its workers
-- and its leader alike, refuses the subtransaction the exception block opens ("cannot start
-- subtransactions during a parallel operation"), so no query that calls it may run in parallel.
--
-- The triggers keep search_text (0056) and the ask-block strip, and only their last step changes;
-- create or replace keeps each trigger bound to its function, so this takes no lock on any table.
-- The duplicate check (api/duplicates.go) builds title lexemes through search_vector too, so a title
-- stored under the bound never fails the duplicate check of the issues created after it.
create function search_vector(head text, body text) returns tsvector
language plpgsql immutable strict as $$
declare
  kept integer := length(body);
begin
  loop
    begin
      return to_tsvector('english', head) || to_tsvector('english', left(body, kept));
    exception when program_limit_exceeded then
      if kept = 0 then
        raise;
      end if;
      kept := kept / 2;
    end;
  end loop;
end $$;

create or replace function issues_search() returns trigger language plpgsql as $$
begin
  new.search := setweight(search_vector(search_text(new.key), search_text(new.title)), 'A');
  return new;
end $$;

create or replace function artifact_versions_search() returns trigger language plpgsql as $$
begin
  new.search := search_vector('', regexp_replace(search_text(new.markdown), '(?s):::ask\{.*?:::', '', 'g'));
  return new;
end $$;

create or replace function comments_search() returns trigger language plpgsql as $$
begin
  new.search := search_vector('', search_text(new.body));
  return new;
end $$;

create or replace function asks_search() returns trigger language plpgsql as $$
begin
  new.search := search_vector('', search_text(new.question || ' ' || ask_search_suffix(new.options, new.answer)));
  return new;
end $$;

create or replace function messages_search() returns trigger language plpgsql as $$
begin
  new.search := search_vector('', search_text(new.body));
  return new;
end $$;
