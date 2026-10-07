-- 0032_controller_claim.up.sql — the project controller's claim, which the daemon holds when it
-- launches the controller itself (`controller: daemon`): on the role `controller`, with no issue and
-- no tree. The role check is one list, so it names every role; the shape check holds the controller
-- role and the empty issue and tree together, so neither a controller claim on an issue nor a
-- workflow claim on none is ever stored.
alter table claims drop constraint claims_role_check;
alter table claims add constraint claims_role_check check (role in (
  'architect', 'planner', 'implementer', 'tester', 'reviewer', 'merger', 'controller'));
alter table claims add constraint claims_controller_shape check (
  (role = 'controller') = (issue = '' and tree = ''));
