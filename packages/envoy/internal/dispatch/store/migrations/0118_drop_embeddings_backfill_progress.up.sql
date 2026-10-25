-- 0118_drop_embeddings_backfill_progress.up.sql
-- INFRA-393: the backfill-embeddings subcommand is gone; the server's coverage pass
-- (internal/dispatch/embedqueue) embeds everything it holds, items written before the embedding
-- triggers included, with no checkpoint to resume from. embeddings_backfill_progress was that
-- subcommand's checkpoint and nothing else's: no server of any release reads or writes it, only
-- the old person-run subcommand did, so dropping it in the same release breaks no running build.
drop table embeddings_backfill_progress;
