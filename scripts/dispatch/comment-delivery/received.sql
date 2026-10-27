-- Every Envoy delivery of a person's Dispatch comment that an agent session's transcript holds,
-- from {from} (inclusive) to {to} (exclusive) by the row's time. Read-only; run with `tq sql`
-- after replacing {from} and {to} with UTC times (scripts/dispatch/README.md, "Comment delivery").
--
-- omp stores an Envoy delivery as a messages row with kind 'system' and role
-- 'custom:envoy-message'. Its text is the envelope that renderInbound
-- (packages/envoy-client/src/delivery.ts) encodes as TOON: one key per line, two spaces of indent
-- per level, every string on one line (a newline in a string is the escape \n). A delivery of a
-- comment.created event has, at the envelope's level, `id: <envelope id>` and then the
-- `dispatch:` block, whose lines are indented four spaces or more:
--
--   envoy:
--     to: you (<session>)                      only on the session's own subject
--     from: dispatch
--     at: "2026-10-19T01:31:21Z"
--     id: dispatch-<event id>                  a mention delivery: the id the listener minted
--     re: ...
--     reply_with: ...
--     dispatch:
--       owner: BET-15
--       type: comment.created
--       actor:
--         kind: user
--         id: sami@theorem.dev
--       payload:
--         id: <comment id>
--         ...
--
-- An ask-thread reply has no payload block: the block holds `question:`, `reply:`, `state:` and
-- `waiting_on:` (dispatchCompact), and no comment id. A frame whose event did not pass
-- DispatchEventSchema has the raw event in the block (`id: <event id>`, `type:`, `actor:` and
-- `payload:` at the same levels). This query reads every one of these forms: it keys on the
-- `dispatch:` block's `type: comment.created` line and its actor of kind `user`, and takes the
-- comment id from the payload block's own `id:` line when the block has one.
--
-- Columns: session_id is the base id (the part before `@` of a diverged copy
-- `<id>@<host>:<path hash>`); event_id is the Dispatch event id when the envelope id is
-- `dispatch-<id>` (a frame the outbox published), else empty (a mention delivery); comment_id is
-- empty when the frame names none (an ask-thread reply), and report.ts then ties a mention
-- delivery to its comment by envelope_id (comments.sql's mention_envelope_id). Identical rows
-- (a batch the store loaded twice) are one row; two rows of one source line that differ stay
-- two, and report.ts refuses them.
SELECT DISTINCT
    splitByChar('@', row_session)[1] AS session_id,
    toString(row_host) AS host,
    concat(replaceOne(toString(row_ts), ' ', 'T'), 'Z') AS ts,
    raw_file,
    raw_line,
    extract(text, '(?m)^  id: "?([^"\n]*)"?$') AS envelope_id,
    extract(envelope_id, '^dispatch-([0-9]+)$') AS event_id,
    extract(block, '(?m)^    payload:\n(?:      [^\n]*\n)*?      id: "?([0-9A-Fa-f-]{36})"?$') AS comment_id
FROM (
    SELECT
        session_id AS row_session, host AS row_host, ts AS row_ts, raw_file, raw_line, text,
        -- The dispatch: block: every line after `  dispatch:` that is indented four spaces or more.
        extract(text, '\n  dispatch:\n((?:    [^\n]*(?:\n|$))*)') AS block
    FROM messages
    WHERE kind = 'system'
      AND role = 'custom:envoy-message'
      AND ts >= parseDateTime64BestEffort('{from}', 3, 'UTC')
      AND ts < parseDateTime64BestEffort('{to}', 3, 'UTC')
)
WHERE match(block, '(?m)^    type: "?comment\\.created"?$')
  AND match(block, '(?m)^    actor:\n(?:      [^\n]*\n)*?      kind: "?user"?$')
ORDER BY session_id, raw_file, raw_line, host, ts, envelope_id, comment_id
FORMAT TSVWithNames
