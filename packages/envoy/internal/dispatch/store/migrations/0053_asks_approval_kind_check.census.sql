-- The rows 0053's check would refuse: an approval ask whose approval object is not the route's
-- shape, or an ask of any other kind carrying an approval. The check's own predicate, negated,
-- so the two cannot drift apart unnoticed. The case never answers null (each branch is a
-- boolean that is never null), so `not` counts exactly the rows the check refuses.
select count(*)
from asks
where not (
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
