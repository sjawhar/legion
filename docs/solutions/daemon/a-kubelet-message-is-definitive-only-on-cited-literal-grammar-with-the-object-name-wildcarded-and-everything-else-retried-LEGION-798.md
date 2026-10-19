---
title: "A kubelet message is definitive only on cited literal grammar with the object's name wildcarded, read from both surfaces the fault reaches, gated on the state that can produce it, and everything else is retried"
category: daemon
tags:
  - image-probe
  - failed-mount
  - create-container-config-error
  - kubelet
  - retry-policy
  - boot-refusal
  - sandbox-runtime
date: 2026-10-19
status: active
module: packages/daemon/internal/runtime/sandbox/probe.go
applies_when:
  - Adding a stop to the image probe's stuck that ends an attempt on a Kubernetes event or container waiting state
  - Classifying any kubelet or API-server text as a boot refusal rather than a transient the retry budget covers
  - Documenting a boot-blocking refusal over a value of runtime.kubernetes.pod
related_issues:
  - "LEGION-798"
  - "sjawhar/legion#2043"
---

# A kubelet message is definitive only on cited literal grammar with the object's name wildcarded, read from both surfaces the fault reaches, gated on the state that can produce it, and everything else is retried

Extends docs/solutions/daemon/boot-probe-kill-is-transient-not-a-verdict.md.
That document's taxonomy (definitive, waiting, transient under a bounded retry) holds for the Go
runtime's probe; this is how a Kubernetes signal earns the definitive class when its wording was
read from the kubelet's source rather than measured on the cluster.

- Match the fixed grammar literally and wildcard only the piece that varies: one regexp,
  `(configmaps?|secrets?) "[^"]*" not found|couldn't find key \S+ in (ConfigMap|Secret) `, plus
  plain `strings.Contains` for the forms that carry no name. Put every producer's spelling of the
  same fault in the alternation (the kubelet's watching manager says `secret "x" not found`, the
  API server it asks on another strategy says `secrets "x" not found`), and cite the upstream file
  and symbol beside the pattern. A wording you did not cite falls through to the retry budget,
  never to the refusal: a near-miss costs the old wait, a wide match (any `not found`) turns a
  self-healing cache miss into a boot refusal.
- Read both surfaces one operator mistake reaches. A volume's missing object is a `FailedMount`
  event (an API list, interval-gated); the same object missing from a container's environment is a
  `CreateContainerConfigError` waiting state on the pod you already hold, with a different wording
  for the same key (`couldn't find key k in ConfigMap ns/n` against `configmap references
  non-existent config key: k`). Judge the status surface first and on every pass: it costs no call.
- Gate the event read on the state that can produce the event, not on a configuration flag:
  `pod.Spec.NodeName != ""` (the kubelet mounts only a pod bound to it), so a pod the pool cannot
  place costs no list and a deployment with no providers Secret still gets a mount diagnosis.
  Stamp the read time before the list, so an API error costs one interval and not a tight loop.
- Keep the existing exact-name rule first on the same list (the providers Secret's wording is
  what its tests and docs quote) and let the general rule take the rest; one read feeds both.
- Write the documented guarantee with the gate's own qualifier. The kubelet attempts no volume no
  container mounts, so "a volume naming a missing ConfigMap is refused" is wider than the code: say
  "a volume a `volume_mounts` entry mounts", and have the live check mount its fixture volume for
  the same reason.

## Evidence

Before LEGION-798 `stuck` read a probe pod's events only when the pod mounted provider keys and
refused only the providers Secret's three Secret forms; any other `FailedMount` ran the attempt to
its budget, six attempts of 660 s at the production settings (about 71 minutes), past the
Deployment's 60-minute startup probe, so a misconfigured pod block never named its missing object.
The change (`probe.go`: `missingObjectForm`, `missingObject`, `missingVolume`, `volumeRefusal`,
`volumeSources`, `probeWaiting` returning the whole waiting state) refuses within one 15 s read of
the pod's scheduling; the throwaway smoke with `bootprobe.Image` and a 660 s budget refused in
under 20 ms with one Sandbox create, and the retried form `failed to sync configmap cache: timed
out waiting for the condition` ran a 2 s budget out over two creates. The forms were read at
Kubernetes v1.35.8 (`pkg/volume/configmap`, `pkg/volume/secret`, `pkg/kubelet/util/manager`,
`kubelet_pods.go`'s env path); the plural API-server form was found only by reading the manager's
strategies, and the `desired_state_of_world_populator.go` skip of an unmounted volume only by
asking why a volume alone would raise no event. Both would have been missed by copying one
message seen once on a cluster. The docs' unconditional sentence was caught in review (PR #2043,
round 1) and qualified in three places.

The kubelet's real wording beside the refusal stays unmeasured until the stage 4a check
`image-probe-missing-volume-negative` runs on a devbox; the production check reads it there.
