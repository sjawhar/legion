-- 0056_search_text.up.sql
-- Search indexing in time linear in the text (LEGION-465), part 1 of 7: the normaliser.
--
-- Postgres's text-search parser tries to read a host name wherever letters or digits meet an
-- underscore, and gives up only at the end of the whitespace-free run it is in, so a run such as
-- `a_a_a_…` is scanned again from every letter in it: to_tsvector took 5 s at 25 KB, 21 s at 50 KB
-- and 84 s at 100 KB of `a_` (Postgres 16.15; src/backend/tsearch/wparser_def.c, TPS_InHost).
-- search_text breaks such a run with a space after every sixteenth underscore-joined segment,
-- which bounds the rescans; a word run split at an underscore gives the same lexemes, since the
-- parser reads `_` between words as a blank. Of 3,336 real documents (the repository's markdown
-- and every production document), none changes its vector.
--
-- 0057-0061 give each table's search column to a trigger that applies it, one table per migration
-- so one transaction holds one table's ACCESS EXCLUSIVE lock, for the milliseconds its catalog
-- change takes; 0062 re-indexes the rows whose vector it could change. The function is STRICT, as
-- every function in the expressions it joins is.
create function search_text(body text) returns text
language sql immutable parallel safe strict as $$
  select regexp_replace(body, '((?:[A-Za-z0-9.-]+_){16})', '\1 ', 'g')
$$;
