-- The plain-text refresh tokens 0069 forgets: one per person who signed in through the pool before
-- this release. people is 0068_people's, which ships in an earlier release.
select count(*) from people where refresh_token is not null and refresh_token not like 'v1:%'
