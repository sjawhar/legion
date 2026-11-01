-- 0006_ask_threads.up.sql
alter table comments add column ask_id uuid references asks(id);
create index comments_ask_id on comments (ask_id) where ask_id is not null;
