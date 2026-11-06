-- 20261105201323_design_gate_step.up.sql — the design gate keyed by the step it holds.
--
-- A deployment's design gate becomes a list of steps, and each step names one document of the root
-- issue: 'decisions' its decisions document, 'criteria' its Verification document, 'spec' its
-- primary document. The root registers each step the deployment gates, and the work waits on every
-- one of them, so the record keeps one design_gates row for each step rather than one for each
-- issue. The key is the issue and the step.
--
-- The default is what reads every row stored before this migration as the 'spec' step. The release
-- that wrote those rows gated the issue's primary document and nothing else, and this release
-- resolves `gates.design: root-issues` to the one step 'spec', so the stored row already is that
-- step's row. No design_gates row is rewritten, here or later: a step is a name the daemon holds,
-- not a value a backfill could learn from a row it finds.
--
-- Rolling the image back is what this costs. A daemon of an earlier release can no longer write
-- design_gates on this schema: its PutGate names `on conflict (issue)`, and with the key on
-- (issue, step) no unique index on issue alone is left for Postgres to infer, so the statement is
-- refused before it runs. Its reads still answer every row, because this release resolves
-- root-issues to the one step 'spec' and no deployment gates more than that yet, so the table still
-- holds one row for each issue and that row is the one the older daemon means. Migrations here are
-- forward-only and this one has no down file: a deployment that has to leave this release leaves it
-- forward, and an image rolled back runs with its design gate unwritable until it is rolled on.
--
-- The refused READY goes the same way, through the shape check this migration adds to issues
-- below. An earlier daemon's PutIssue names ready_pending_version and never ready_pending_step:
-- a new refusal writes a version beside a null step, which the check refuses, and a clear nulls
-- the version while the 'spec' this migration backfilled stays, which the check refuses too.
-- Either way the whole fact fails, so a rolled-back daemon can neither refuse a READY nor
-- release one it is already holding, until it is rolled on.
alter table design_gates add column step text not null default 'spec'
  check (step in ('decisions', 'criteria', 'spec'));
alter table design_gates drop constraint design_gates_pkey;
alter table design_gates add primary key (issue, step);

-- issues.ready_pending_step is the step whose approval a READY the gate refused waits on. It sits
-- beside ready_pending_version (0004), the version of that step's document to approve. The merge
-- refusal writes the pair together, from the first required step that is closed, and everything
-- that voids the refused READY clears the pair together.
--
-- The column is null exactly when ready_pending_version is, and the shape check refuses either
-- half on its own. A version with no step would name a document version to approve without saying
-- whose, and a step with no version would leave a name behind that the next refusal would be read
-- against. Every row this migration finds has one of the two states: a refusal stored by the
-- release before this one named a version alone, and that release gated the issue's primary
-- document, so the backfill below gives those rows the step 'spec', which is the step they were
-- waiting on. The pairing is what issues_awaiting_shape in 0057 does for the person-wait's own
-- columns.
alter table issues add column ready_pending_step text
  check (ready_pending_step in ('decisions', 'criteria', 'spec'));
update issues set ready_pending_step = 'spec' where ready_pending_version is not null;
alter table issues add constraint issues_ready_pending_shape check (
  (ready_pending_version is null) = (ready_pending_step is null));
