-- 0053_asks_approval_kind_check.up.sql
-- An ask of kind approval names the document version it asks a human to approve, and no other
-- kind carries an approval at all. The approval-request route is the only writer of the column
-- and always pairs the two, but nothing refused a row that did not, and answering an approval ask
-- that named no document failed. An SQL null dereferenced a nil approval. docs.ScanAsk decodes
-- every other object, and the JSON null that encoding a nil *model.AskApproval writes, into a
-- non-nil approval, so one without the document's id read back as an approval of artifact "" and
-- failed the uuid cast its readers make. The check therefore requires what those readers use: the
-- document's id as Postgres writes a uuid (lowercase hex text, which the open-ask lookup compares
-- as text) and a numeric version on an approval ask, and SQL null on every other kind, so no
-- reader of an approval ask has to check for a missing document.
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
				and jsonb_typeof(approval -> 'version') = 'number',
			false
		)
		else approval is null
	end
);
