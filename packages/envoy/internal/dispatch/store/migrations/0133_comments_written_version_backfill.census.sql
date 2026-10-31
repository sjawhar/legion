-- 0133 writes comments.written_version, which 0130 adds in the same release, on the comments stored
-- before it: it refuses no row and changes no value any reader had before it, so there is nothing to
-- inspect before the deploy. It cannot name the column, which does not exist yet when the census is
-- taken.
select 0
