-- 20261106143638_catch_up_gates.up.sql — the `gate` key of a catch-up notice becomes `gates`. The
-- design gate is a list of steps now, each naming one document of the root issue, so a catch-up
-- states one entry for each step the root must hold open rather than one gate carrying the
-- deployment's policy word. The payload decodes strictly (record.DecodeOutboxPayload) and
-- record.CatchUp no longer has the field, so a catch-up the previous release queued would fail to
-- decode on every attempt: its architect would hear nothing of its tree until a later launch's
-- drop deleted the row, and its project's board lane would log that failure at every sweep and
-- release no waiting notice behind it.
--
-- This rewrites the key rather than deleting those catch-ups, as 0051 and 0065 rewrote the
-- payloads of other kinds. A catch-up is the architect's first instruction at a launch and
-- nothing else wakes it, so a deleted one leaves that architect with no instruction until its
-- claim ends and a new launch writes another; and a catch-up's delivery is what settles the
-- decisions it claims, so deleting the row would unclaim them (parked_decisions.claimed_by is
-- `on delete set null`) with nothing left that names them again.
--
-- The one entry it writes is the gate the old payload states, read as this release reads a stored
-- gate row: the step is spec, the one step `root-issues` resolves to; `gated` is true under that
-- policy and false under `off`, which gates no step; a payload that carries no artifact is a root
-- whose architect has still to register its gate; and the old `open` said the latest version was
-- the approved one, so an open entry's approved version is that version. A row written without
-- the key is untouched.
update outbox
set payload = jsonb_set(payload, '{catch_up}', ((payload -> 'catch_up') - 'gate') ||
  jsonb_build_object('gates', jsonb_build_array(jsonb_build_object(
    'step', 'spec',
    'gated', coalesce(payload #>> '{catch_up,gate,policy}', '') = 'root-issues',
    'unregistered', not (payload #> '{catch_up,gate}' ? 'artifact'),
    'artifact', coalesce(payload #> '{catch_up,gate,artifact}', '""'::jsonb),
    'version', coalesce(payload #> '{catch_up,gate,version}', '0'::jsonb),
    'approved', case when coalesce((payload #>> '{catch_up,gate,open}')::boolean, false)
      then coalesce(payload #> '{catch_up,gate,version}', '0'::jsonb) else '0'::jsonb end,
    'open', coalesce(payload #> '{catch_up,gate,open}', 'false'::jsonb)))))
where kind = 'notice' and payload #> '{catch_up}' ? 'gate';
