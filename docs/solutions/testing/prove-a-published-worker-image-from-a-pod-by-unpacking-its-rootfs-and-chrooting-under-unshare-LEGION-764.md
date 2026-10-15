---
title: "Prove a published worker image from inside a pod by unpacking its rootfs and chrooting under unshare"
category: testing
tags:
  - worker-image
  - ghcr
  - rootfs
  - chroot
  - unshare
  - gvisor
  - probe-image
  - proof
date: 2026-10-15
status: active
module: packages/daemon/docker
related_issues:
  - "LEGION-764"
  - "sjawhar/legion#1967"
---

# Prove a published worker image from inside a pod by unpacking its rootfs and chrooting under unshare

A worker pod has no `docker`, cannot create pods and cannot `exec` into one (`kubectl auth can-i create
pods -n legion` and `create pods/exec` both answer no). It can still prove what a published
`ghcr.io/sjawhar/legion-worker` image carries and what that image's own `legion probe-image` decides:

- Pull the image anonymously — the package is public — with a token from
  `https://ghcr.io/token?scope=repository:sjawhar/legion-worker:pull`, kept in a shell variable and never
  printed. Fetch `/v2/sjawhar/legion-worker/manifests/sha-<12-char head sha>` with an `Accept` naming the OCI
  manifest type, and check `sha256sum` of the manifest file equals the `Worker Image` run's
  `containerimage.digest`: that is what ties the rootfs to the run you cite.
- Unpack the layers in manifest order into one directory with `tar -x -z --no-same-owner --exclude='.wh.*'
  --exclude='dev/*'`, after handling each layer's whiteouts (`.wh.<name>` removes `<name>`; `.wh..wh..opq`
  empties the directory). 22 layers, about 1 GB downloaded, 2.8 GB unpacked, 90 seconds; `rm -rf` it afterwards
  (the implementer's and tester's ephemeral-storage limit is 20Gi, the other roles' 10Gi).
- Enter it with `unshare -r -m -p -f sh -c '…'`: mount `proc` at `<rootfs>/proc`, then `mount --rbind /dev
  <rootfs>/dev`, then `/usr/sbin/chroot <rootfs> /usr/bin/env -i PATH=/opt/legion/bin:/opt/omp/bin:/opt/codegraph/bin:/usr/local/bin:/usr/bin:/bin HOME=/home/legion OMP_PROFILE=legion LEGION_OMP_PATH=/opt/omp/bin/omp DO_NOT_TRACK=1 sh -c '…'`.
  Under gVisor a plain `mount --bind /dev <rootfs>/dev` is refused (`wrong fs type, bad option, bad superblock`);
  `--rbind` is accepted. Without `/dev`, `ssh-keygen` fails with `Couldn't open /dev/null` and `legion probe-image`
  with `boot gate: run the OMP pi.agents probe: open /dev/null: no such file or directory`.
- Inside, run the image's own binaries: `legion version` (the commit it was built from), `legion probe-image
  --plugin-root /opt/legion/pi-legion --envoy-plugin-root /opt/legion/pi-envoy --skip-agent-models` (the
  capability table and the OK line, exit 0), and the subprocess forms of any PATH claim. Take the negative control in
  the same rootfs: `cp -p <rootfs>/usr/bin/<name> aside; rm <rootfs>/usr/bin/<name>`, the probe prints
  `capability toolchain: missing (<name> is not on PATH)` and exits 1, restore, and the pass returns.

What this proves and what it does not. It proves the published image's contents and the verdict of the
`legion` that image carries, which is also the binary the daemon's probe pod runs (`internal/runtime/sandbox/probe.go`,
`r.tools.Legion`) and the build's final `RUN` runs. It does not run the daemon's probe Sandbox, so Stage 4a's `image-probe`
check stays the operator-lane proof for a change to how the probe is launched or what it loads
(`docs/solutions/testing/stage-4a-packs-its-plugins-through-lib-pack-plugin-and-only-a-full-devbox-run-from-identity-is-the-proof-LEGION-164.md`);
for a change to what the probe looks for inside the image, LEGION-764's architect ruled this rootfs run, the Worker Image
run's own probe step and the branch's `legion probe-image` in the pod enough before the merge, with the daemon's probe of
the pinned image recorded in the production check. A criterion stated for "a worker pod created after the pin" is still
the production check's, not this recipe's.

Three things the chroot gets wrong, none a defect of the image: you are uid 0 inside it (`unshare -r`), so a test that
expects a non-root uid fails there (the tester's `install-model-gateway.test.ts` case); the chroot's `sh` is dash, whose
`command -v a b` prints only the first name (POSIX takes one operand) — use `bash -c 'type -P a b'`; and
`/tmp` inside the rootfs is the rootfs's own, so a key written to `/tmp/k` there is gone with the directory.

## The two scripts

`pull-rootfs.sh <tag> <rootfs>`:

```bash
#!/usr/bin/env bash
set -euo pipefail
tag="$1"; rootfs="$2"; repo=sjawhar/legion-worker
tok="$(curl -fsS "https://ghcr.io/token?scope=repository:${repo}:pull" | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')"
mkdir -p "$rootfs" /tmp/layers
curl -fsS -H "Authorization: Bearer $tok" \
  -H 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
  "https://ghcr.io/v2/${repo}/manifests/${tag}" > /tmp/manifest.json
sha256sum /tmp/manifest.json   # equals the Worker Image run's containerimage.digest
mapfile -t layers < <(python3 -c 'import json; m=json.load(open("/tmp/manifest.json")); [print(l["digest"], l["mediaType"]) for l in m["layers"]]')
for entry in "${layers[@]}"; do
  d="${entry%% *}"; mt="${entry#* }"; f="/tmp/layers/${d#sha256:}.tar"
  case "$mt" in *gzip) comp="-z" ;; *zstd) comp="--zstd" ;; *) comp="" ;; esac
  curl -fsSL -H "Authorization: Bearer $tok" "https://ghcr.io/v2/${repo}/blobs/${d}" -o "$f"
  tar -t $comp -f "$f" | grep -E '(^|/)\.wh\.' | while IFS= read -r wh; do
    dir="$(dirname "$wh")"; base="$(basename "$wh")"
    if [ "$base" = ".wh..wh..opq" ]; then rm -rf "${rootfs:?}/${dir:?}"/* 2>/dev/null || true
    else rm -rf "${rootfs:?}/${dir}/${base#.wh.}"; fi
  done || true
  tar -x $comp -f "$f" -C "$rootfs" --no-same-owner --exclude='.wh.*' --exclude='dev/*' || echo "tar exit $? (continuing)"
  rm -f "$f"
done
```

`chroot-probe.sh <rootfs>`:

```bash
#!/usr/bin/env bash
exec unshare -r -m -p -f sh -c '
  rootfs="$1"
  mount -t proc proc "$rootfs/proc"
  mount --rbind /dev "$rootfs/dev"
  /usr/sbin/chroot "$rootfs" /usr/bin/env -i \
    PATH=/opt/legion/bin:/opt/omp/bin:/opt/codegraph/bin:/usr/local/bin:/usr/bin:/bin \
    HOME=/home/legion OMP_PROFILE=legion LEGION_OMP_PATH=/opt/omp/bin/omp DO_NOT_TRACK=1 \
    sh -c "
      bash -c \"type -P jq ssh-keygen\"; echo rc=\$?
      legion version
      legion probe-image --plugin-root /opt/legion/pi-legion --envoy-plugin-root /opt/legion/pi-envoy --skip-agent-models; echo rc=\$?
    "
' sh "$1"
```

## Evidence

LEGION-764 (sjawhar/legion#1967) added `jq` and `openssh-client` to the image and both names to the probe's toolchain
row. In pod `legion-legion-legion-764`, on `ghcr.io/sjawhar/legion-worker:sha-40b71edb8b43` (manifest sha256
`8044216f…`, the `Worker Image` run 38766268187 digest): `type -P` printed `/usr/bin/jq` and `/usr/bin/ssh-keygen`,
`legion version` printed `commit 40b71edb…`, `legion probe-image` printed 20 rows with 7 `present`, the toolchain row ending
`/usr/bin/jq, /usr/bin/ssh-keygen; go version go1.26.8 linux/amd64`, and the OK line, rc 0. With `/usr/bin/jq` removed:
`probe-image: capability toolchain: missing (jq is not on PATH)`, stderr `legion probe-image: capability toolchain is
missing: jq is not on PATH`, rc 1; the same for `ssh-keygen`; restored, the pass again. The tester re-ran the recipe from a
PR comment, added `chmod a-x /usr/bin/jq` (`jq is not on PATH` too: `lookPath` checks the execute bit) and bind-mounted
the issue workspace at `/work` to run `bun test scripts/e2e/lib/` inside the chroot: 314 pass, 3 fail (two `docker`
spawns, one uid-0 artefact), none naming `jq`.
