-- 0064_pull_request_pass_settlement.up.sql — a pull request's CI settlement may be one the daemon's
-- two-minute pass built from its own read of a head's check runs, for a head no settlement stood
-- for (classify.PassGeneration): it is recorded at generation -1, below every generation Envoy's
-- listener publishes (0 and up), and the listener's later settlement of the same head is merged
-- into it check by check.
alter table pull_requests drop constraint pull_requests_generation_check;
alter table pull_requests add constraint pull_requests_generation_check check (generation >= -1);
