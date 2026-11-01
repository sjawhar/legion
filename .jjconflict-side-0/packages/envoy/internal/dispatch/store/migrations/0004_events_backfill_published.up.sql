-- 0004_events_backfill_published.up.sql
-- Events recorded while the outbox published only notifying rows were never meant to
-- reach NATS. Mark them published so widening the scan does not replay hours of
-- history to every subscriber; consumers that need older events read them over HTTP.
update events set published_at = created_at where not notify and published_at is null;
