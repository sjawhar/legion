-- 0022_issue_handed_over.up.sql — whether an issue carries the Dispatch label that hands it to Legion.
--
-- Legion admits a root, or an orphan as a root of its own, only while Dispatch shows it carrying the
-- `legion` label (record.LegionLabel), so the record keeps what the last observation of the issue
-- said. An issue recorded before this column counts as not handed over until the next observation
-- of it, from the stream or from the boot read, which records every issue in the workflow's
-- statuses; a tree already running keeps its slot either way.
alter table issues add column handed_over boolean not null default false;
