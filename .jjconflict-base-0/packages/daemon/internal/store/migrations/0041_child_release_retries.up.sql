-- 0041_child_release_retries.up.sql -- bounded retry state for a todo child waiting on its parent.
create table child_release_retries (
    issue text primary key,
    dispatch_seq bigint not null check (dispatch_seq > 0),
    attempts integer not null check (attempts > 0),
    next_at timestamptz not null,
    exhausted boolean not null default false
);
