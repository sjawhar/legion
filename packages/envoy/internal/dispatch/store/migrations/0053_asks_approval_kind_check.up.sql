-- 0053_asks_approval_kind_check.up.sql
-- An ask of kind approval names the document version it asks a human to approve, and every other
-- kind carries no approval. The approval-request route is the only writer of the column and
-- always pairs the two; this check makes every row do so.
--
-- docs.ScanAsk reads SQL null as no approval and any other value as a model.AskApproval, and
-- fails on one that does not decode. On an approval ask the check admits only an approval whose
-- three keys hold what the route writes: artifact_id as Postgres writes a uuid (lowercase hex
-- text, which docs.ApprovalAskAt compares as text and answering the ask casts to uuid), name a
-- string, and version a JSON number written as a whole number from 1 of at most ten digits, which
-- an int holds. A missing key, the JSON null and a key of the wrong type all fail it. Any of them
-- would otherwise reach the ask's readers: an id not written that way fails the cast or is never
-- found, and a value that does not decode fails every read of the ask, every Inbox listing it and
-- every version write on its document, since docs.ApprovalAskAt reads each open approval ask of
-- the document then.
--
-- The check tests the three exact keys, and encoding/json matches keys to fields ignoring case,
-- Unicode folding included, so it guarantees neither a decode nor that the version decoded is the
-- one it tested. "Version": 1.5 or "verſion": 1.5 beside "version" still reaches
-- AskApproval.Version and fails to decode, as does a value nested more than 10,000 deep under any
-- key. "verſion": 2 beside "version": 3 decodes without an error as version 2, since jsonb stores
-- the longer key after "version" and the decoder keeps the last match. Refusing every key but the
-- three closes all of these; dispatch://LEGION-429 holds that check and why it waits on a count
-- of production rows.
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
