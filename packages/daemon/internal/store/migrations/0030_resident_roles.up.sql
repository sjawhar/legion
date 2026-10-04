-- 0030_resident_roles.up.sql — a phase worker stays live from its role's first assignment until
-- its issue closes, so no transition suspends the role whose phase it ends.
--
-- Such a suspend named the phase it ended ("leaves") and finished without acting once the issue was
-- back in a phase its role works. Every row still queued in that shape is deleted: the outbox decodes
-- rows strictly and the field is gone, so the row would fail on every attempt, and read without the
-- field it would be an unconditional stop of a role its issue still needs. A suspend that names no
-- phase ended (an issue's close, a child's leave, a re-entered child's interrupted run) is kept.
delete from outbox where kind = 'supervise' and payload->>'op' = 'suspend' and coalesce(payload->>'leaves', '') <> '';
