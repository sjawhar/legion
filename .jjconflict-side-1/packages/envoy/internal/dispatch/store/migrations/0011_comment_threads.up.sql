-- 0011_comment_threads.up.sql
alter table comments
  add column resolved_by jsonb,
  add column resolved_at timestamptz,
  add column edited_at timestamptz;
