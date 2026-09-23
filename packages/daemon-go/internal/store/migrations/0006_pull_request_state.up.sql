-- 0006_pull_request_state.up.sql — whether the issue's pull request is open, merged, or closed, so
-- a re-admitted generation keeps an open pull request and drops only a finished one.
alter table pull_requests add column state text not null default 'open' check (state in ('open', 'merged', 'closed'));
