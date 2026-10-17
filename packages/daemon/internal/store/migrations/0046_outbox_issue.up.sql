-- 0046_outbox_issue.up.sql — an index on the outbox by issue (LEGION-694). Each issue's rows now
-- run on two lanes of their own, and every lane read — the issue's due rows, when it is next due, the
-- ordering rules' look at an issue's older rows, the role runs the state report reads — is by issue;
-- outbox_due, on (next_at, id), serves none of them. The id follows so a lane's rows come in queue
-- order from the index.
create index outbox_issue on outbox (issue, id);
