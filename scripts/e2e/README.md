# Live proofs

Each script here runs real binaries built from the checkout against real dependencies on this box
and fails loudly on the first step that does not hold. Some are a stage's gate for the Go
coordinator — unit tests do not gate a stage, these do; others prove one capability end to end
against the world it will run in. A later stage's script lands beside these.

| script | proves |
| :--- | :--- |
| `stage1-skeleton.sh` | `legion start` boots against a local Postgres, serves `/healthz` and `GET /legion/v1/state`, answers `legion state`, registers itself in the Go daemon's own legions registry, survives a restart against the same store with its first boot time intact, and refuses an unreachable Postgres by the host it could not reach and never by the password |
| `verifiers-staging-token.sh` | `dispatch` and the Envoy listener authenticate a projected service-account token the staging EKS cluster actually minted — the right audience is accepted, the other binary's audience and a missing bearer are refused, each shared token still works, half an OIDC pair and an issuer that does not answer refuse the boot, and a refused token leaves its failure class in the log and nowhere else |

## stage1-skeleton.sh

```sh
bash scripts/e2e/stage1-skeleton.sh     # → "stage 1 e2e: PASS", exit 0
```

Needs `go`, `docker`, `jq`, `curl` and `ss`. It builds the binary from the checkout
(`packages/daemon-go/cmd/legion`), so it proves the tree you are standing in.

| input | default | meaning |
| :--- | :--- | :--- |
| `LEGION_E2E_PG_DSN` | unset | the Postgres to run against. Unset, the script starts its own `postgres:16` container (`legion-e2e-pg-<pid>`) on an ephemeral loopback port and removes it on the way out — the devbox path. Set, it starts no container: that is how CI hands it the job's service. |

Everything the run takes is its own, so two runs on one box — a CI job and a devbox session, or
two sessions — neither collide nor report each other as a leftover:

- a `mktemp -d` work directory (`/tmp/legion-e2e.XXXXXXXX`) — the built binary, the two
  `legion.yaml`s, the two state documents, the refusal log. Removed when the run passes; **kept
  when it fails**, and its path printed, because those documents are the evidence.
- `XDG_STATE_HOME=<work>/xdg` — so the legions registry the run writes is its own, never the
  box's `~/.local/state/legion/legions-go.json`.
- a per-run project key (`E2E<pid><epoch>`) — boots are counted per project, so a fresh key is
  what makes `boots == 1` true on a store that has served other runs.
- a free daemon port picked per run (20000–39999, checked with `ss`) and, on the devbox path, the
  container `legion-e2e-pg-<pid>`.

After any exit — pass, failure, or an interrupt — the `EXIT` trap removes the container and
signals the daemon; `docker ps -a --filter name=legion-e2e-pg` comes back empty and no daemon is
left holding the run's port.

### How it fails

Every step is fatal and names its reason: the refusal that does not name its host, a `/healthz`
that never answers, a state document that is not boot 1 with cap 4 and no issues, a registry
without exactly one entry for this run, a `firstBootAt` that moved across the restart, a daemon
that ignored a stop (SIGKILLed on the way out, so nothing holds the port) or exited non-zero on
one. `stop_daemon` reads and judges the exit status, because `daemon.Run` returns 0 on a
cancelled context — a non-zero status there is a defect, not a stop.

Two notes on what the script had to learn about its own surface:

- Postgres readiness is probed **over TCP** (`pg_isready -h 127.0.0.1`). The container
  entrypoint's bootstrap phase answers on the unix socket while nothing listens on 5432 yet, and
  a daemon that connects in that window is reset by the peer.
- The "never the password" assertion is written `grep -q … && exit 1`, not `! grep -q …`:
  `set -e` ignores a negated pipeline (shellcheck SC2251), so the negated form could never fail
  the run.

### In CI

The `daemon-go` job in `.github/workflows/envoy-and-contracts.yaml` runs `go vet ./...` and
`go test ./...` in `packages/daemon-go` against its `postgres:16` service (`LEGION_TEST_PG_DSN`),
then this script with `LEGION_E2E_PG_DSN` pointing at the same service — so the script runs no
docker of its own there. The job is gated on the workflow's `changes` filter (`daemon_go`:
`packages/daemon-go/**`, `go.work`, `scripts/e2e/**`).

## verifiers-staging-token.sh

```sh
bash scripts/e2e/verifiers-staging-token.sh     # → "verifiers e2e: PASS", exit 0
```

Needs `go`, `docker`, `kubectl`, `jq`, `curl`, `ss` and `base64`, and credentials for the `staging`
kube context — it mints two real tokens with `kubectl -n legion create token default --audience
{dispatch,envoy} --duration 10m` (the TokenRequest API) and reads the issuer from the cluster's own
`/.well-known/openid-configuration`. That is why it is a devbox gate and not a CI job: GitHub's
runners have no cluster to mint from. It builds both binaries from the checkout, so it proves the
tree you are standing in.

| input | default | meaning |
| :--- | :--- | :--- |
| `VERIFIERS_E2E_KUBE_CONTEXT` | `staging` | the cluster to read the issuer from and mint against |
| `VERIFIERS_E2E_NAMESPACE` | `legion` | the namespace whose `default` service account the tokens are minted for |

What the run touches, and nothing else:

- `/tmp/verifiers-e2e` — the two built binaries, the boot logs, the refusal logs, the last response
  body, and the scratch `HOME` the binaries run under, so Dispatch's signing key never lands in the
  box's `~/.local/share/dispatch`. Recreated from empty each run.
- the containers `verifiers-e2e-pg` (`postgres:16`) and `verifiers-e2e-nats` (`nats:2.10 -js`), both
  on ephemeral loopback ports.
- the first free port at or above 14100 for `dispatch` and 14200 for the listener.

After any exit — pass, failure, or an interrupt — the `EXIT` trap signals both binaries and removes
both containers; `docker ps -a --filter name=verifiers-e2e` comes back empty.

### No raw token is printed

A token lives in a shell variable, reaches curl through a config document on stdin — never an
argument, since `/proc/<pid>/cmdline` is world-readable, and never a file — and everything the
script prints that it did not compose itself goes through `redact`, which replaces any JWT-shaped
run with `<redacted-jwt>`. The two minted tokens are reported by their decoded `iss`, `aud`, `sub`
and `exp`. The last step asserts that neither binary's log contains `eyJ`.

### How it fails

Every step is fatal and names its reason: a cluster that will not mint, a discovery document with
no `https` issuer, a token whose `iss`, `aud` or `sub` is not the one asked for, a container that
never becomes ready, a binary that exits during boot (reported at once from its dead pid, with its
log, rather than after the whole wait), a boot log that does not name the issuer, any of the eight
calls answering the wrong status, a body that is not the actor the verifier should have produced, a
refusal whose class is missing from the log — or present in the 401 body, which would tell an
unauthenticated caller which credential the listener is configured for.

Two notes on what the script had to learn about its own surface:

- Postgres readiness is probed **over TCP** (`pg_isready -h 127.0.0.1`). The container entrypoint's
  bootstrap phase answers on the unix socket while nothing listens on 5432 yet, and a client that
  connects in that window is reset by the peer.
- The listener answers `/healthz` 200 with `{"status":"starting"}` before NATS is up, and every
  `/v1` route is 503 until then, so the wait is for the healthy status and not for the port.
- The "never prints a token" assertions are written `grep -q … && fail`, not `! grep -q …`:
  `set -e` ignores a negated pipeline (shellcheck SC2251), so the negated form could never fail the
  run.
