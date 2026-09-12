alter table asks add column block_id text;
alter table asks add column block_artifact_id uuid references artifacts(id);

create unique index asks_block_id_unique
  on asks (block_artifact_id, block_id)
  where block_id is not null;
