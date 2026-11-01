-- loadOwnerComments filters issue-owned comments by issue_key and orders them by creation.
create index comments_issue_created
  on comments (issue_key, created_at, id)
  where issue_key is not null;

-- loadReplyChain joins reply_to (uuid) to the recursive row id (uuid). Keep
-- comments_reply_to_text: graph_edges projects the relationship as text.
create index comments_reply_to
  on comments (reply_to)
  where reply_to is not null;
