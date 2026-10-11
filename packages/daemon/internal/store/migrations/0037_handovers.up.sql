-- 0037_handovers.up.sql -- a closing root's in-flight child hand-over fence.
--
-- A hand-over PATCH reaches Dispatch outside intake's transaction. The row stays until that
-- daemon-authored event either moves the child to its own tree or is rejected because the root
-- came back, so re-admission and the late PATCH cannot both act on one child.
create table handovers (
    issue text primary key references issues(key) on delete cascade,
    tree text not null,
    linger_generation bigint not null check (linger_generation > 0),
    cancelled boolean not null default false
);
