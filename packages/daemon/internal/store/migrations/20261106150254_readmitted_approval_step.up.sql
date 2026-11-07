-- 20261106150254_readmitted_approval_step.up.sql — the kept approval names the step it is of.
--
-- readmitted_approvals (20261101163106) keeps the one approval a re-admission found below its
-- document's latest version, for the new architect's start task, which tells that architect to
-- request approval of the latest version. The design gate is a list of steps now, each naming one
-- document of the root issue, so the pair is read from the first required step that holds a stale
-- approval and the task must name that step's document: without the step a pair kept from the
-- decisions document sends the architect to the spec at the decisions document's versions.
--
-- The default is what reads every row stored before this migration as the 'spec' step. The release
-- that wrote those rows gated the issue's primary document and nothing else, and this release
-- resolves `gates.design: root-issues` to the one step 'spec', so a stored pair already is that
-- step's. The check admits the three step names, as design_gates.step does (20261105201323), so
-- the database holds the shape rather than the writer alone.
--
-- Rolling the image back costs nothing here: the column has a default, so an earlier daemon's
-- insert, which names no step, still lands. Migrations here are forward-only and this one has no
-- down file.
alter table readmitted_approvals add column step text not null default 'spec'
  check (step in ('decisions', 'criteria', 'spec'));
