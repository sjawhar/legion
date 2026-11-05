-- The partial index scans messages while holding the index-build lock.
select count(*) from messages
