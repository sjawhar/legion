---
title: "A command with an implicit default is proven by varying each source of the default"
category: testing
tags:
  - measurement
  - kubectl
  - kubernetes
  - issue-pod
  - evidence
date: 2026-10-25
status: active
module: docs/kubernetes.md
related_issues:
  - "INFRA-372"
  - "sjawhar/legion#2190"
---

# A command with an implicit default is proven by varying each source of the default

Extends docs/solutions/testing/a-command-that-cannot-fail-cannot-tell-you-which-half-answered-INFRA-69.md.

That document is about an explicit `a || b` fallback. This one is about a fallback inside the
client: a command that takes a namespace, context or config from the environment when no flag
names it. A `yes` where the default happens to equal the target proves nothing about how the
default is chosen.

- **Find the default's sources in the client's own source before you write the mechanism.** For
  `kubectl` in a pod with no kubeconfig, client-go's in-cluster `Namespace()`
  (`tools/clientcmd/client_config.go`) reads `POD_NAMESPACE`, else the
  `/var/run/secrets/kubernetes.io/serviceaccount/namespace` file, else `default`.
- **Run the command four ways:** without the flag; with the flag naming the target; with the flag
  naming the other namespace the reader might assume (here, the pod's own); and without the flag
  while each source you can set names that other namespace (`POD_NAMESPACE=<other>`). Only the
  last run separates "the flag-less form is right" from "the flag-less form happens to be right in
  this pod".
- **Write the mechanism the varied runs show, not the one a ruling or message states.** A
  mechanism handed to you is a claim, as INFRA-69's last bullet says of a path. Here it was wrong,
  though its conclusion was right.
- **Say which source you read and did not run.** A fallback you could not trigger (a mounted file
  you cannot remove) is cited from source, at the source's version, and the proof says so.

## Evidence

The ruling that widened INFRA-372 said the section's three `kubectl auth can-i` gate commands, run
without `-n`, "ask about the pod's own namespace". Measured on 2026-10-25 from a role container in
the `legion` namespace, with `POD_NAMESPACE` unset:

| Form | create jobs | create services | get services |
| --- | --- | --- | --- |
| no `-n` | yes | yes | yes |
| `-n <workload namespace>` | yes | yes | yes |
| `-n legion` (the pod's own) | no | no | no |
| `POD_NAMESPACE=legion`, no `-n` | no | no | no |
| `POD_NAMESPACE=legion`, `-n <workload namespace>` | yes | yes | yes |

The flag-less form answered for the workload's namespace because the deployment mounts a
`namespace` file from a ConfigMap naming it, next to the token. In a pod without that file, or
with `POD_NAMESPACE` set, the same command answers about another namespace. Two roles in
differently mounted pods had read opposite answers from it, and one fetched a database by hand as a
result. The page now writes `-n <namespace>` and states the measured order. The `default` step
was cited from client-go v0.35.0 for kubectl v1.35.9, because the mounted file could not be
removed from inside the pod; nothing measured it.
