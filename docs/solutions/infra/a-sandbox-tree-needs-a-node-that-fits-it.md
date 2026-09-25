---
title: "A Sandbox tree's pods share the first pod's node, so the node's size comes from the pool's floor, never from a resource request on a later pod"
category: infra
tags:
  - kubernetes-runtime
  - agent-sandbox
  - karpenter
  - scheduling
  - production
date: 2026-09-24
status: active
module: packages/daemon-go/internal/runtime/sandbox
related_issues:
  - "LEGION-208"
symptoms:
  - "a worker pod of a tree stays Pending with `1 Too many pods` and Karpenter's `unsatisfiable topology constraint for pod affinity, key=kubernetes.io/hostname`"
  - "a root pod that requests CPU stays Pending with `Insufficient cpu` on the node a child of its tree was placed on"
---

# A Sandbox tree needs a node that fits it

## Why every pod of a tree lands on one node

The tree volume is a single-node EBS volume that every pod of the tree mounts. The runtime
therefore gives each pod a required pod affinity to the tree, topology `kubernetes.io/hostname`,
whenever another pod of the tree is already scheduled (`manifest.go`, `podTemplate`). Whichever
pod is placed first — usually the root, but a child when both launch at once, as boot's
`launchUnfinished` can — decides the node for the whole tree. Nothing reschedules the tree when
that node fills up.

## What production's `legion` NodePool did

With no resource request, Karpenter launches the cheapest node that fits: a `c7a.medium`. Its
max-pods is 8, and seven daemonsets (cilium, cilium-envoy, datadog, ebs-csi, pod identity,
kube-proxy, fluent-bit) take seven of them. The first Legion pod fills the node; the second pod of
its tree can never schedule (`1 Too many pods`), and no new node helps, because the affinity names
the full one.

## Why a resource request is the wrong fix

A CPU request on the root makes Karpenter launch a bigger node for the root — when the root is
placed first. When a child is placed first, the child goes to any node that fits it, and the root
must then fit on the child's node. The Stage 4a run placed tree 2's child on the 4-vCPU node that
already held tree 1's 2-CPU root, and tree 2's 2-CPU root stayed Pending: `0/80 nodes are
available: 2 Insufficient cpu`. Under required colocation a request on a later pod can strand it.

## What to do

Size the node where every pod of every tree gets it, so whichever pod is first gets a node that
fits the tree: the `legion` NodePool in agent-c (`components/legion`) requires
`karpenter.k8s.aws/instance-cpu Gt 3`, so Karpenter launches the cheapest 4-vCPU type, with 58 pod
slots, while no Legion pod requests anything. A node selector on the same label does the same
only if every pod carries it, the image probe's included; the pool's floor needs nothing on any pod.

Then keep each node to one tree: every tree pod carries a required pod anti-affinity against pods
whose `legion.dev/tree` exists and is not its own, topology `kubernetes.io/hostname`
(`manifest.go`, `podTemplate`), beside the affinity to its own tree. Without it, two trees' roots
placed while the pool had room share one 4-vCPU node, and the node the floor sized for one tree
now runs two. The image probe's pod carries no tree label, so it never counts against a tree.
`scripts/e2e/stage4a-sandbox-runtime.sh` runs with neither a selector nor a request: its
`worker-colocated` check proves the tree fits its node, and its `concurrent-provision` check
proves the second tree runs on a node the first has no pod on.
