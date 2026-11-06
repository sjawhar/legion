---
title: "A preview dry-runs every create, so a property naming a resource that preview creates comes from its ID"
category: infra
tags:
  - pulumi
  - pulumi-kubernetes
  - rbac
  - preview
date: 2026-11-02
status: active
module: packages/envoy/infra/gcp
related_issues:
  - "sjawhar/legion#2668"
---

# A preview dry-runs every create, so a property naming a resource that preview creates comes from its ID

- A `pulumi preview` of a Kubernetes create is a real write-shaped request: pulumi-kubernetes sends
  the API server a server-side dry run, and the preview fails on what that request is refused for.
  Read the refusal as the API server's, not as a Pulumi error.
- Build a property that names another resource of the same stack from the referent's `ID()`, never
  from a literal and never from its `metadata.name` alone. On a preview that creates the referent
  only the ID is unknown; a name the program set comes back known, so it suppresses nothing.
  One unknown input makes the provider skip the dry run and return the inputs as they are.
- `pulumi.DependsOn` does not fix such a preview. Step order does not make an input unknown, and
  the skip is decided by the inputs alone.
- The provider already absorbs some preview refusals and not others. It falls back to a
  client-side preview on `Forbidden`, on a dry run the server has disabled, on its own preview
  error and, under server-side apply, on `AlreadyExists`. It does **not** absorb `NotFound`, which
  is what the API server answers when it cannot read the thing your property names. So the
  unknown is the remedy for a reference, where a plain permission gap needs none.
- Keep the apply value byte-identical to what the literal held. A reference is often the
  resource's force-new property, so a value that resolves differently replaces every live object
  rather than updating it. Pin that value in a test of the apply path, beside the preview case.
- The same unknown also orders the apply: taking the name from the referent's ID is a data
  dependency, where shared resource options alone leave the engine free to create the referring
  object first and meet the same refusal for real.

## Evidence

sjawhar/legion#2668 fixed `envoy-infra`'s `gcp:Dispatch`, whose front door declares a Role and a
RoleBinding `<hostname>-tailnet-tls`. The binding named its Role with a literal `roleRef.name`, so
a preview sent the API server a dry run of the binding. The deploy account holds no `bind` verb in
that namespace, so the API server checked escalation by reading the referenced Role, which the
same preview had not created yet, and answered `NotFound`; the preview failed. It had stopped a
production cutover twice before the component was fixed, and the workaround on the consumer's side
was a `bind` grant on that one Role.

Read at the pins. `packages/envoy/infra/go.mod:22` pins `pulumi-kubernetes/sdk/v4 v4.34.2`:

- `provider/pkg/provider/provider.go:1728` — `skipPreview := hasComputedValue(newInputs) || …`,
  and line 1733 returns `Id: "", Properties: req.GetProperties()` for a preview that meets it. An
  unknown input is the one criterion a program controls.
- `provider.go:1799-1818` — the preview fallbacks: `apierrors.IsForbidden`,
  `isDryRunDisabledError`, `await.PreviewError`, and `IsAlreadyExists` under server-side apply.
  There is no `IsNotFound`.
- `provider.go:1889-1893` — `id := ""; if !req.GetPreview() { id = fqObjName(initialized) }`:
  every preview create answers with an empty ID, which is what leaves `role.ID()` unknown in the
  real engine and not only under a test harness.
- `provider/pkg/provider/diff.go:192-193` — `var roleBinding = properties{".roleRef"}`, the
  RoleBinding's one force-new property.

Kubernetes answers that `NotFound` from its RBAC storage: a create of a RoleBinding short-circuits
on the `bind` verb, and without it reads the referenced Role to confirm no escalation, returning
the read's own error unchanged. Only the escalation check itself answers `Forbidden`.

The fix is `packages/envoy/infra/gcp/dispatch_frontdoor.go:556`,
`pulumi.All(role.ID(), role.Metadata.Name().Elem()).ApplyT(…)`, which is the form
theorem-labs/agent-infra's `shared/rbac` `ref` already used. `packages/envoy/infra/gcp/dispatch_test.go`
pins the apply value (`roleRef` equals `{apiGroup, kind: Role, name: example-dispatch-tailnet-tls}`)
and asserts the preview value is unknown on both the component and the plugin's `Construct`
surface.

[INFERENCE] The ordering point and the `DependsOn` point are read off the pinned provider source
and the component's resource options, not from an observed failure.
