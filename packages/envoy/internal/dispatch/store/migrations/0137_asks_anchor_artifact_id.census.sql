-- The rows 0137's index would refuse: an anchored ask whose anchor->>'artifact_id' does not cast
-- to uuid, which fails the build of asks_anchor_artifact_id over every row whose anchor is set.
-- Only a row written before 0137 with block_artifact_id set can be one: asks_anchor_artifact
-- (0032) already casts that text on every row whose block_artifact_id is null, so those are
-- checked, and the ask-block insert (docs/ask_blocks.go), the only writer of block_artifact_id,
-- writes no anchor, so no release writes such a row today — but an earlier release's row is
-- unchecked by either, so the count is taken, not assumed. It calls the cast's own input function,
-- so the two cannot drift apart unnoticed. An anchor carrying no artifact_id casts to null and is
-- refused by nothing; pg_input_is_valid answers null for it, so `not` leaves it uncounted.
select count(*)
from asks
where anchor is not null
    and not pg_input_is_valid(anchor ->> 'artifact_id', 'uuid');
