-- 0053_asks_approval_kind_check.up.sql
-- An ask of kind approval names the document version it asks a human to approve, and no other
-- kind names one. The approval-request route is the only writer of the column and always pairs
-- the two, but nothing refused a row that did not, and answering an approval ask that named no
-- document dereferenced a nil approval. The constraint states the pairing the way
-- asks_answer_state_check and asks_resolution_state_check (0007) state theirs, so no reader of
-- an approval ask has to check for a missing document.
--
-- Adding the constraint validates every existing row under an ACCESS EXCLUSIVE lock on asks,
-- which the runner holds until its transaction commits. The lock timeout bounds the wait for that
-- lock, so a migration queued behind a long transaction on asks fails the deploy, naming the
-- lock, instead of holding every read and write of asks queued behind it. SET LOCAL lasts until
-- the runner's transaction ends.
set local lock_timeout = '5s';
alter table asks add constraint asks_approval_kind_check check ((kind = 'approval') = (approval is not null));
