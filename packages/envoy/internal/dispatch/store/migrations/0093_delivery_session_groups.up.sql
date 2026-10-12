-- 0093_delivery_session_groups.up.sql
-- LEGION-682: the Delivery page's session groups. A group is a stable id, a name and a set of
-- agent session ids; a pull request whose first Omp-Session trailer names a member shows its
-- group as its Parent agent instead of that session (api/delivery.go). One list serves everyone,
-- so every reader of the page and every measure sees the same grouping.
--
-- A session belongs to at most one group: session_id is the members table's primary key, so a
-- second group naming it is refused by the database too, not only by the route's own check. Names
-- are unique without regard to case, since the page shows two groups that differ only in case as
-- the same label. Deleting a group deletes its members with it, and its sessions show as
-- themselves again.
--
-- Two new tables: nothing existing is locked or rewritten.
create table delivery_session_groups (
  id uuid primary key default gen_random_uuid(),
  name text not null check (btrim(name) <> ''),
  created_by jsonb not null,
  created_at timestamptz not null default now(),
  updated_by jsonb not null,
  updated_at timestamptz not null default now()
);

create unique index delivery_session_groups_name on delivery_session_groups (lower(name));

create table delivery_session_group_members (
  session_id text primary key,
  group_id uuid not null references delivery_session_groups(id) on delete cascade
);

create index delivery_session_group_members_group on delivery_session_group_members (group_id);
