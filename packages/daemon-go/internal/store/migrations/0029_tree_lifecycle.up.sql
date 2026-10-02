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
