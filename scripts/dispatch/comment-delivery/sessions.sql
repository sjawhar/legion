-- Every agent session the transcript store holds that started before {to}, one row per session.
-- Read-only; run with `tq sql` after replacing {from} and {to} with UTC times
-- (scripts/dispatch/README.md, "Comment delivery"). {from} is not read: a session that ended
-- before the window is still a session the store holds, so report.ts calls it "not running"
-- when a comment was sent to it, not "unknown".
--
-- A session is the base id: the part of session_id before `@`. The store imports a diverged copy
-- of a session file as a session of its own, `<id>@<host>:<path hash>`; each copy is the same
-- agent session, so the copies merge into one row: every host, sorted and joined with `,`; the
-- earliest started_at; the latest ended_at, or empty when a copy has none (it is still being
-- written). parent_session is the base id of the session this one was forked from, empty when it
-- was not forked; when copies name different parents, each is listed, sorted and joined with `;`.
-- A subagent's parent_session names the session that started it, which is another agent with its
-- own Envoy identity, so a subagent (role other than `main`) gets no parent here.
SELECT
    sid AS session_id,
    arrayStringConcat(arraySort(groupUniqArray(row_host)), ',') AS host,
    concat(replaceOne(toString(min(row_started)), ' ', 'T'), 'Z') AS started_at,
    if(
        countIf(row_ended IS NULL) > 0,
        '',
        concat(replaceOne(toString(max(row_ended)), ' ', 'T'), 'Z')
    ) AS ended_at,
    arrayStringConcat(arraySort(groupUniqArrayIf(row_parent, row_parent != '')), ';') AS parent_session
FROM (
    SELECT
        splitByChar('@', session_id)[1] AS sid,
        toString(host) AS row_host,
        started_at AS row_started,
        ended_at AS row_ended,
        if(
            ifNull(role, 'main') = 'main',
            splitByChar('@', ifNull(parent_session, ''))[1],
            ''
        ) AS row_parent_any,
        if(row_parent_any = sid, '', row_parent_any) AS row_parent
    FROM sessions_latest
)
GROUP BY sid
HAVING min(row_started) < parseDateTime64BestEffort('{to}', 3, 'UTC')
ORDER BY session_id
FORMAT TSVWithNames
