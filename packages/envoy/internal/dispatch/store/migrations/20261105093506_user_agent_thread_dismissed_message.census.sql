-- 20261105093506 creates one index and nothing else: no row that exists before it can violate
-- anything it adds, no row is rewritten, and the SHARE lock it takes on
-- user_agent_thread_dismissed is released with the migration's own transaction.
select 0;
