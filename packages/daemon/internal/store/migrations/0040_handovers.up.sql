-- 0040_handovers.up.sql -- a closing root's in-flight child hand-over fence.
--
-- A hand-over PATCH reaches Dispatch outside intake's transaction. The row stays until that
-- daemon-authored event either moves the child to its own tree or is rejected because the root
-- came back, so re-admission and the late PATCH cannot both act on one child.
create table handovers (
    issue text primary key references issues(key) on delete cascade,
    tree text not null,
    linger_generation bigint not null check (linger_generation > 0),
    cancelled boolean not null default false
);

-- A root a hand-over created carries its child's phase, phase rows and pull request into that
-- generation, and its old root's gate when the old root had one. Its architect's catch-up says
-- so: it resumes the tree rather than decomposing it anew, and registers its own spec only when no
-- gate came with it.
create table handed_over_roots (
    issue text primary key references issues(key) on delete cascade,
    generation bigint not null check (generation > 0),
    gate_carried boolean not null
);

-- A carried root holds a copy of its old root's gate, on the old root's document, whose events
-- arrive keyed by the old root; each event is applied to every gate on that document.
create index design_gates_artifact_id on design_gates (artifact_id);
