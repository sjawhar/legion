-- 0062_supervise_quiesce.up.sql — the `quiesce` key of a supervise start payload goes. A start no
-- longer takes a phase over from the role still at work in it: when CI goes red the daemon starts
-- the implementer at once, as at every other phase change, and the role whose phase ended keeps its
-- turn, its jobs, its process and its session. The payload decodes strictly
-- (record.DecodeOutboxPayload) and record.SuperviseRequest no longer has the field, so a start the
-- previous release queued with it would fail to decode on every attempt, and its issue's lifecycle
-- lane would stall on a row no retry can finish. This strips the key from every row still queued,
-- which leaves each row the start it already was; one written without it is untouched, as 0051 did
-- for an earlier field.
update outbox set payload = payload - 'quiesce' where kind = 'supervise' and payload ? 'quiesce';
