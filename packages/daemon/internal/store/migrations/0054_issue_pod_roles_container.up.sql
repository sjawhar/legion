-- 0054_issue_pod_roles_container.up.sql — an issue pod runs every workflow role in one container,
-- `roles`, so a process locator of an issue pod's role names that container (runtime.Locator's
-- Validate refuses any other). A daemon of the layout before it stored each role's locator naming a
-- container of the role's own, the claim's role; this rewrites those to `roles`, in both tables that
-- keep a locator, so the rows load. The controller's pod still runs its one container `controller`,
-- and its locators are left as they are; a tmux locator carries no sandbox member. The pod such a
-- locator names keeps running the previous layout until its next launch, which the runtime makes
-- replace it (sandbox.movedLayout).
update claims
set locator = jsonb_set(locator, '{sandbox,container}', '"roles"')
where locator->>'runtime' = 'sandbox'
  and locator->'sandbox' ? 'container'
  and locator->'sandbox'->>'container' <> 'controller';

update claim_capabilities
set locator = jsonb_set(locator, '{sandbox,container}', '"roles"')
where locator->>'runtime' = 'sandbox'
  and locator->'sandbox' ? 'container'
  and locator->'sandbox'->>'container' <> 'controller';
