-- 0064 drops users: the count is the GitHub OAuth token pairs it deletes, one per GitHub login that
-- signed in. people is a new table and touches no existing row.
select count(*) from users;
