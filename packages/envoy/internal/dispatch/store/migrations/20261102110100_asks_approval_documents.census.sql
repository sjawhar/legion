-- 20261102110100 fills approval.documents from the four top-level fields of every stored
-- approval ask and then requires the array. 0064's asks_approval_kind_check already refuses
-- every approval row whose artifact_id, name, version and requested_version are not of the shape
-- the rebuilt check holds each document to, so every row this backfills passes it, and a row of
-- another kind keeps approval null, which both checks admit. This counts the approval rows that
-- hold no source value for the array, the negated invariant that check makes: none.
select count(*)
from asks
where kind = 'approval'
  and not (approval ? 'artifact_id' and approval ? 'name' and approval ? 'version' and approval ? 'requested_version');
