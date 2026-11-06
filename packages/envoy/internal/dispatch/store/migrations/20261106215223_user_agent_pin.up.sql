-- 20261106215223_user_agent_pin.up.sql
-- The agent conversations a person pinned. A pin is what puts a session in the sidebar's Agents
-- list, so the few agents a person is working with stay in front of them on every page and on
-- every device they sign in from, which is what the browser kept per device until now.
--
-- A table of its own rather than a column on user_agent_state, for two reasons, both of which a
-- column would have to break. That table's cleared_before is not null, so a session a person
-- pinned and never cleared would have no row to carry the pin, and a Dispatch that predates this
-- change scans every row's cleared_before as a timestamp. And user_agent_state.login is the raw
-- actor id (migration 0033), while this login is canonical (lower case), like user_agent_read's
-- (0051) and user_ask_snooze's (0048), so one person holds one set of pins however their identity
-- source spells their login.
--
-- Per (login, session_id) like user_agent_read, so the pin follows the person across their
-- devices. pinned_at is the moment they pinned it, which is the order the list is drawn in,
-- oldest first; unpinning deletes the row, so the table holds what is pinned now and no history.
--
-- A new table: nothing existing is read, locked or rewritten, its census answers 0, and the
-- release before this one neither reads nor writes it, so it rolls beside this one
-- (cmd/dispatch/README.md, "Database migrations"). Its identifier is the UTC moment it was
-- written, as `migrationid` requires of every migration past the numbered series, which closed at
-- 0161.
create table user_agent_pin (
  login text not null,
  session_id text not null,
  pinned_at timestamptz not null,
  primary key (login, session_id)
);
