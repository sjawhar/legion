-- 0070 adds issues.title_lexemes with a constant default, a catalog change that rewrites and scans
-- no row, creates title_lexemes(title), and replaces the issues trigger's function, which locks
-- nothing. It refuses no row; 0071 owns the backfill.
select 0;
