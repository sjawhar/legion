-- 0013_pull_request_planned_red.up.sql — whether the newest head that changed a path outside
-- .legion/ was the review App's (the tester's red tests). A red on it is planned, so the head
-- after it is not a fix attempt; handoff-only heads carry the mark. False for every existing pull
-- request, which counts as before.
alter table pull_requests add column planned_red boolean not null default false;
