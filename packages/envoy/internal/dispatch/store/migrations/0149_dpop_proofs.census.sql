-- 0149 creates an empty table (dpop_proofs) and its expiry index, and nothing else: no row that
-- exists before it can violate anything it adds, no row is rewritten, and it locks no existing
-- table.
select 0;
