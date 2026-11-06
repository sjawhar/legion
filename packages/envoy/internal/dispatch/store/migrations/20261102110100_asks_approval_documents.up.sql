-- 20261102110100_asks_approval_documents.up.sql
--
-- One approval request can now name more than one document: a spec and its issue's Verification
-- document are approved together, in one Inbox card, and the answer writes a review on each.
-- asks.approval grows a documents array for it, one entry per document the request names:
--   artifact_id        the document.
--   name               its name as the question spells it.
--   version            the version the request names now; a later settled version of any of them
--                      moves the request to it, as one document's version moved it before.
--   requested_version  the version of that document its agent last handed to a human; below
--                      version, the request waits on the agent (api/ask_rows.go, approvalMoved).
-- The first entry is the document the request was opened on, and approval.artifact_id, name,
-- version and requested_version go on holding its four values: the release before this one reads
-- the request as the one-document request it has always read, and every index, join and query of
-- that release goes on finding it by its first document. A request that names two documents
-- reaches that release as a request on the first alone; it is the release this one replaces, and
-- only this one opens such a request.
--
-- The backfill writes the array every stored approval already implies, its one document. The
-- normalizing trigger writes it for a row the release before this one inserts or rewrites during
-- a rolling deploy, which carries the four top-level fields and no array - 0064's trigger for
-- requested_version, whose name it sorts after so that the value it defaults is the one copied
-- here, and before asks_written_version. So every approval row carries the array, which the
-- rebuilt asks_approval_kind_check requires: an array of at least one document, each with the
-- fields and bounds the top-level four have always been held to, its first naming the same
-- document as approval.artifact_id.
--
-- asks_open_approvals (0014) indexes the first document alone and stays as it is for the release
-- that reads it. asks_open_approval_documents indexes every document of an open request, so a
-- read by any of them - the open request on a document, and the request a new version moves -
-- seeks instead of scanning the open approvals.
--
-- Every statement locks asks alone, in this transaction; building the index takes SHARE, so ask
-- writes wait for it. It refuses no stored row (the census says why) and rewrites only the
-- approval blobs it fills in.
update asks
set approval = jsonb_set(approval, '{documents}', jsonb_build_array(jsonb_build_object(
    'artifact_id', approval -> 'artifact_id',
    'name', approval -> 'name',
    'version', approval -> 'version',
    'requested_version', approval -> 'requested_version')))
where kind = 'approval' and not (approval ? 'documents');

create function asks_normalize_approval_documents() returns trigger language plpgsql as $$
begin
    if new.kind = 'approval' and new.approval is not null and not (new.approval ? 'documents') then
        new.approval := jsonb_set(new.approval, '{documents}', jsonb_build_array(jsonb_build_object(
            'artifact_id', new.approval -> 'artifact_id',
            'name', new.approval -> 'name',
            'version', new.approval -> 'version',
            'requested_version', new.approval -> 'requested_version')));
    end if;
    return new;
end;
$$;

create trigger asks_normalize_approval_documents
before insert or update of approval on asks
for each row execute function asks_normalize_approval_documents();

alter table asks drop constraint asks_approval_kind_check;

alter table asks add constraint asks_approval_kind_check check (
    case
        when kind = 'approval' then coalesce(
            approval ->> 'artifact_id' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                and jsonb_typeof(approval -> 'name') = 'string'
                and case
                    when jsonb_typeof(approval -> 'version') = 'number'
                        and approval ->> 'version' ~ '^[1-9][0-9]{0,9}$'
                        and jsonb_typeof(approval -> 'requested_version') = 'number'
                        and approval ->> 'requested_version' ~ '^[1-9][0-9]{0,9}$'
                    then (approval ->> 'requested_version')::bigint <= (approval ->> 'version')::bigint
                    else false
                end
                and jsonb_typeof(approval -> 'documents') = 'array'
                and jsonb_array_length(approval -> 'documents') >= 1
                and approval -> 'documents' -> 0 ->> 'artifact_id' = approval ->> 'artifact_id'
                and not jsonb_path_exists(approval, '$.documents[*] ? (!(@.artifact_id.type() == "string" && @.artifact_id like_regex "^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$" && @.name.type() == "string" && @.version.type() == "number" && @.version >= 1 && @.version <= 9999999999 && @.version.floor() == @.version && @.requested_version.type() == "number" && @.requested_version >= 1 && @.requested_version.floor() == @.requested_version && @.requested_version <= @.version))'),
            false
        )
        else approval is null
    end
);

create index asks_open_approval_documents on asks
    using gin (jsonb_path_query_array(approval, '$.documents[*].artifact_id') jsonb_ops)
    where kind = 'approval' and state = 'open';
