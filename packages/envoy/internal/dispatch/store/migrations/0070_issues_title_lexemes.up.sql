-- 0070_issues_title_lexemes.up.sql
-- The duplicate-title check reads each stored title's lexemes instead of parsing every title in the
-- project on each creation (LEGION-505).
--
-- The check (duplicateQuery, api/duplicates.go) compares a new title's lexemes with those of every
-- other issue in its project, and it built each title's from the title as it ran, several times
-- over once Postgres inlined the query's CTEs: in a project of 2,000 titles at the 1,000-character
-- cap that took 1.3-2.5 s for every creation. issues.search cannot stand in for it, since it holds
-- the key's lexemes beside the title's, and a title holding a word of its own key would lose that
-- word were the key's taken out. title_lexemes(title) is the one definition of a title's lexemes,
-- the lexemes of its search_vector (0069) with an empty head, each once: the issues trigger stores
-- it in issues.title_lexemes on every insert and every update of the title, 0071 for every issue
-- stored before, and the check builds the new title's with it. No application code writes the
-- column, a binary older than this one included; like search, it keeps the lexemes the english
-- configuration gave when the row was written.
--
-- The trigger builds search with search_vector too, so no title past Postgres's limit on one
-- tsvector fails its write. It weights the one vector A where it weighted the key's and the title's
-- apiece and joined them, which is the same vector: each of this repository's 77,925 markdown lines
-- of 1 to 1,999 characters and eight edge shapes, as a title under six keys, gives the same vector
-- both ways.
--
-- A column with a constant default is a catalog change that rewrites and scans no row, so this
-- transaction holds issues' ACCESS EXCLUSIVE lock for milliseconds; replacing the trigger's function
-- keeps the trigger bound to it and locks nothing more. The rows already stored hold the default, {},
-- until 0071 fills them in a transaction of its own, so this lock is not held while that runs. The
-- server applies both before it serves, and an older one never reads the column.
alter table issues add column title_lexemes text[] not null default '{}';

create function title_lexemes(title text) returns text[]
language sql immutable as $$
  select tsvector_to_array(search_vector('', search_text(title)))
$$;

create or replace function issues_search() returns trigger language plpgsql as $$
begin
  new.search := setweight(search_vector(search_text(new.key), search_text(new.title)), 'A');
  new.title_lexemes := title_lexemes(new.title);
  return new;
end $$;
