-- 0068_people.up.sql
-- AGENTC-1563: people sign in to Dispatch with Google Workspace through the shared sign-in pool
-- and are named by lowercase email.
--
-- `people` is everyone who has signed in: the options the assignee picker offers and the only
-- names an issue may be assigned to. For a person signed in through the pool it also holds the
-- pool's refresh token, which Dispatch renews their membership with, and when it last confirmed
-- that membership; a person recorded any other way holds neither.
--
-- `users` held each GitHub login's GitHub OAuth token pair, for the GitHub proxy that acted as the
-- signed-in person. The proxy reads GitHub as the App now, and no sign-in stores a GitHub token,
-- so the table and its rows are dropped (its census counts them).
create table people (
  email text primary key check (email <> '' and email = lower(email)),
  signed_in_at timestamptz not null default now(),
  refresh_token text,
  confirmed_at timestamptz,
  check ((refresh_token is null) = (confirmed_at is null))
);

drop table users;
