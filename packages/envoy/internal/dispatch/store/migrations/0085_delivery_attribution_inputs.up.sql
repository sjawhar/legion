-- What a pull request names that the issue attribution reads (delivery/attribution.go), stored
-- once from GitHub so every reconcile pass resolves its issue from the database alone: the issue
-- keys its title and body name, the GitHub issues its body cites (full URLs, body order), the keys
-- its head branch names and the keys its commit messages name (commit order). Null until the
-- row's GitHub facts have been read for them: the reconcile's backfill fetches each such row once.
-- attribution_checked_at is when a reconcile pass last resolved the row's issue from them: each
-- pass resolves the rows checked longest ago, a batch at a time.
alter table delivery_pull_requests
  add column attribution_title_keys text[],
  add column attribution_cited_issues text[],
  add column attribution_branch_keys text[],
  add column attribution_commit_keys text[],
  add column attribution_checked_at timestamptz,
  add constraint delivery_pull_requests_attribution_inputs check (
    num_nulls(attribution_title_keys, attribution_cited_issues, attribution_branch_keys, attribution_commit_keys) in (0, 4)
  );
create index delivery_pull_requests_attribution_unread on delivery_pull_requests (merged_at desc)
  where attribution_title_keys is null and not partial and unfetchable_at is null;
create index delivery_pull_requests_attribution_walk on delivery_pull_requests (attribution_checked_at nulls first, repo, number)
  where attribution_title_keys is not null;
