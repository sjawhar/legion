---
title: "A file-mode or group claim about a published image needs a positive control and the layer's tar header"
category: testing
tags:
  - worker-image
  - rootfs
  - tar
  - setuid
  - positive-control
  - proof
date: 2026-10-15
status: active
module: packages/daemon/docker
related_issues:
  - "LEGION-767"
  - "sjawhar/legion#1973"
---

# A file-mode or group claim about a published image needs a positive control and the layer's tar header

Extends docs/solutions/testing/prove-a-published-worker-image-from-a-pod-by-unpacking-its-rootfs-and-chrooting-under-unshare-LEGION-764.md.
That recipe proves what a published image carries; these are the extra rules when the claim is about a
file's mode bits or its group rather than its presence.

- Extract with `tar -xp`. GNU tar running as a non-root uid strips setuid and setgid bits on extraction
  unless `-p` is given, so without it a mode claim reads `755` on every image, fixed or not. The recipe
  carries the flag now; this is why it is there.
- Prove a bit is absent only beside an image where it is present. Run the same `stat`, `find -perm /6000`
  and guard commands on the pre-fix image first — the `sha-<12 hex>` tag of the base's last `Worker Image`
  run — and see the bit (`4755`, `2755`); then run them on the head's tag. `755` without that control
  proves the extraction, not the image. Cheap when the two tags share most layers; one rootfs at a time
  (2.8 GB each).
- A file's group cannot be read inside the chroot: `--no-same-owner` as uid 1000 and `unshare -r` make
  every extracted file `root:root` there, so `stat -c '%U:%G'` answers `root:root` whatever the image
  holds. Read the layer's tar header before unpacking it — `tar -tv $comp -f "$f" | grep -E
  'usr/(bin/ssh-agent|lib/openssh/)'` inside the pull loop prints `-rwxr-xr-x 0/101 usr/bin/ssh-agent`,
  and `getent group 101` in the chroot names the gid (`_ssh`). The header is the evidence; `stat` is not.
- A guard the Dockerfile's final step runs is layer-scoped: it fails a rebuild of the layer that brings a
  bit back (a new base digest, a package update) and reads nothing once the image is published. A
  criterion about the deployed image is still the production check's: pull the released digest with the
  recipe (the manifest endpoint takes `@sha256:…` as it takes a tag) and run the same commands in its
  chroot.

## Evidence

LEGION-767 (sjawhar/legion#1973) dropped `openssh-client`'s set-id bits (`ssh-keysign` 4755, `ssh-agent`
2755 `_ssh`) from the worker image and added a final-step guard. The planner measured that a synthetic
4755/2755 pair tarred and extracted as uid 1000 read `755`/`755` without `-p` and `4755`/`2755` with it,
so the spec's `stat … → 755` proof was vacuous under the recipe as written. Implementer and tester each
ran the recipe with `-p` on the pre-fix `sha-40b71edb8b43` (manifest sha256 `8044216f…`: `4755
/usr/lib/openssh/ssh-keysign`, `2755 /usr/bin/ssh-agent`, `find / -xdev -perm /6000 -path '*ssh*'`
naming both, the guard's command rc 1) and then on the head's `sha-11e8dd571109` (manifest sha256
`28eee65e…`, run 38782814273's `containerimage.digest`: `755`/`755`, `find` printing nothing, `set-id
bits: none` rc 0, `legion probe-image` OK). The tester read `ssh-agent`'s group from the apt layer's
header (`0/101`) after the chroot's `stat` showed `root:root` for every file.
