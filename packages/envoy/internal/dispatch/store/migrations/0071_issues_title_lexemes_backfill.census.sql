-- 0071 writes issues.title_lexemes, which 0070 adds in the same release, on every issue from that
-- issue's own title: it refuses no row and changes no value any reader had before it, so there is
-- nothing to inspect before the deploy. It cannot name the column, which does not exist yet when the
-- census is taken.
select 0;
