-- 0150_comment_fallback_copies.up.sql
--
-- The copies of a new comment the outbox held back for a mention no answer has come for within a
-- claim lease (internal/dispatch/outbox/mentions.go): one row per (comment, destination topic)
-- the outbox did not publish the comment.created event to because that mention's send is still
-- to come - the delivery sweep resends an attempt no sender holds, and a live sender's lane
-- sends one it has held queued past the lease - which the outbox's fallback pass publishes once
-- that send fails, and settles with nothing sent once the mention was sent. event_id names the
-- comment.created event whose envelope the copy is.
-- Exactly one of session_id and target is the key whose mention attempts decide the copy: the
-- session an attempt counts under, or the target as written for a route the comment suppressed by
-- that string alone. settled_at is when the fallback pass decided the row, published whether it
-- published the copy then; next_attempt_at is when the row is next due: the claim one pass holds
-- on the row while it decides it, so a second process skips it, the backoff after a failed
-- decision or publish, or the wait a pass released it for without counting an attempt - the
-- mention's send is still undecided (mentionPendingDelay), or NATS was unreachable
-- (retryInterval); a pass cut short by its process stopping releases it with no wait at all, so
-- whatever process runs next takes the row at once. attempt_count is how many of the pass's
-- decisions or publishes of the row failed, which paces that backoff (retry.Delay) as an event's
-- attempt_count paces its own. A failure never settles the row.
--
-- The partial index holds the unsettled rows alone, in the order the pass reads them: a pass with
-- nothing to decide reads an empty index, and a settled row leaves it.
--
-- One new table and its index: nothing existing is read, locked or rewritten here, and the release
-- before this one neither reads nor writes the table, which is what a rolling update requires of a
-- migration (cmd/dispatch/README.md, "Database migrations").
create table comment_fallback_copies (
    comment_id uuid not null,
    destination text not null,
    event_id bigint not null,
    session_id text,
    target text,
    created_at timestamptz not null default now(),
    next_attempt_at timestamptz,
    attempt_count integer not null default 0,
    settled_at timestamptz,
    published boolean not null default false,
    primary key (comment_id, destination),
    check ((session_id is null) <> (target is null))
);

create index comment_fallback_copies_unsettled on comment_fallback_copies (created_at) where settled_at is null;
