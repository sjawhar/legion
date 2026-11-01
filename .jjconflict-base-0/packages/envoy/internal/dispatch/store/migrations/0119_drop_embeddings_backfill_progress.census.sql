-- The drop deletes the old backfill-embeddings subcommand's checkpoints, which only that removed
-- subcommand could resume from: no stored value anything still reads is lost, so nothing here is
-- one to inspect before the deploy.
select 0;
