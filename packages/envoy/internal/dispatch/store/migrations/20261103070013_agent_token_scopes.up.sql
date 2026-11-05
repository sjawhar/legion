-- 20261103070013_agent_token_scopes.up.sql — the OAuth scopes a personal agent token holds.
--
-- INFRA-1017: Dispatch stops refusing every bearer token on the routes only a browser could call,
-- and two scopes decide instead: `person`, "this credential acts as its person", and `approve`,
-- the right to settle what a person settles. A personal agent token carries its own set, so the
-- column is where that set lives.
--
-- The default is in the column, and not left to the insert, because Dispatch rolls: the release
-- before this one is still serving against this schema while the new one boots, and its mint
-- names four columns (`insert into agent_tokens (owner, name, token_hash, prefix)`,
-- api/agent_tokens.go). A column with no default would refuse that insert, and the store's rule
-- is that a migration must leave the release before it able to read and write the schema
-- (internal/dispatch/store/README.md). With the default here that release keeps minting tokens,
-- each holding the whole set, which is exactly the authority its tokens have today.
--
-- The check constraint makes "a token cannot hold `approve` without `person`" a fact of the
-- database and not only of the handler that mints one: direct SQL, a later release's insert and a
-- repair by hand are all held to it, so no row can grant the right to settle a person's decisions
-- to a credential that does not act as that person. `<@` holds the set to the two scopes Dispatch
-- knows, so a misspelt one is refused where it is written rather than read as nothing later.
--
-- Adding a column with a constant default writes the catalog alone (Postgres 11 on), so it holds
-- agent_tokens' ACCESS EXCLUSIVE lock for milliseconds and rewrites no row; the check is on the
-- new column alone, whose every existing row takes the default, so it refuses none either. Its
-- identifier is the UTC moment it was written, as `migrationid` requires of every migration past
-- the numbered series, which closed at 0161 (cmd/dispatch/README.md, "Database migrations").
alter table agent_tokens
  add column scopes text[] not null default array['person','approve']
  check (scopes <@ array['person','approve']
         and ('approve' <> all(scopes) or 'person' = any(scopes)));
