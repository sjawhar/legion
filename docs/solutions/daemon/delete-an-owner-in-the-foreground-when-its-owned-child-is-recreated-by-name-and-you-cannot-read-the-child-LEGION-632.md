---
title: "Delete an owner in the foreground when its owned child is recreated by name and you cannot read the child"
category: daemon
tags:
  - kubernetes
  - garbage-collection
  - owner-reference
  - persistent-volume-claim
  - agent-sandbox
  - rbac
date: 2026-10-09
status: active
module: packages/daemon/internal/runtime/sandbox
related_issues:
  - "LEGION-632"
  - "sjawhar/legion#1842"
---

# Delete an owner in the foreground when its owned child is recreated by name and you cannot read the child

- A Kubernetes delete with the default (background) propagation removes the owner at once and
  leaves garbage collection to remove what it owns afterwards. If the next incarnation of the owner
  asks for an owned child by the same name — an issue's next Sandbox names the same PVC,
  `issue-legion-<project>-<issue>` — a create inside that window asks for a child still
  `Terminating`, and the operation that should have been clean fails or waits on a retry. Use
  `metav1.DeletePropagationForeground`: the owner stays, `Terminating`, until every owned object is
  gone, so the owner's absence is the confirmation that the child is gone too.
- That confirmation is the only one a restricted identity has. The Legion daemon's Role holds no
  PVC verb, so it cannot list or read the volume it needs gone; with foreground propagation it
  polls the Sandbox alone (`deleteSandbox`, `internal/runtime/sandbox/cleanup.go`), and a launch
  that waits out a Sandbox being deleted (`ensureSandbox`) waits for the volume with it. Design
  waits around the object you may read.
- Fence every delete to the object you decided on, `Preconditions{UID, ResourceVersion}`: a
  suspended Sandbox is relabelled in place (same UID) when its issue is re-admitted into another
  tree, so only UID plus ResourceVersion tells the orphan the sweep's snapshot saw from a Sandbox
  running again under a new tree. A conflict means someone wrote it since; let the next owner of
  the decision (the sweep) decide again rather than deleting what is now someone else's.
- Give every delete path the same propagation, or a reader will assume one. LEGION-632 shipped the
  tree cleanup and the done child's release in the foreground and the orphan sweep's fenced delete
  in the background; the reviewer found the inconsistency and the re-admission race behind it, and
  round 8 made `deleteFenced` (`internal/runtime/sandbox/sandbox.go`) foreground too. The two
  helpers still differ in their conflict policy (wait vs keep-and-log) and are the named debt of
  the fast-follow: name the policies and share the call.

## Evidence

#1842 (LEGION-632): each issue's Sandbox owns its PVC and role Secrets by owner reference, the
PVC named by the issue, and a child issue set `done` releases its Sandbox while its parent runs
(`SuspendIssue` with release, `internal/runtime/sandbox/issue_suspend.go`); the tree cleanup
deletes every remaining Sandbox of a tree (`CleanupTree`, `cleanup.go`); a later `todo` on the
same child creates a Sandbox naming the same PVC. The reviewer's finding at 21d9cb03 read the
sweep's `deleteFenced` as background beside the foreground tree cleanup and named the Terminating
PVC a re-admission inside the GC window would hit; the architect took it as a must for round 8
(commit 3e54a458). Stage 4a's `orphan-sweep` checkpoint asserts the swept issue's PVC is gone with
its Sandbox (`kubectl get pvc -l legion.dev/issue=<orphan>`: none), and `child-release` in stage 4b
asserts a done child's PVC goes while the parent's stays Bound.
