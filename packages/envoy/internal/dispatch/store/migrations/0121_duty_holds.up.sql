-- INFRA-590: one holder per name, a lease a process keeps by renewing it (duty.Hold). The events
-- outbox's single publisher holds "dispatch-outbox" here, and INFRA-558's delivery intakes hold one
-- name per durable. The loop leases (duty.Exclusive, migration 0115) are session advisory locks,
-- which a process holds only while one connection stays open; a publisher holds its role for as
-- long as it runs, across every connection its pool replaces, so the lease is a row instead.
--
-- holder is the process instance that holds name, a uuid it draws at boot. expires_at is when the
-- hold lapses, always now() + the hold's TTL on the database's clock, written by the statement
-- that takes or renews it: two processes' wall clocks differ by whatever their hosts' do, so the
-- database's is the one clock a lapse is judged by. A process takes a name whose row is absent,
-- lapsed (expires_at <= now()) or already its own, in one statement. token is free text the holder
-- records (an intake's deliver subject); the outbox records none.
--
-- Release deletes the row and sends pg_notify('dispatch_duty_holds', name) in the same statement,
-- so a standby listening there takes the name at once rather than at the lapse.
--
-- A new table: nothing existing is read, locked or rewritten here, and the release before this one
-- neither reads nor writes it, which is what a rolling update requires of a migration
-- (cmd/dispatch/README.md, "Database migrations").
create table duty_holds (
    name text primary key,
    holder uuid not null,
    token text not null default '',
    expires_at timestamptz not null
);
