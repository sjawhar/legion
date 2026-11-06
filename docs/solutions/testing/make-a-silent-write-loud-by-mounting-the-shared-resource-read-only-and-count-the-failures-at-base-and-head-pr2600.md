---
title: "Make a silent write loud: mount the shared resource read-only under unshare -Urm, and count the failures at the base and at the head"
category: testing
tags:
  - legion-pod
  - unshare
  - read-only-mount
  - negative-control
  - shared-clone
date: 2026-10-31
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2600"
---

# Make a silent write loud: mount the shared resource read-only under `unshare -Urm`, and count the failures at the base and at the head

When the suspected defect is a write into something the process does not own, do not go looking for
the write. Forbid it, and count what breaks.

```sh
cat > "$TMPDIR/ro.sh" <<'EOF'
#!/bin/sh
set -e
mount --bind /legion/repos /legion/repos
mount -o remount,bind,ro /legion/repos
cd "$1"; shift; exec "$@"
EOF
chmod +x "$TMPDIR/ro.sh"
unshare -Urm "$TMPDIR/ro.sh" <the directory that reproduces it> <the command>
```

- **Run it from the directory that reproduces the defect, not from the repository root.** Here the
  working directory *is* the defect, so a `go test` whose binary runs in its own package directory
  reproduces it and a run from a neutral directory does not.

- **Report two numbers at the base commit and two at the head**: how many tests failed, and how
  many lines of output name the forbidden path. "0 and 0" is the claim; a green run on its own is
  not, because a green run is also what a silent write gives you.

- **Build the base binary separately.** A scratch copy of the module under `$TMPDIR` with the
  changed files restored from the base commit gives a binary you can run with the *same* working
  directory, which a `go test` inside the scratch copy would not.

- **Know what the guard itself breaks.** `unshare -U` makes the process uid 0 with
  `CAP_DAC_OVERRIDE`, so any test whose isolation is permission bits (`os.Chmod(dir, 0)`) fails
  under the guard at every commit, your change or not. Run that package outside the guard and say
  which one it is.

## Evidence (sjawhar/legion#2600, INFRA-623)

- The planner's first measurement, before any code moved: the `internal/workspace` test binary run
  from its package directory with `/legion/repos` read-only gave **61 test failures** and 96
  occurrences of `Internal error: Failed to determine the secure config for a repo` /
  `Read-only file system (os error 30) at path "/legion/repos/github.com/sjawhar/legion/.jj/repo/.tmpXXXXXX"`;
  the same binary from an empty directory outside any jj repository passed with 0 failures. That
  pair is what turned "the tests fail in a pod" into a located defect.

- Base against head, the same binary shape both times: at the base commit, exit 1, **61** `--- FAIL`
  lines and **192** lines naming `/legion/repos/github.com/sjawhar/legion`; at the head, exit 0,
  **0** and **0**.

- The guard's own cost, recorded in the handoff so the next role does not read it as a regression:
  `internal/runtime/sandbox`'s `TestWhatAnIssuePlantsNeitherReadsTheProvisioningTokenNorOutlivesItsPod`
  hides volumes with `os.Chmod(dir, 0)` and so fails under the guard at the head and at the base
  alike, and passes without it. It was controlled against the base commit's own file before being
  called the environment's.

Related: `docs/solutions/legion/namespace-isolation-under-gvisor-holds-on-locked-mounts-and-ownership-not-a-read-only-root-INFRA-208.md`
for what a namespace can and cannot hold, and
`docs/solutions/testing/go-test-in-a-worker-pod-leaves-gowork-to-go-work-and-names-the-packages-the-pod-cannot-run-LEGION-630.md`
for the packages a pod cannot run at all.
