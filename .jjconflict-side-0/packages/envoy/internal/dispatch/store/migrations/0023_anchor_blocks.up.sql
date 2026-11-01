create index comments_anchor_block_id_idx
  on comments ((anchor->>'artifact_id'), (anchor->>'block_id'))
  where anchor->>'block_id' is not null;

create index asks_anchor_block_id_idx
  on asks ((anchor->>'artifact_id'), (anchor->>'block_id'))
  where anchor->>'block_id' is not null;
