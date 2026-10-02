-- 0056_asks_approval_requested_version.up.sql
-- An approval request follows a document's current version while requested_version records the
-- version the agent last handed to a human. Every pre-0056 approval ask was shown at its stored
-- version, so the backfill preserves that fact before the stricter shape takes effect.
--
-- 0053 already guarantees the remaining fields on every approval row. Rebuilding the check after
-- the backfill makes every approval ask decodable by model.AskApproval and preserves the stored
-- ordering invariant that a request can never name a version after the document's current one.
alter table asks drop constraint asks_approval_kind_check;

update asks
set approval = jsonb_set(approval, '{requested_version}', approval -> 'version')
where kind = 'approval';

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
                end,
            false
        )
        else approval is null
    end
);

-- A rolling deploy can have an older binary insert the three-field approval JSON after this
-- migration committed. Normalize that row before the check runs, preserving the version it showed.
create function asks_default_approval_requested_version() returns trigger language plpgsql as $$
begin
    if new.kind = 'approval' and not (new.approval ? 'requested_version') then
        new.approval := jsonb_set(new.approval, '{requested_version}', new.approval -> 'version');
    end if;
    return new;
end;
$$;

create trigger asks_default_approval_requested_version
before insert on asks
for each row execute function asks_default_approval_requested_version();
