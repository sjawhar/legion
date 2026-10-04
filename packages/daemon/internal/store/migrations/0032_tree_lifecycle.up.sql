-- 0032_tree_lifecycle.up.sql - each tree's durable admission and cleanup barrier. Claims, starts
-- and resources bind the open epoch; a cleanup reservation refuses them until API-confirmed
-- release, and only a fresh root admission opens the next epoch. Keyed by the normalized project
-- token claims and runtime labels use (claim.ProjectToken), not the issue row's Dispatch key.
create table tree_lifecycles (
    project text not null,
    tree text not null,
    epoch bigint not null check (epoch > 0),
    authority text not null check (authority in ('workflow', 'operator')),
    cleanup_started boolean not null default false,
    cleanup_confirmed_at timestamptz,
    updated_at timestamptz not null default now(),
    primary key (project, tree)
);

alter table claims add column tree_epoch bigint not null default 0 check (tree_epoch >= 0);

-- Trees admitted before the barrier existed stay runnable at epoch one: workflow roots from their
-- issue rows, operator trees from the claims no workflow issue backs. Their claims bind it.
insert into tree_lifecycles (project, tree, epoch, authority)
select distinct regexp_replace(lower(i.project), '[^a-z0-9]', '', 'g'), i.key, 1, 'workflow'
from issues i
where i.key = i.tree
on conflict do nothing;

insert into tree_lifecycles (project, tree, epoch, authority)
select distinct c.project, c.tree, 1, 'operator'
from claims c
where not exists (select 1 from issues i where i.key = c.tree)
on conflict do nothing;

update claims c set tree_epoch = 1
where exists (select 1 from tree_lifecycles l where l.project = c.project and l.tree = c.tree);
