-- 0003_events_unpublished_all.up.sql
drop index events_unpublished;
create index events_unpublished on events (id) where published_at is null;
