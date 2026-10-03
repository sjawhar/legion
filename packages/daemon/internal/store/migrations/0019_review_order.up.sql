-- 0019_review_order.up.sql - a pull request's reviews are ordered by when they were submitted,
-- then by GitHub's review id, since a draft keeps the id it was created with. The newest deciding
-- review belongs to the pull request, not to a review round, so it moves off the reviewer's phase
-- row, and the decision a round keeps no longer carries its id.
alter table pull_requests add column review_seen bigint not null default 0;
alter table pull_requests add column review_seen_at timestamptz not null default '0001-01-01 00:00:00+00';
update pull_requests set review_seen = phases.review_seen from phases
  where phases.issue = pull_requests.issue and phases.role = 'reviewer';
alter table phases drop column review_seen;
update phases set decision = decision - 'id' where decision is not null;
