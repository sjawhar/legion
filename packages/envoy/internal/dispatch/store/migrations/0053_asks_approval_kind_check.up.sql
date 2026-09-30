-- 0053_asks_approval_kind_check.up.sql
-- An ask of kind approval names the document version it asks a human to approve, and no other
-- kind carries an approval at all. The approval-request route is the only writer of the column
-- and always pairs the two, but nothing refused a row that did not.
--
-- docs.ScanAsk reads SQL null as no approval and any other value as a model.AskApproval, and
-- fails on one that does not decode. SQL null on an approval ask dereferenced a nil approval. The
-- JSON null, which encoding a nil *model.AskApproval writes, reads as an approval of artifact ""
-- at version 0, whose answer failed the uuid cast. A value that does not decode, such as a
-- version of 1.5 or a name of 5, fails every read of the ask, the Inbox that lists it among them,
-- and docs.ApprovalAskAt reads every open approval ask of a document on each version write, so
-- one such row also stops the document taking versions. On an approval ask the check admits only
-- an approval whose three keys hold what the approval-request route writes: artifact_id as
-- Postgres writes a uuid (lowercase hex text, which docs.ApprovalAskAt compares as text), name a
-- string, and version a JSON number written as a whole number from 1 of at most ten digits, which
-- an int holds. So it refuses every approval whose known keys hold the wrong type, which is what a
-- mistaken hand edit writes. It does not guarantee a decode: encoding/json matches keys to fields
-- ignoring case, Unicode folding included, so "Version": 1.5 or "verſion": 1.5 beside "version"
-- still reaches AskApproval.Version, and it fails on a value nested more than 10,000 deep under
-- any key. Refusing every key but the three would close that; dispatch://LEGION-429 holds that
-- check and why it waits on a count of production rows. On every other kind the check admits
-- only SQL null.
--
-- Adding the constraint validates every existing row under an ACCESS EXCLUSIVE lock on asks,
-- which the runner holds until its transaction commits. The lock timeout bounds the wait for that
-- lock, so a migration queued behind a long transaction on asks fails the deploy, naming the
-- lock, instead of holding every read and write of asks queued behind it. SET LOCAL lasts until
-- the runner's transaction ends.
set local lock_timeout = '5s';
alter table asks add constraint asks_approval_kind_check check (
    case
        when kind = 'approval' then coalesce(
            approval ->> 'artifact_id' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                and jsonb_typeof(approval -> 'name') = 'string'
                and jsonb_typeof(approval -> 'version') = 'number'
                and approval ->> 'version' ~ '^[1-9][0-9]{0,9}$',
            false
        )
        else approval is null
    end
);
