-- Two new tables: no stored row can violate them.
--
-- 20261101162254 creates decision_keeps and user_document_reads and nothing else: it rewrites no row, locks
-- no existing table, and the release before it neither reads nor writes either table.
select 0;
