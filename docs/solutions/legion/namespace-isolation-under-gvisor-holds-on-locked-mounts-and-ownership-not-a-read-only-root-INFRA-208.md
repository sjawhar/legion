---
title: "Namespace isolation under gVisor: an unprivileged user namespace can cover a directory and bind one child back, and the cover holds, but a read-only root does not, so isolate roles of one pod by locked mounts and ownership"
category: legion
tags:
  - kubernetes
  - gvisor
  - user-namespaces
  - isolation
  - gke-autopilot
  - ephemeral-storage
date: 2026-10-24
status: active
module: packages/daemon/internal/launcher
related_issues:
  - "INFRA-208"
---

# Namespace isolation under gVisor holds on locked mounts and ownership, not on a read-only root

An issue pod runs every workflow role in one container, `roles`, and each role's generation in
Linux user, mount, PID and IPC namespaces of its own (`legion launcher role-init`,
`docs/kubernetes.md` "Anatomy of a Sandbox pod"). Each design choice of that isolation rests on
one of the measurements below. Read them before you change what a role's namespace covers.

## What was measured

All on 2026-10-24, on the production cluster, namespace `legion`, the worker image under
gVisor, as uid 1000 with all capabilities dropped and the RuntimeDefault seccomp profile. The
probes were throwaway shell scripts run in a pod (`nstest*.sh`) and server-side dry runs
(`warden-dryrun.sh`), kept outside this tree.

- `unshare --user --map-root-user --mount --pid --fork` works unprivileged. A nested
  `unshare --user --map-user=1000` works inside it.
- A tmpfs over a directory, then a bind of one subdirectory back: the agent in the nested user
  namespace reads its own file and gets `ENOENT` for a sibling's. `umount` and `umount -l` of the
  cover from a further nested user namespace fail ("not mounted": the mounts are locked).
  `mount --bind /` fails.
- The agent sees only its namespace's PIDs. Its `kill -9 -1` leaves processes outside alive. When
  the namespace's init exits, every process in it dies, a `setsid` one and one that ignores
  SIGTERM included.
- An overlayfs over `/home/legion` works with a tmpfs upper and with a disk `emptyDir` upper, not
  with an upper on the root filesystem. Its writes stay out of the container's view.
- A bind of a projected Secret's directory sees the kubelet's in-place update.
- A read-only remount of `/` inside the role's namespace is undone by a further nested user
  namespace.
- Writable by uid 1000 in the image's root filesystem: `/home/legion`, `/tmp`, `/var/tmp`,
  `/run/lock` (`/var/lock`), `/run/shm`, `/opt/legion/pi-envoy`, `/opt/legion/pi-legion`, and a
  few `/usr/lib/systemd/system/*.service` files nothing runs.
- GKE Autopilot admission: two init containers at 10Gi of ephemeral storage beside a 10Gi main
  container are admitted; a main container at 11Gi is refused (`Total ephemeral-storage requested
  by containers … higher than the Autopilot maximum of '10Gi'`); an init container at 11Gi is
  refused (`Max ephemeral-storage requested by init containers …`).
- Unprivileged user namespaces are refused on the devbox and for a non-root user in a privileged
  Docker container there (`kernel.apparmor_restrict_unprivileged_userns = 1`). [INFERENCE:
  GitHub's `ubuntu-24.04` runners have the same sysctl, since it is Ubuntu 24.04's default.]

## What follows from it

- **Cover and bind back; never rely on a read-only root.** A role sees its own gh, private, state
  and agent-secrets directories because role-init covers `/var/run/legion/roles` with an empty
  memory filesystem and binds only that role's back. The cover is locked, so the agent cannot
  remove it. A read-only `/` is no fence, because a nested user namespace undoes it. So every
  path uid 1000 could write in the image is either made the role's own (`/home/legion` as an
  overlay, `/tmp` and `/var/tmp` as binds of the role's directories, `/run/lock`, `/var/lock` and
  `/dev/shm` as private memory filesystems) or made root-owned in the image (the two plugin
  directories).
- **role-init stays the PID namespace's init.** Its exit ends every process of the generation, so
  a stop leaves nothing behind, and no role can signal a sibling.
- **The home overlay's upper needs a disk `emptyDir`.** An upper on the root filesystem does not
  work, and a tmpfs upper would charge a role's home writes to the pod's memory. So the pod has one
  disk `emptyDir`, `role-homes`.
- **A role's gh bundle follows token rotation.** The bind of the projected Secret's directory sees
  the kubelet's update, so no relaunch is needed when a role's App token is refreshed.
- **GKE Autopilot's 10Gi is per pod's main containers and per init container.** One `roles`
  container with the issue's whole reservation, and init containers that carry the same
  reservation, fit under `issue: {ephemeral_storage: 10Gi, ephemeral_storage_request: 10Gi}`.
- **Tests that make a user namespace need a host that allows it.** On a host with
  `apparmor_restrict_unprivileged_userns = 1`, role-init's tests cannot run unprivileged. The CI
  job that runs them must allow user namespaces first.
