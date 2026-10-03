-- 0069_issues_title_lexemes.up.sql
-- The duplicate-title check reads each stored title's lexemes instead of parsing every title in the
-- project on each creation (LEGION-505).
--
-- The check (duplicateQuery, api/duplicates.go) compares a new title's lexemes with those of every
-- other issue in its project, and it built each title's with search_vector('', search_text(title))
-- as it ran, several times over once Postgres inlined the query's CTEs: in a project of 2,000 titles
-- at the 1,000-character cap that took 1.3-2.5 s for every creation. issues.search cannot stand in
-- for it, since it holds the key's lexemes beside the title's, and a title holding a word of its own
-- key would lose that word were the key's taken out. title_lexemes holds the title's alone,
-- tsvector_to_array(search_vector('', search_text(title))): the lexemes the check built, each once.
-- The issues trigger writes it beside search on every insert and every update of the title, so no
-- application code writes it, a binary older than this one included; like search, it keeps the
-- lexemes the english configuration gave when the row was written.
--
-- A column with a constant default is a catalog change that rewrites and scans no row, so this
-- transaction holds issues' ACCESS EXCLUSIVE lock for milliseconds; replacing the trigger's function
-- keeps the trigger bound to it and locks nothing more. The rows already stored hold the default, {},
-- until 0070 fills them under ROW EXCLUSIVE in a transaction of its own, so no read of issues waits
-- for that. The server applies both before it serves, and an older one never reads the column.
alter table issues add column title_lexemes text[] not null default '{}';

create or replace function issues_search() returns trigger language plpgsql as $$
begin
  new.search := setweight(search_vector(search_text(new.key), search_text(new.title)), 'A');
  new.title_lexemes := tsvector_to_array(search_vector('', search_text(new.title)));
  return new;
end $$;
