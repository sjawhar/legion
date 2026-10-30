---
title: "A docker run line is proven from a pod with no container runtime as a two-container Job whose client ships in the image"
category: testing
tags:
  - kubernetes-job
  - proof
  - negative-control
  - docker-run
  - image-digest
date: 2026-10-29
status: active
module: scripts/e2e
---

# A docker run line is proven from a pod with no container runtime as a two-container Job whose client ships in the image

Extends docs/solutions/testing/prove-a-worker-image-change-with-a-job-on-the-published-digest-the-bases-image-and-controls-inside-the-head-image-INFRA-257.md.

That note runs a Job whose container is a check script that exits, and says a Job "can carry a
sidecar, such as Postgres for a suite". This note is the other arrangement: the sidecar is the
subject, the check is the second container, and the Job never completes.

- **Choose the form by where the client runs, not by what is quicker.** A client the image under
  test already carries (`psql` and `pg_isready` in a Postgres image) goes in a second container of
  the same pod and reaches the server at `127.0.0.1:<port>` over the pod's shared network
  namespace, which is what `-p 127.0.0.1::5432` plus a client on the host is on a devbox. That
  form needs no Service, no reachability label and no `pods/exec`, so it runs in a deployment that
  grants `create jobs` and refuses the Service verbs, where the published sequence cannot run at
  all. A client that is the role pod itself — a test binary, a language suite — still needs the
  Service form of `docs/kubernetes.md`, "Running a workload in the cluster from a role", whose gate
  is all three of `create jobs`, `create services` and `get services` answering `yes`.
- **Put the namespace's four-processor floor on the auxiliary container.** Admission asks that the
  pod's largest container request at least four processors (`docs/kubernetes.md`, the rules list of
  that section). When the point of the run is the cap the `docker run` line names, set the
  subject's `requests` and `limits` to that real cap (`--cpus 2 --memory 2g` → `cpu: "2"`,
  `memory: 2Gi`) and request the floor on the client. A one-container Job at the real cap is
  refused at submission and runs nothing, so a driver that does not read its own `create` error
  reports a case it never ran.
- **A server container never completes, so read the client's exit code.** A Job whose main
  container runs a server reaches neither `Complete` nor `Failed` while the server lives, so
  `kubectl wait --for=condition=complete` and `--for=condition=failed` both sit to their timeout.
  Poll
  `{.items[*].status.containerStatuses[?(@.name=="client")].state.terminated.exitCode}`
  instead, and the server container's own exit code for a case that has no client, such as a
  refusal. Read both containers' logs before the delete, since the delete takes the pod.
- **Resolve a moving tag at two registries and record the image's config, not only its digest.**
  Ask the mirror and the upstream registry for the tag's `docker-content-digest`, require the two
  answers to agree, then read the linux/amd64 config blob and record `Env`, `Volumes`,
  `Entrypoint` and `Cmd` in the pull request. The config says which mounts the image accepts and
  where its data directory is, which is what decides whether each copied line needs a mount edge
  at all; the digest alone says none of that. Confirm the run with the pod's `imageID`, as the
  note above says.
- **A lane that compiles the changed line is not a lane that runs it.** Before citing CI as
  evidence for a changed command, read the job's own environment: a job that sets the variable the
  line is guarded by takes the other branch, and a skip message's text is printed by no passing
  test. Record such a lane as the regression check it is, and prove it compiles the line by
  breaking it once and watching the lane go red.

## Evidence

- sjawhar/legion#2498 moved five devbox `docker run` sites and the daemon's
  four test skip messages to a digest-pinned PostgreSQL 18. No CI lane runs any of those lines:
  `.github/workflows/pr-and-main.yaml:291` sets `LEGION_E2E_PG_DSN` for the `test` job, and
  `scripts/e2e/stage1-skeleton.sh:148` guards the edited `docker run` with
  `if [ -z "${LEGION_E2E_PG_DSN:-}" ]`, while `LEGION_TEST_PG_DSN` at `:289` keeps every skip
  message unprinted. Deleting the `testpg` import from
  `packages/daemon/internal/store/store_test.go` made `go vet ./internal/store/` exit 1 with
  `undefined: testpg`, which is the whole of what that lane proves about these lines.
- The implementer's run `1c370b3b`, six Jobs in the `builds` namespace: five clients exited 0 with
  `select 1 -> 1`, `server_version_num -> 180006` and
  `data_directory -> /var/lib/postgresql/18/docker`, each `db` container reporting
  `imageID sha256:a1be1c83…` for the pinned digest. No Service and no reachability label was
  created for any of them.
- The tester drove the same surface with each `db` container capped at its own line's
  `--cpus`/`--memory` (2 CPU and 2 GiB, 0.5 CPU and 2 GiB for
  `docs/site/media/walkthroughs/legion-operator/record-casts.sh:122`) and the floor on the client,
  runs `b3d9af3a` and `ccb23eed`, and reproduced every reading. Its first attempt at the refusal
  case, with the server at 2 CPU and no client container, never ran a container: admission refused
  the Job because no container of its pod template requested four processors. The case was re-run
  with the floor on the server, which is what a client-less case must do.
- The same pull request ran both forms. The end-to-end check of what the skip messages tell a
  developer — `LEGION_TEST_PG_DSN=… go test ./internal/store/ ./internal/api/` from the role pod —
  had to use the Service form, and reported `ok …/internal/store 60.016s` and
  `ok …/internal/api 151.763s` against `PostgreSQL 18.6 (Debian 18.6-1.pgdg13+2)`.
- The tag `18` at `mirror.gcr.io/library/postgres` and at `registry-1.docker.io` both answered
  `sha256:979bccfde35445db618c65107d27d08c6c21339d8dc3ad4c830c6633947be0e4`, whose amd64 config
  holds `PG_VERSION=18.6-1.pgdg13+2`, `PGDATA=/var/lib/postgresql/18/docker` and
  `Volumes {"/var/lib/postgresql":{}}`. That last field is why the four Go skip messages, which
  carry no `--mount`, needed no mount edit: the image's one anonymous volume is already the path
  the entrypoint accepts.
- The driver was recorded as a Dispatch artifact before the
  implementer completed, and the tester re-ran that same file for three of its cases (run
  `54fc4ca8`) beside its own driver.
