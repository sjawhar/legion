-- 20261105152945_going_requests.up.sql — one answer run and the frames of its stream.
--
-- INFRA-1279: a reader asks a question (POST) and reads its answer (GET), two HTTP requests a
-- deployment of more than one pod spreads across pods (cmd/dispatch/conn_age.go: kube-proxy and
-- the front door spread load per connection, not per request). So a run's state and its frames
-- are rows rather than process memory, and any pod answers any reader's stream from them
-- (api/going.go).
--
-- going_requests is the question, one row per POST:
--
-- - id is the row's own uuid, the run the stream route names.
-- - author is the asking actor, as handoffs.author and messages.author are.
-- - project is the project the question was asked under, the empty string for every project, as
--   search's own project filter reads it (api/search.go).
-- - words is the question, which the route caps at contracts.GoingWordsMax.
-- - created_at is when the POST wrote the row, and ends_at when the run is over:
--   created_at + contracts.GoingRunWindow, written by that POST. The window belongs to the record
--   and not to the process running the answer, which is why it is a column and not a timer: a pod
--   that dies mid-run stores no terminal frame, and a reader on another pod ends the stream
--   itself once now() has passed ends_at, as does the sweep for a run nobody reads
--   (api.sweepGoingRequests).
--
-- going_events is the stream, one row per frame, in sequence from 1:
--
-- - request_id is a foreign key to going_requests, on delete cascade: the sweep deletes a run
--   past contracts.GoingRequestTTL and its frames go with it.
-- - sequence is the frame's place in the stream, the Last-Event-ID a reader resumes after. Unique
--   within the request, so no two frames share one.
-- - name is one of the contracts.GoingEvent names and data the frame's payload, the JSON the
--   stream writes as the frame's data (packages/envoy/going/events.go).
--
-- going_events_terminal is what settles a run: at the deadline the running answer, a reader and
-- the sweep may each try to end the run, and the table decides which one does rather than
-- whichever is first. It is unique over the request for a terminal event name, the two of
-- contracts.GoingEventEnd ('end') and contracts.GoingEventError ('error'), whose literals the
-- index holds as the search kinds are held in api/search.go. Every terminal write is an insert
-- that does nothing on a conflict, and the writer that loses reads the stored terminal frame and
-- reports that one.
--
-- Two new tables no earlier release names: nothing existing is read or rewritten, and the release
-- before this one never names them, so it rolls (cmd/dispatch/README.md, "Database migrations")
-- and the census answers 0. going_events' foreign key holds going_requests, created by this same
-- migration, in SHARE ROW EXCLUSIVE while the table is created. The identifier is the UTC moment
-- this was written, as `migrationid` requires of every migration past the numbered series.
create table going_requests (
  id uuid primary key default gen_random_uuid(),
  author jsonb not null,
  project text not null,
  words text not null,
  created_at timestamptz not null default now(),
  ends_at timestamptz not null
);

create table going_events (
  request_id uuid not null references going_requests(id) on delete cascade,
  sequence integer not null check (sequence > 0),
  name text not null,
  data jsonb not null,
  created_at timestamptz not null default now(),
  unique (request_id, sequence)
);

create unique index going_events_terminal on going_events (request_id) where name in ('end', 'error');
