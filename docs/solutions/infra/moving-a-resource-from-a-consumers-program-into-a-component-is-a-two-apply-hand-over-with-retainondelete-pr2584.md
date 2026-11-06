---
title: "Moving a resource from a consumer's program into a component is a two-apply hand-over with retainOnDelete"
category: infra
tags:
  - pulumi
  - component
  - migration
  - state
  - envoy-infra
date: 2026-10-31
status: active
module: packages/envoy/infra
related_issues:
  - "sjawhar/legion#2584"
---

# Moving a resource from a consumer's program into a component is a two-apply hand-over with retainOnDelete

A component that starts declaring a resource its consumers used to declare, and whose name it used
to take as an input, creates a window in which one live resource has two owners. Write the
hand-over in the operator document, in this order, and never as one apply:

1. **On the old pin, set `retainOnDelete: true` on the consumer's own declaration and apply.**
   Where the consumer protected it, set `protect: false` **in the program** in the same step: every
   apply writes the program's `protect` back to state, so a `pulumi state unprotect` run beforehand
   does not survive the apply, and the apply that removes a still-protected resource is refused
   even with the retain.
2. **In the next apply, remove the declaration and the input and pass the component its provider.**
   The component registers the resource with no consumer dependency, adopts the live one, and the
   consumer's retained delete drops its own state entry and leaves the resource alone.

- **`pulumi state delete` on the consumer's resource is not the route**, however naturally it comes
  to mind. It is refused for exactly the stacks that need it: a stack that passed the resource's
  output into the component has the component depending on it, since the SDK puts the input's
  dependency in the Construct request's `Dependencies` and that becomes the component's own
  `DependsOn`, and `pulumi state delete` refuses a resource something in state depends on. The
  `--target-dependents` the CLI then suggests removes the component and every child of it from
  state.
- **Say in the document whether the provider's create is an upsert.** Where it is, the first apply
  does not fail on a live resource: it adopts it and overwrites every field the component sets. A
  breaking change that cannot fail is the dangerous kind, because nothing stops an operator who
  has not read the step.
- **Put the same sentence in the changelog fragment**, since that is what a consumer reads when it
  moves the pin, not the component's README.

## Evidence

sjawhar/legion#2584 moved Dispatch's Tailscale Service `svc:<hostname>` from the consumer's program
into `envoy-infra`'s `gcp:Dispatch`, removing the `tailnetService` input.

- The procedure is `packages/envoy/infra/README.md`, "Moving a stack that declares the Service
  itself", and the fragment `packages/envoy/.changes/unreleased/pr2584-Changed-20261031-011500.yaml`.
- The upsert is `VIPServices().CreateOrUpdate` in terraform-provider-tailscale v0.29.2's
  `tailscale/resource_service.go`, which the reviewer read; the first apply therefore adopts a live
  `svc:<hostname>` and overwrites its comment, ports and tags.
- The refusal is pulumi's `pkg/resource/edit/operations.go`, reproduced by the reviewer on
  v3.268.0 against a file backend: `can't be safely deleted because the following resources depend
  on it: * "dispatch" … Delete those resources first or pass --target-dependents.`
- The `protect: false`-in-the-program half came from pulumi's `step_generator.go`: the first
  version of the step said to run `pulumi state unprotect` before the apply, which the next apply
  undoes. Three review rounds were spent on this one step, the first two on routes that do not work.
