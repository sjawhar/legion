-- 0053_answer_questions.up.sql — every question the Envoy listener handed the daemon for one of its
-- agents that is not running, and what the answer loop spent a day.
--
-- answer_questions: one row per question and the conversation answering it. identity is what makes
-- a later hand-off the same question: dispatch:<message id> or dispatch:<comment id> for a Dispatch
-- origin, the envelope's dedupe key for an Envoy one; a second hand-off of it bumps deliveries and
-- last_seen_at and is not answered again. id is the question's own id (named by the gh tool's
-- answer_tool_calls rows, its footer, its legion-answer marker and the conversation's legion-answer
-- entry). session, token, issue and role are the conversation answering it; kind where it came from
-- (a targeted Dispatch message, a mention, a reply on the agent's ask, an agent's Envoy message);
-- handoff the hand-off as received. state is received -> generating -> appended -> posted, or
-- passed_on once a reply the controller must hear of was posted, forwarded to the agent that runs
-- the conversation (forward_delivered once its role topic took it), or dropped. reply_text is what
-- is posted once the reply is decided (appended), reply_kind the model's kind ('' for the daemon's
-- own reply), pass_reason why the question is passed on ('' for none).
create table answer_questions (
  id text primary key,
  project text not null,
  identity text not null,
  session text not null,
  token text not null,
  issue text not null,
  role text not null,
  kind text not null check (kind in ('message', 'mention', 'ask_reply', 'envoy')),
  may_ask_nothing boolean not null,
  handoff jsonb not null,
  state text not null check (state in ('received', 'generating', 'appended', 'posted', 'forwarded', 'passed_on', 'dropped')),
  reply_kind text not null default '',
  reply_text text not null default '',
  pass_reason text not null default '',
  forward_delivered boolean not null default false,
  deliveries integer not null default 1 check (deliveries > 0),
  received_at timestamptz not null,
  last_seen_at timestamptz not null,
  updated_at timestamptz not null,
  unique (project, identity, session)
);

create index answer_questions_open on answer_questions (project, received_at)
  where state in ('received', 'generating', 'appended') or (state = 'forwarded' and not forward_delivered);

-- answer_spend: the answer loop's model spend in US dollars, by Legion project token and UTC day,
-- checked before an answer is generated and added to after, so a restart keeps the day's total.
create table answer_spend (
  project text not null,
  day date not null,
  usd double precision not null check (usd >= 0),
  primary key (project, day)
);
