-- One partial index on comment_deliveries: the build reads every row of the table, which the census
-- report counts and holds to its size limit, and refuses and rewrites none.
select 0
