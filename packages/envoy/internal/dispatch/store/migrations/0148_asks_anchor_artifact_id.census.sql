-- 0148 refuses no row and rewrites none: its index expression guards the uuid cast with a regex,
-- so it answers null rather than raising on an anchor whose artifact_id is not a document id, and
-- the build succeeds over any data. There is therefore nothing for this census to count, and a
-- count would be the wrong instrument: any non-zero census refuses the deploy, and the rows left
-- out of the index are not a reason to refuse one. Each boot logs their count with a warning
-- instead (store.AsksOutsideAnchorArtifactIndex, cmd/dispatch/main.go).
select 0;
