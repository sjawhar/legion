-- 0051_answer_tool_calls.up.sql — every gh call the daemon's answer loop runs for a stopped agent.
--
-- A row is written before the call runs and finished after: result is what the model read (gh's
-- output, cut to the answer's bound, or the refusal), exit gh's exit status (null when gh never
-- started), finished_at when it ended. A row with no result is a call whose outcome is unknown,
-- one the daemon stopped during: a restart looks a comment or review it may have posted up on
-- GitHub by the footer's answer key, <question>.<seq>. args are the gh arguments as run, app the
-- App role whose token ran it, owner the repository owner whose gh directory it ran with.
create table answer_tool_calls (
  question text not null,
  seq integer not null check (seq > 0),
  args jsonb not null,
  app text not null,
  owner text not null,
  started_at timestamptz not null,
  result text,
  exit integer,
  finished_at timestamptz,
  primary key (question, seq)
);
