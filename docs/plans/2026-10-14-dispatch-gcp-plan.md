# Dispatch on Google Cloud: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Legion's `envoy-infra` plugin deploys Dispatch on GKE Autopilot with Cloud SQL for
PostgreSQL, reached only over a tailnet, as the component `envoy-infra:gcp:Dispatch`, with the two
Envoy changes a Dispatch pod on a tailnet needs: a SOCKS5 dialer in the NATS bus, and the NATS
permission set of Dispatch's one nkey user.

**Architecture:** three slices stacked on slice 4 of `docs/plans/2026-10-14-envoy-infra-plan.md`
(branch `envoy-infra-gcp`), each one pull request. Slice 5 adds one setting to
`packages/envoy/internal/bus` that routes every bus connection through a SOCKS5 proxy, which a pod's
userspace Tailscale container offers (`TS_SOCKS5_SERVER`). Slice 6 adds `core.DispatchPermissions`,
the grant of the nkey user `envoy-dispatch` and the `envoy-listener` beside it share, built from
names `internal/bus` and `internal/contracts` export, and lets `gcp:NatsHub` render it for a user.
Slice 7 adds `gcp:Dispatch`, a Go component beside `gcp:NatsHub` that declares an Autopilot cluster
of its own with private nodes and Cloud NAT, Cloud SQL on a private IP reached through the Cloud SQL
Auth Proxy, and one Deployment of one replica whose pod runs `envoy-dispatch`, `envoy-listener`, one
unprivileged userspace Tailscale container (Serve inbound on 443, SOCKS5 outbound to the hub) and
the Auth Proxy as a native sidecar; it builds an explicit Kubernetes provider from its own cluster.

**Tech stack:** Go 1.26.8; nats.go v1.50.0 (`CustomDialer`, `SkipHostLookup`);
`golang.org/x/net/proxy` v0.59.0 (SOCKS5 with the host name sent unresolved); pulumi-go-provider
v1.7.0 and Pulumi `sdk/v3` v3.265.0 (slice 4's pins); pulumi-gcp SDK v10.1.0 (slice 4's pin);
pulumi-kubernetes SDK v4.34.2 and pulumi-random SDK v4.21.2 (both require `sdk/v3` v3.259.0, so
minimum version selection keeps v3.265.0); testcontainers-go v0.41.0 with the pgvector image CI
already pins; Pulumi CLI v3.268.0 in CI.

**Spec:** the deploying organization's Dispatch spec, in its deployment repository (private), whose
rulings are copied below; `docs/plans/2026-10-14-envoy-infra-design.md` (the plugin's design);
`docs/plans/2026-10-14-dispatch-google-signin.md` on branch `dispatch-google-signin` (the Google
sign-in mode whose settings `gcp:Dispatch` passes). Every statement about existing code below cites
the file and line it was read at, at the slice 4 head (`envoy-infra-gcp`); anything not read is
marked `[INFERENCE]`.

## Global constraints

Everything under "Global constraints" in `docs/plans/2026-10-14-envoy-infra-plan.md` holds here,
in particular: Legion is public, so no file, commit, PR title, body, branch or comment names the
deploying organization, its project, its tailnet, a host or an account (placeholders:
`example-dispatch`, `example-tailnet.ts.net`, `example-project`, `example.com` for a Workspace
domain), and `.github/scripts/check-private-names.sh` passes at every head; every component input
holding a secret carries `provider:"secret"`; every component refuses an invalid plain input and a
missing provider before it registers anything; no defensive guards, no silent fallbacks; `gofmt`
clean, tests beside the code, CI's exact commands pass locally before every push. In addition:

- `packages/envoy/go.mod` and `go.sum` change only in Task 5.1 (`golang.org/x/net` becomes a
  direct requirement); `packages/envoy/infra/go.mod` and `go.sum` change only in Task 7.1
  (pulumi-kubernetes and pulumi-random); `packages/daemon/go.mod` and `go.sum` change only by the
  `golang.org/x/net` line Task 5.1 raises, `packages/envoy/infra/go.work.sum` only by the one
  checksum the go command adds for it there, and the root `go.work` and `go.work.sum` not at all
  (Task 5.1 step 3 says why `go work sync` is not the tool).
- Dispatch reads its environment only through its settings table
  (`packages/envoy/cmd/dispatch/settings.go:11-21`; `TestNoReaderBypassesTheSettingsTable`,
  `settings_test.go:134-147`, fails on any other read), so the new setting is a row there, and
  every row has a case in `TestEverySettingReachesItsReader` (`settings_test.go:1149-1153`).
- A permission set spells no subject of its own: it is built from names `packages/envoy/internal/bus`
  and `internal/contracts` export, and a subject Dispatch's code spells inline gets an exported
  name in `bus` that the code moves onto (`packages/envoy/infra/AGENTS.md`, "Permission sets").
- A live test proves a listener is up only by `/healthz` answering `"status":"healthy"`, and
  Dispatch only by `/healthz` answering 200 with a `schema_version`
  (`packages/envoy/AGENTS.md`, "Every pool's size is set in code": `store.Pool.Healthy` reads the
  highest applied schema version and `/healthz` reports it as `schema_version`).
- The bus's SOCKS5 tests reach only an in-process SOCKS5 server and a loopback NATS from `testnats`;
  the hostname they send through the proxy is `nats.example.test`, which resolves nowhere.

## Decisions carried from the design conversation

Sami's rulings (2026-10-14), recorded in the spec: Dispatch runs on GKE Autopilot; its database is
Cloud SQL for PostgreSQL; it is reached only over the deploying organization's tailnet, which is
closed (a member reaches their own devices and the Envoy services, nothing else); anyone with a
verified account of the organization's Workspace domain signs in (the Google sign-in mode is the
separate plan above); the organization shares no infrastructure with any other.

The coordinator's decisions inside those rulings, which this plan implements and never reopens:

1. **Tailnet transport.** One unprivileged userspace Tailscale container in the Dispatch pod does
   both directions: Serve (`TS_SERVE_CONFIG`) answers `https://<hostname>.<tailnet>.ts.net` and
   proxies to Dispatch on `127.0.0.1:8766`; its SOCKS5 proxy (`TS_SOCKS5_SERVER`) carries the NATS
   connections of Dispatch and of the pod's `envoy-listener` to the hub's MagicDNS name on 4222;
   its state lives in a Kubernetes Secret (`TS_KUBE_SECRET`) the pod's service account may read and
   write. No Tailscale Kubernetes operator, no Helm. The device carries a tag of its own
   (`tag:example-dispatch` here), owned by the deployment's automation tag.
2. **The bus gains a SOCKS5 dialer.** One setting that, when set, routes every bus NATS connection
   through a SOCKS5 proxy at that address, sending the host name unresolved so tailscaled resolves
   MagicDNS names. Dispatch reads it through its settings table, the listener through the bus's
   environment lookup. Tested against an in-process SOCKS5 server, with the negative controls
   (unset: no proxy is dialed; a dead address: the connect fails naming it).
3. **`core.DispatchPermissions`**: `core.HostPermissions` plus the subjects Dispatch itself
   publishes and consumes, written out by hand in its test, and proven by a `natslive` test that
   runs the real `envoy-dispatch` against a nats-server holding only that set and refuses nothing.
   The name and shape are the interface the separate `aws:Dispatch` plan also uses.
4. **`envoy-infra:gcp:Dispatch`** declares: an Autopilot cluster of its own (private nodes, Cloud
   NAT), Cloud SQL for PostgreSQL at the smallest dedicated tier (1 vCPU, 3.75 GB) on a private IP,
   reached through the Cloud SQL Auth Proxy as a native sidecar with `--private-ip`; a built-in
   database user with a generated password; one Deployment of one replica, strategy `Recreate`,
   termination grace 30 s, Guaranteed resources (Dispatch 1 vCPU / 4 GiB), readiness on `/healthz`
   and no liveness probe on it; containers `envoy-dispatch`, `envoy-listener` (Dispatch's
   `ENVOY_URL` is `http://127.0.0.1:9020`), the Tailscale container, the Auth Proxy; the one grant
   the component makes is resource-level, straight to the Kubernetes service account's Workload
   Identity principal: Secret Manager accessor on the NATS seed secret. `roles/cloudsql.client`
   on that principal is a project-level grant and is the consumer's, made by its bootstrap
   program, which its project owner applies (ruling, 2026-10-14: a deploy identity that may set
   project IAM can grant itself any role; R30). The NATS seed stays out of Pulumi state: `envoy-infra
   seed generate` writes it into Secret Manager and the pod reads it as a file
   (`NATS_NKEY_SEED_FILE`) through the GKE Secret Manager add-on. The cookie signing key, the agent
   bearer and the database URL are generated and held as Kubernetes Secrets.
5. **The Kubernetes provider** the component builds from its own cluster is explicit, under
   `internal/providers`' rule that no resource falls back to a default provider. R20 below chooses
   its kubeconfig's user.
6. **The consumer** (its own plan, in the deployment repository) runs `gcp:Dispatch` from a Pulumi
   program in Go and extends its tailnet policy with the device's tag and the grants
   `autogroup:member` and the hosts' tag to `tag:example-dispatch:443`, `tag:example-dispatch` to
   `tag:example-hub:4222` (the hosts' tag is `tag:example-host`).

Not in this version (the spec's list): meaning search (keyword search only, since embeddings use
AWS Bedrock, `settings.go:124-126`'s `COHERE_API_KEY` row says so); an S3 file store (files stay in
Postgres, `settings.go:43-44`); Dispatch's GitHub App and any webhooks; a monitoring alert policy
(`gcp:Dispatch` declares none; a Dispatch that is down shows as the browser failing to reach it).

## Rulings made while planning

- **R16. The setting is `ENVOY_NATS_SOCKS5_PROXY`, a `host:port`.** The bus's other variables are
  `ENVOY_ALLOW_REMOTE_NATS` (`internal/bus/reach.go:25`) and `NATS_NKEY_SEED[_FILE]`
  (`internal/bus/nkey.go:14-17`), so an `ENVOY_NATS_` name sits beside them, and a bare `host:port`
  is how Tailscale itself spells the proxy (`TS_SOCKS5_SERVER=:1055`, its Docker parameters page,
  read 2026-10-14). A value with a scheme, a missing port or an empty string is refused before
  anything dials, naming the variable; unset means a direct dial, as today.
- **R17. `golang.org/x/net/proxy` is the SOCKS5 client.** It sends a host that is not an IP as a
  domain name (`AddrTypeFQDN`, `golang.org/x/net@v0.59.0/internal/socks/client.go:96-103`), which
  is what lets tailscaled resolve the MagicDNS name; its `Dialer` implements `DialContext`
  (`internal/socks/socks.go:151`), so the handshake is bounded by the bus's 5 s connect timeout
  (`internal/bus/nats.go:154`). `golang.org/x/net` is already in `packages/envoy/go.sum` at v0.58.0
  and an indirect requirement of `packages/envoy/infra/go.mod` at v0.59.0 (`go.mod:191`); Task 5.1
  requires v0.59.0 in `packages/envoy/go.mod` so both modules compile the same copy. A hand-written
  SOCKS5 client was rejected: the protocol is small, but the standard package is the boring answer
  and is already in the module graph.
- **R18. With a proxy set, nats.go resolves no host name itself.** nats.go's `createConn` looks a
  host name up with `net.LookupHost` and dials each address unless `SkipHostLookup` is set
  (`nats.go@v1.50.0/nats.go:2256-2265`, `:555-556`). In a pod the cluster's resolver knows no
  MagicDNS name, and if it did answer the proxy would be handed an address rather than the name.
  So `connectWithContext` sets `SkipHostLookup` exactly when it sets `CustomDialer`, and the
  dialer receives `url.Host` as written (`:2283-2284`). The proxy applies to every connection: the
  first (`nats.go:333`), `Dial`'s (`:293`), the replacement a recovery dials (`nats.go:662`, in
  `ensureConnWithContext`, through the `Client`'s own field), and nats.go's in-place reconnects,
  which reuse `Opts`.
- **R19. `DispatchPermissions(machineID)` takes the machine id of the listener beside Dispatch,**
  and `gcp:Dispatch` sets that listener's `ENVOY_MACHINE_ID` to its `hostname`. The hub renders
  the grant for a user that names `dispatchMachineId` (Task 6.4), so the consumer writes the same
  string in both places; a mismatch leaves the listener's durable `listener-<id>` out of the grant
  and the listener never healthy, the shape slice 4's negative control proves
  (`core/permissions_live_test.go:95-113`). The set is `HostPermissions(machineID)` plus what
  Dispatch does on NATS, read from its code: two durable push consumers on the stream
  (`internal/dispatch/delivery/intake.go:227,246,274-281`: `delivery-events` filtered on
  `notifications.github.>`; `delivery/jobs.go:57-58,129-134`: `delivery-jobs` filtered on
  `notifications.github.*.*.job.>`), each looked up, created, updated in place, replaced at its ack
  floor and bound (`intake.go:298-355`); one KV bucket, `envoy_webhook_redelivery`
  (`internal/dispatch/redeliver/sweep.go:46,76-82`, through `bus.EnsureKeyValue`,
  `internal/bus/kv.go:28-35`: `KeyValue`, then `CreateKeyValue` when absent); the agent
  conversation relay, which subscribes to each session's frames and publishes its control subject
  (`internal/dispatch/agentstream/agentstream.go:65,97`, the reverse of a session's own
  directions); and the outbox, which publishes to the stream's own subjects and the role lanes
  (`internal/dispatch/outbox/publisher.go:30,304,631-633,651-654`), all already in the host's
  publish set (`core/permissions.go:50-52`). A durable with one filter subject is created on
  `$JS.API.CONSUMER.CREATE.<stream>.<durable>.<filter>` (nats.go `jsm.go:515-525`, `js.go:160`), so
  each durable's create entry is `…CONSUMER.CREATE.ENVOY_NOTIFICATIONS.<durable>.>`, as the
  listener's KV watchers' is (`permissions.go:67`), rather than the exact filtered subject: the
  filter is Dispatch's to change without a plugin release and a hub re-render. The listener's own
  durable uses `FilterSubjects` (`cmd/listener/durable.go:44-45`), which nats.go creates on the
  plain subject (`jsm.go:515-517`), which is why the listener's entry is exact.
  `$JS.API.STREAM.PURGE.ENVOY_NOTIFICATIONS` is not in the set: `ensureStreamWithConfig` sends it
  only while migrating role lanes off a stream that carried them (`internal/bus/stream.go:255-258`),
  which a hub rendered by this plugin never does.
- **R20. The kubeconfig's user runs `gke-gcloud-auth-plugin`.** The alternative, a kubeconfig
  carrying the gcp provider's client-config access token, needs no other binary and authenticates
  exactly as the gcp provider does, but the token lives about an hour, so the provider resource's
  input changes on every run (every `pulumi preview` shows the Kubernetes provider updating, which
  defeats the "preview shows no diff" discipline the plugin's consumers run on) and a token sits in
  state. The exec plugin keeps the kubeconfig a plain, stable document (endpoint, CA certificate,
  exec block) with no secret in it, and is what Pulumi's own GKE example does
  (`pulumi/examples` `gcp-go-gke/main.go:123-150`, read for the research memo). Its cost, stated:
  `gke-gcloud-auth-plugin` must be installed on every machine that runs `pulumi` against the stack,
  laptop and CI, and it takes its credentials from gcloud (the active or `CLOUDSDK_ACTIVE_CONFIG_NAME`
  configuration on a laptop; the credential file `google-github-actions/auth` writes in CI
  `[INFERENCE]`, which the first preview on each proves, hardening ledger). The plugin ships only
  as a Google Cloud SDK component: `gcloud components install gke-gcloud-auth-plugin` on an SDK
  install, the distribution package `google-cloud-cli-gke-gcloud-auth-plugin` where gcloud came
  from a package manager, and in GitHub Actions `google-github-actions/setup-gcloud` with a pinned
  `version` and `install_components: gke-gcloud-auth-plugin`; its version is the SDK's, so pinning
  the SDK pins the plugin. mise is not a route: its registry has no entry, and aqua's
  (`kubernetes/cloud-provider-gcp/gke-gcloud-auth-plugin`) is a `go_build` from source, not a
  pinned binary (both read 2026-10-14). The component exports the kubeconfig it built, so an
  operator's `kubectl` uses the same document. The README documents the token form as the
  alternative for a consumer that cannot install the plugin, with its two costs.
- **R21. Cloud SQL's private IP is private services access (one VPC peering),** not Private
  Service Connect: PSC needs a service connection policy or a manual endpoint plus DNS, and the Auth
  Proxy needs DNS names for a PSC instance (the research memo, `sql-psc`), where `--private-ip`
  against a PSA instance needs neither. The component reserves one `/16` for Google's services on
  its own network and connects `servicenetworking.googleapis.com` to it.
- **R22. Generated secrets are `random.RandomPassword` resources,** so a preview regenerates
  nothing and the values live in state as secrets (the consumer's stack encrypts with its own
  secrets provider). The consumer therefore passes a `random` provider beside its `gcp` one, and the
  component requires both (`internal/providers`). Letters and digits only (`Special: false`), so
  the database password needs no URL escaping in `DATABASE_URL`.
- **R23. No Kubernetes Service, and every server in the pod binds loopback.** Tailscale Serve is
  the only way in, and it proxies to `127.0.0.1:8766` inside the pod's network namespace; so
  Dispatch sets `DISPATCH_LISTEN_HOST=127.0.0.1`, the listener keeps its default
  (`internal/config/config.go:44-47`: `ENVOY_LISTEN_HOST` unset is `127.0.0.1`), which also means
  `/v1` needs no `ENVOY_API_TOKEN` (`cmd/listener/apiauth.go:87-92`), and Dispatch sends no
  `ENVOY_TOKEN`. Readiness probes are therefore `exec` probes with the image's `curl`
  (`packages/envoy/docker/Dockerfile:95`, which the image's own `HEALTHCHECK` uses, `:125-126`),
  never `httpGet` on the pod IP.
- **R24. The pod runs as the image's user and states no `runAsNonRoot`.** The image's user is the
  named system user `envoy` with no fixed uid (`Dockerfile:96`), and a kubelet cannot verify a
  non-numeric user against `runAsNonRoot`, so the pod sets neither it nor `runAsUser`; the Auth
  Proxy container, whose image runs as a numeric non-root user, sets `runAsNonRoot: true`. The
  first deployment proves Dispatch's containers run as a non-zero uid (hardening ledger).
- **R25. Cloud SQL runs `POSTGRES_16` in the `ENTERPRISE` edition on `db-custom-1-3840`.** The
  pgvector image Dispatch's tests and the dev Postgres run is PostgreSQL 16
  (`packages/envoy/scripts/dev-postgres.sh:15-17`), so the first production major is the one every
  test ran on; both are inputs with those defaults. The edition is explicit because pulumi-gcp
  defaults a PostgreSQL 16+ instance to `ENTERPRISE_PLUS`, where a custom machine type is refused
  (`pulumi-gcp/sdk/v10@v10.1.0/go/gcp/sql/pulumiTypes.go:1703-1711,1746`).
- **R26. No pre-deploy census Job.** `Recreate` stops the old pod before the new one starts
  (`strategy: Recreate`, the one strategy with no overlap), so the boot migration meets no
  connection of Dispatch's own; the census guards exactly that overlap. The README gives the
  `kubectl run` recipe for an operator who wants to run `envoy-dispatch census` by hand before a
  digest change.
- **R27. `deletionProtection` is an input, default `true`,** applied to the cluster and the
  database instance. pulumi-gcp's own default for both is `true` as well (the provider's schema;
  the SDK's field comments at `container/cluster.go:1850` and `sql/databaseInstance.go:882` name
  the field and not the default `[INFERENCE: the schema default is the upstream provider's
  documented one]`), so the component passes its value explicitly rather than leaving the field
  unset. Deleting a Cloud SQL instance deletes its automated backups with
  it, so the default stays on; a first deployment that is still being torn down and rebuilt sets
  it `false` in its stack configuration.
- **R28. Serve's configuration names the FQDN literally.** The component knows `hostname` and
  `tailnetDnsName`, so `serve.json`'s `Web` key is `<hostname>.<tailnetDnsName>:443` as written,
  and no `${TS_CERT_DOMAIN}` substitution is relied on. It is mounted as a directory
  (`/etc/tailscale`), which Tailscale's Docker parameters page requires for `TS_SERVE_CONFIG`.
- **R29. `core.ListenerConfig` gains `SOCKS5Proxy`, rendered by `ListenerEnv`,** so the pod's
  listener environment comes from the same builder as `host:Listener`'s (`core/listener_env.go:22-27,
  58-92`). `host:Listener` does not expose it (nothing on a Docker host needs it; slice 1's R9
  pattern).
- **R30. Every IAM write the component makes is resource-level.** There are two: the
  `secretmanager.SecretIamMember` giving the pod's principal `roles/secretmanager.secretAccessor`
  on the NATS seed secret, which needs `secretmanager.secrets.setIamPolicy` on that one secret and
  is covered by the consumer's deploy identity holding `roles/secretmanager.admin` on the project
  (which the first Google Cloud deployment's bootstrap already grants it for the seed secrets);
  and the Kubernetes `Role` and `RoleBinding` on the Tailscale state Secret, which are Kubernetes
  RBAC objects written through the cluster's API, reached by whoever created the cluster through
  `roles/editor`'s `container.*` permissions `[INFERENCE: the first CI apply proves it; the laptop's
  owner has them]`. The component makes no `projects.IAMMember`, no `serviceaccount.IAMMember`
  and no Google service account. `roles/cloudsql.client`, which the Auth Proxy needs
  (`cloudsql.instances.connect`) and which Cloud SQL offers only at project level, is the
  consumer's bootstrap's to grant to the pod's principal before the first apply; the component
  exports that principal as `principal`, documents its form so a bootstrap can spell it before any
  cluster exists (the project number and id, the namespace and the hostname are all known then),
  and the README names the grant as a prerequisite. Without it the Auth Proxy refuses to connect
  and logs the permission it lacks, `cloudsql.instances.connect`, and Dispatch's readiness never
  passes, so the omission is loud at the first apply.

## Slices

| Slice | PR | Outcome someone can try | Depends on |
| --- | --- | --- | --- |
| 5 | a new PR stacked on slice 4's (gh-stack, linear: `envoy-infra-core` ← `envoy-infra-gcp` ← `envoy-bus-socks5`), branch `envoy-bus-socks5` | `ENVOY_NATS_SOCKS5_PROXY=127.0.0.1:1055 envoy-listener …` reaches a NATS server only a SOCKS5 proxy can reach, by its name; `envoy-dispatch settings` lists the setting | slice 4 merged into the branch's base |
| 6 | a new PR stacked on slice 5's, branch `envoy-infra-dispatch-permissions` | a nats-server whose one user holds `core.DispatchPermissions("example-dispatch")` runs the real `envoy-dispatch` with no permissions violation; `gcp:NatsHub` renders that user from `dispatchMachineId` | Task 5.1 (the stack's order; the set itself needs only slice 4); E14's live test needs the intake fix on `main` (below) merged and merged into the stack |
| 7 | a new PR stacked on slice 6's, branch `envoy-infra-gcp-dispatch` | `envoy-infra:gcp:Dispatch` previews a cluster, a database and a pod from a YAML program; the first Google Cloud deployment signs in to `https://<hostname>.<tailnet>.ts.net` with Google | Tasks 5.1, 6.1-6.4 |

**The intake fix, outside this stack.** Dispatch's two delivery intakes bind their durables through
one `bus.Client` (`cmd/dispatch/main.go:371,374` → `bindDurableConsumer` → `client.Subscribe`,
`delivery/intake.go:350`), and that client keeps one JetStream subscription, unsubscribing the
earlier one when a second registers (`internal/bus/nats.go:368-379,420-438`), so only one durable
stays `PushBound` and a production Dispatch with a GitHub App silently stops delivering on one
intake today. The plan gate found it by reading (the lines above, at `main`); a separate change
reproduces it in `TestBothIntakesKeepTheirSubscriptions` and fixes it in its own
PR against `main` (branch `fix-dispatch-intake-subscriptions`), not in this stack. Slice 6's E14
asserts both durables `PushBound`, which holds only once that PR is merged and has reached the
stack; Task 6.3 says so. The fix reaches the stack from the bottom: `envoy-infra` (slice 1)
merges `origin/main`, then each layer merges the layer below it (`envoy-infra-core` merges
`envoy-infra`, `envoy-infra-gcp` merges `envoy-infra-core`, `envoy-bus-socks5` merges
`envoy-infra-gcp`, and so on up), so every PR's diff against its base keeps only its own change
and the fix's commits appear in no PR of the stack.

Releases: slice 5 cuts a `legion-envoy` image release (the listener and Dispatch binaries change)
and an `envoy-infra` plugin release from `release-envoy-infra.yaml` (`internal/bus` is compiled
into the plugin, `packages/envoy/infra/AGENTS.md`, "Release contract"), with no component change.
Slice 6 cuts both too (Dispatch's code moves onto `bus`'s names; `core` and `gcp:NatsHub` change).
Slice 7 cuts a plugin release, and an image release as well: Task 7.6 edits
`packages/envoy/cmd/dispatch/README.md`, a path inside `release-envoy-listener.yaml`'s push paths
(`packages/envoy/**` less `infra`) and `release-bump.sh`'s `packages/envoy/` filter, and a squashed
`feat(…)` subject bumps the minor version (`release-bump.sh:72-76`, the gate's citation); the PR
body says that image release carries no binary change.

**Which image runs where.** `DISPATCH_SIGNIN_DOMAIN` exists only on branch `dispatch-google-signin`
(its `cmd/dispatch/settings.go:49-58`); the slice-4 head has the pool's four rows alone
(`settings.go:49-56`), and no branch of this stack carries both the SOCKS5 dialer and the sign-in
mode. So the pre-merge proof (E17, Task 7.7) runs an image built from a throwaway integration
branch: `envoy-dispatch-integration`, cut from slice 6's head, merging the sign-in PR's head
(`git merge --no-ff origin/dispatch-google-signin`, conflicts resolved in favour of neither: both
sides add settings rows, so the merge is a table with both), pushed, and built by
`release-envoy-listener.yaml`'s `workflow_dispatch` from that branch, which publishes one immutable
`ghcr.io/sjawhar/legion/envoy:<commit sha>` and moves no tag (`packages/envoy/AGENTS.md`, the
`release-envoy-listener.yaml` paragraph: a branch dispatch publishes one immutable image for a
dev slot to pin before merge). The consumer pins that image by digest (`docker buildx imagetools
inspect ghcr.io/sjawhar/legion/envoy:<sha>` for the digest) for E17 alone; the branch is deleted
once E17 is recorded and nothing else is built from it. The first real deployment pins a
`legion-envoy-v*` release once the sign-in PR and slices 5-6 are all merged (its setting names
are strings to `gcp:Dispatch`; an image without the mode refuses boot with the sign-in domain set,
`dispatch-google-signin` plan D1, so a wrong pin is loud).

A stacked branch takes its lower layer's later commits by merging it, never by a rebase and a force
push, and after a lower layer merges, the branch above merges `origin/main` before its PR is
retargeted (the rule under "Slices" in `docs/plans/2026-10-14-envoy-infra-plan.md`).

---

## Slice 5: the bus's SOCKS5 dialer

Branch `envoy-bus-socks5` in its own clone (`~/.worktrees/legion/envoy-bus-socks5`), stacked on
`envoy-infra-gcp`. Task 5.1 first; 5.2 needs 5.1; 5.3 needs both.

### Task 5.1: `ENVOY_NATS_SOCKS5_PROXY` in `internal/bus`

**Files:**
- Create: `packages/envoy/internal/bus/socks5.go`, `socks5_test.go`, `socks5_dialer_test.go`
- Modify: `packages/envoy/internal/bus/nats.go` (`connectOpts`, `connect`, `connectWithContext`,
  `Dial`, `newClient`, the `Client` struct, and the replacement dial in
  `ensureConnWithContext`), `reach.go` (Finding 12: a set proxy is remote reach),
  `.github/workflows/envoy-and-contracts.yaml` (`./internal/bus/` joins the `envoy-go-race`
  job's packages), the two tests
  that call `connectWithContext` with today's arity, `connect_deadline_test.go:22` and
  `stream_config_test.go:103,132` (each gains the new `nil` dialer argument),
  `remote_nats_test.go` (the proxy-is-remote case), `packages/envoy/go.mod`, `go.sum`
  (`golang.org/x/net v0.59.0` direct), `packages/daemon/go.mod` and `go.sum` (its
  `golang.org/x/net` line only), `packages/envoy/infra/go.work.sum` (the checksum the go command
  adds), `packages/envoy/AGENTS.md` (the "NATS credential" row of "Where to
  look" names the three variables a connect reads; it gains the fourth)

**Interfaces:**

```go
// SOCKS5ProxyEnvVar names the variable that sends every bus NATS connection through a SOCKS5
// proxy, as host:port. Unset, the bus dials each server directly. Set, the proxy is handed each
// server's host name as the URL spells it, never resolved here, so a proxy that knows names this
// process's resolver does not (tailscaled's SOCKS5 server and MagicDNS) connects to them.
const SOCKS5ProxyEnvVar = "ENVOY_NATS_SOCKS5_PROXY"

// socks5Dialer is the nats.CustomDialer a proxied connect installs. Each Dial is bounded by
// timeout: the bus's connect timeout, or what remains of a connect's deadline when that is shorter
// (bounded), since nats.go applies a connect's own timeout only to its default dialer. It records
// the last error a Dial returned, which connectWithContext names when nats.go reports only
// ErrNoServers (nats.go discards "connection refused"). The record is shared by every copy bounded
// makes and is safe for concurrent use: nats.go dials from its own reconnect goroutine
// (nats.go@v1.50.0:3038, doReconnect → createConn), and a client's background recovery and a
// publish's replacement dial can connect at once, each storing while the other loads.
type socks5Dialer struct {
	address string              // the proxy's host:port, for the error that names it
	proxy   proxy.ContextDialer // golang.org/x/net/proxy.SOCKS5 over a net.Dialer
	timeout time.Duration
	lastErr *atomic.Pointer[error]
}

// bounded is d with its timeout cut to remaining when that is shorter, sharing d's record.
func (d *socks5Dialer) bounded(remaining time.Duration) *socks5Dialer

// Dial connects through the proxy and records the error it returns.
func (d *socks5Dialer) Dial(network, address string) (net.Conn, error)

// LastError is the error the latest Dial returned, or nil.
func (d *socks5Dialer) LastError() error

// Address is the proxy's host:port.
func (d *socks5Dialer) Address() string

// socks5Proxy reads SOCKS5ProxyEnvVar through lookup. Unset is (nil, nil): a direct dial. Set, the
// value must be host:port (CheckSOCKS5Proxy); anything else is an error naming the variable, before
// anything dials. The *socks5Dialer it returns is the one value connectWithContext takes as its
// dialer, so the connect reads the address and the last error from it and never reads the lookup
// itself.
func socks5Proxy(lookup func(string) (string, bool)) (*socks5Dialer, error)
```

`connectWithContext` takes a `dialer *socks5Dialer` after `credential` (a typed nil when no proxy
is set, never an interface holding a nil pointer); when it is not nil the options get
`next.CustomDialer` and `next.SkipHostLookup = true` (R18). The dialer installed is
`dialer.bounded(remaining)` when the connect has a deadline, so a proxied attempt ends with it as
a direct one does. `connect`, `Dial` and `newClient` read the dialer from the lookup they already
read the credential from, and `Client` keeps it in a `dialer` field beside `credential` for the
replacement connection `ensureConnWithContext` dials.

Two rules the dialer carries (Findings 4 and 12 of the plan gate):

- **A set proxy is remote reach.** With a proxy, a loopback URL no longer names this machine's
  server: the proxy chooses the destination. `refuseRemoteNATS` (`reach.go`) refuses when
  `SOCKS5ProxyEnvVar` is set and `AllowRemoteEnvVar` is not `"1"`, with an error naming both
  variables and not the proxy's value, before anything dials. The dev sign-in fence's NATS guard
  is `ENVOY_ALLOW_REMOTE_NATS` alone (`cmd/dispatch/main.go:634-636`), so it keeps a proxy out too.
  Every planned deployment already sets `ENVOY_ALLOW_REMOTE_NATS=1`: the pod's Dispatch
  environment (Task 7.4) and `ListenerEnv` for any non-loopback URL (`core/listener_env.go:67-87`).
- **A failed proxied connect is named.** Once a dialer is set, a deferred wrap gives every error
  `connectWithContext` returns the prefix `bus: NATS connect through the SOCKS5 proxy
  ENVOY_NATS_SOCKS5_PROXY=<address> failed:`, whatever failed: the ten attempts ran out, or the
  context ended first, as a publish's 5 s replacement dial does. nats.go discards a dial error
  containing "connection refused" and returns `ErrNoServers` (`nats.go@v1.50.0/nats.go:2705-2716`),
  which names no address, so when the attempts end in `ErrNoServers` the wrapped error is
  `dialer.LastError()`. The record is written on the goroutine nats.go dials from and read by
  another connect on the same dialer, so it is an `atomic.Pointer[error]`, and
  `TestConnectsSharingADialerRecordItsLastError` runs two connects on one dialer at once under the
  race detector: Task 5.1 adds `./internal/bus/` to the `envoy-go-race` job's package list (the
  one step that runs `-race`; `envoy-go`'s `go test` does not). With the record a plain field,
  that test fails there.

`bus` exports the address check, `func CheckSOCKS5Proxy(value string) error`, which `socks5Proxy`
calls and `core.ListenerConfig.CheckPlain` calls too (Task 7.2), so the rule is spelled once. It
refuses an empty value, a value holding an nkey seed (`bus.HoldsSeed`, the detector `nkey.go`
shares with the plugin's refusals: a seed of any type in either case, inside a longer run, or split
by `.`, `-`, `_` or whitespace; a seed is a valid DNS label, and an admitted address is printed by
a failed connect), whitespace around the value, a scheme, a value `net.SplitHostPort` cannot
split, an empty host, an IPv6 address with a zone (every proxy a deployment names is loopback, and
a zone is free text), a host that is neither an IP address nor a DNS name as Go's resolver reads
one (labels of 1 to 63 letters, digits, hyphens and underscores, at most 253 bytes, so userinfo, a
path, whitespace and control bytes are refused), a port that is not a number, and a port outside
1-65535. Its error says what is wrong and never prints the value, which can carry credentials and
reaches the process's log. `TestAProxyHoldingAnNkeySeedIsRefused` covers a seed as the whole
value, the host, a label and a lower-case label, an account seed, a label with a base32 letter
before or after the seed, and a seed split across labels by `.`, `-` and `_` (user and account,
upper and lower case), each refused at once and printing none of the seed. Its controls are a
58-letter base32 label that is a seed with one character changed, whole and split across labels,
which pass the check and dial.

- [ ] **Step 1: failing tests** in `socks5_test.go`. A `fakeSOCKS5` server in the test: it accepts
  on a loopback port, reads the greeting (`05 <n> <methods>`), answers `05 00`, reads a CONNECT
  request (`05 01 00 <atyp> <addr> <port>`), records the address type and the address as text, dials
  the real `testnats.URL(t)` server whatever address was asked (so a name that resolves nowhere is
  served), answers `05 00 00 01 00000000 0000`, then copies bytes both ways until either side closes;
  `connects()` returns the recorded requests; `dropAll()` closes every relayed connection. Every
  case's lookup carries `"ENVOY_ALLOW_REMOTE_NATS": "1"` unless it says otherwise. Cases:
  1. `TestSOCKS5ProxyCarriesEveryConnectionWithTheHostUnresolved`: `bus.Connect([]string{"nats://nats.example.test:4222"}, bus.WithEnvironment(lookup{"ENVOY_NATS_SOCKS5_PROXY": fake.addr, …}))`
     connects; the fake recorded exactly one request, address type `0x03` (a domain name) and address
     `nats.example.test:4222`; a publish and a core subscribe round-trip through it; `fake.dropAll()`
     makes nats.go reconnect in place, and the fake's count reaches 2 with the same address. The same
     for `bus.Dial("test", urls, lookup)` and `bus.ConnectOwningStream`.
  1a. `TestSOCKS5ProxyIsHandedANameTheResolverKnows`: the URL is `nats://localhost:<testnats port>`
     (a name the runner resolves); the fake recorded address type `0x03` and `localhost:<port>`,
     never an IP. This is the case that pins `SkipHostLookup` (R18): without it nats.go resolves
     `localhost` and hands the dialer `127.0.0.1:<port>` (`nats.go@v1.50.0:2256-2265`), which case 1
     cannot see because `nats.example.test` resolves nowhere and falls back to the name anyway.
  2. `TestNoProxyIsDialedWhenTheSettingIsUnset`: `t.Setenv(bus.SOCKS5ProxyEnvVar, fake.addr)` so
     the process environment names the fake; `bus.Connect([]string{testnats.URL(t)}, bus.WithEnvironment(lookup{}))`,
     a lookup that lacks the variable, connects; `client.Conn.ConnectedAddr()` is the NATS server's
     own address and `fake.connects()` is empty. A second case with no `WithEnvironment` (the
     process environment, `ENVOY_ALLOW_REMOTE_NATS=1` set the same way) goes through the proxy:
     the two together show the lookup, not the process environment, decides.
  3. `TestAFailedConnectThroughAProxyNamesIt`: three proxies, each through `bus.Connect` and
     `bus.Dial`, every one on loopback: `127.0.0.1:1` (nothing listens; `settings_test.go`'s
     `localNATS` uses the same port for the same reason), a fake that closes the connection without
     answering, and a fake that answers the CONNECT `05 04` (host unreachable). Each error contains
     `ENVOY_NATS_SOCKS5_PROXY` and the proxy's address; the first also `connection refused` (the
     recorded dial error, since nats.go's own result would be the bare `nats: no servers available
     for connection`), the third `host unreachable`. Each takes the bus's ten attempts a second
     apart, about 10 s; the subtests run in parallel.
  3a. `TestAPublishWhoseDeadlineEndsFirstNamesTheProxy`: a client connected through a fake, whose
     proxy and connection then go away; `PublishCore`'s replacement dial ends on its 5 s deadline
     before its ten attempts, and the error is `context.DeadlineExceeded` naming
     `ENVOY_NATS_SOCKS5_PROXY` and the proxy's address.
  3b. `TestAProxiedDialEndsAtTheConnectsDeadline` and `TestConnectsSharingADialerRecordItsLastError`
     (internal, `socks5_dialer_test.go`): a proxy that never answers holds the dial for the
     connect's one-second deadline, not the 5 s connect timeout; two connects on one dead-proxy
     dialer at once both name the proxy and `connection refused`.
  4. `TestAMalformedProxyIsRefusedBeforeAnyDial`: every value `CheckSOCKS5Proxy` refuses (a scheme,
     no port, no host, `socks`, `+1`, `0`, `65536`, `""`, whitespace around or inside, an IPv6
     zone, userinfo, a path, a control byte, an empty label, a label or a name too long) fails
     `bus.Connect` and `bus.Dial` in under a second, naming `ENVOY_NATS_SOCKS5_PROXY`, and the fake
     saw no connection; an out-of-range port is refused as out of range.
     `TestAWellFormedProxyIsAccepted` passes IPv4, bracketed IPv6, `localhost`, a trailing dot,
     leading-zero and highest ports, hyphens and underscores, and the longest label and name.
     `TestARefusalNeverPrintsTheProxyValue` holds that a value carrying a secret (in userinfo, a
     URL or an IPv6 zone) appears in no refusal of `Connect`, `ConnectOwningStream` or `Dial`, with
     or without `ENVOY_ALLOW_REMOTE_NATS=1`.
  5. `TestASetProxyIsRemoteReach` (in `remote_nats_test.go`, beside its cases): a loopback URL with
     `ENVOY_NATS_SOCKS5_PROXY` set and `ENVOY_ALLOW_REMOTE_NATS` unset is refused with
     `bus.ErrRemoteNATS`, the error naming both variables, before any dial (the fake saw nothing),
     for `Connect`, `ConnectOwningStream` and `Dial`; with `ENVOY_ALLOW_REMOTE_NATS=1` the same
     connect goes through the proxy.
- [ ] **Step 2: run them; they fail** (`cd packages/envoy && go test ./internal/bus/ -run 'SOCKS5|Proxy'`).
- [ ] **Step 3: implement**: `socks5.go`
  (`CheckSOCKS5Proxy`, `socks5Proxy`, `*socks5Dialer` with its address and atomic last error), the
  `connectWithContext` change and its three production callers plus the two test callers, the
  `Client.dialer` field, `nats.go:662`, `reach.go`'s proxy rule. Then the requirement, each module
  on its own (`GOWORK=off`): in `packages/envoy`, `GOWORK=off go get golang.org/x/net@v0.59.0 &&
  GOWORK=off go mod tidy` (`go.mod` gains one direct requirement; `go.sum` swaps the two
  `golang.org/x/net` lines); in `packages/daemon`, `GOWORK=off go get golang.org/x/net@v0.59.0`
  alone, which raises `go.mod:117` (`golang.org/x/net v0.58.0 // indirect`) to v0.59.0 and adds
  its two `go.sum` lines. The daemon needs the raise because it requires `github.com/sjawhar/envoy`
  through `replace … => ../envoy`, so a build of it outside the workspace reads Envoy's new
  requirement. Not `go work sync`, and no `go mod tidy` in `packages/daemon`: at the base
  (`envoy-infra-gcp` 5194ca07) the workspace is already out of sync, and `go work sync` on an
  untouched checkout of it rewrites unrelated lines (in Envoy's `go.mod`: `gorilla/websocket` to a
  pseudo-version, `golang.org/x/time` v0.12.0 to v0.15.0, OpenTelemetry v1.41.0 to v1.44.0, gRPC
  v1.79.1 to v1.83.1; in the daemon's: seven aws-sdk indirect lines pruned and one added), and
  `go mod tidy` in `packages/daemon` prunes those same aws-sdk lines; both would break the module
  rule under "Global constraints". The
  plugin's workspace (`packages/envoy/infra/go.work`) then needs `golang.org/x/net
  v0.58.0/go.mod`'s checksum, which Envoy's tidied `go.sum` no longer carries, and the go command
  adds it to `packages/envoy/infra/go.work.sum` on the first build there. That daemon `go.mod`
  line makes `release.yaml`'s `legion` job cut a `legion-v*` release with no daemon code change:
  its version step reads commits under `packages/daemon/`, and `release-bump.sh` makes a
  `feat(…)` subject a minor bump. The worker image is rebuilt either way, since `release.yaml`'s
  `legion` filter covers `packages/envoy/**`. Slice 5's PR body says so.
- [ ] **Step 4: pass**: `go test ./internal/bus/` and `go test -race ./internal/bus/` (the
  `envoy-go-race` job's command for the package), `go vet ./...` in `packages/envoy`; from
  `packages/envoy/infra`, `go build ./...` (the plugin compiles `bus`; `golang.org/x/net` stays an
  indirect requirement there at v0.59.0, so its `go.mod` does not change). No list of release
  paths needs an edit: `release-envoy-infra.yaml`'s version step cuts a version from what
  `go list -deps ./cmd/...` says the plugin and the command compile, `bus` among them
  (`packages/envoy/infra/AGENTS.md`, "Release contract").
- [ ] **Step 5: commit** `feat(envoy): bus dials NATS through a SOCKS5 proxy when ENVOY_NATS_SOCKS5_PROXY is set`.

### Task 5.2: the setting in Dispatch's table

**Files:** modify `packages/envoy/cmd/dispatch/settings.go` (one row), `settings_test.go` (one
case), `docs/site/src/content/docs/dispatch/running-dispatch.md` (one sentence under its NATS
settings, placeholder address only); the configuration reference is generated from the table
(`docs/site/generators/dispatch-config.ts`, cited by the sign-in research memo), so it needs no
edit.

- [ ] **Step 1: failing test.** A case `"ENVOY_NATS_SOCKS5_PROXY"` in `TestEverySettingReachesItsReader`
  (`settings_test.go:427-1153`), beside `"ENVOY_ALLOW_REMOTE_NATS"` (`:711-723`): with
  `"ENVOY_ALLOW_REMOTE_NATS": "1"` and `"ENVOY_NATS_SOCKS5_PROXY": "socks5://127.0.0.1:1055"`,
  `natsConnect(values, localNATS)` fails with the address check's refusal (`is a URL`, naming
  `ENVOY_NATS_SOCKS5_PROXY`, not `bus.ErrRemoteNATS`, so the reach refusal cannot pass it in the
  check's place); with `"ENVOY_ALLOW_REMOTE_NATS": "1"` and the address
  of a refusing SOCKS5 fake (about 30 lines in `cmd/dispatch`'s test package: it reads the
  greeting and the CONNECT, records the request, answers `05 05 00 01 00000000 0000`, connection
  refused, and relays nothing; `bus` exports no test helper), `natsConnect(values, localNATS)`
  fails and the fake recorded at least one request for `127.0.0.1:1` with address type `0x01`
  (the bus makes ten attempts, so the count is ten over about 10 s, not one): the table's value
  reached the bus. Until the row exists the case fails in `settingValues.lookup`'s panic
  (`settings.go:182-188`), which is the test the table is for.
- [ ] **Step 2: the row**, after `NATS_NKEY_SEED` (`settings.go:71-72`):
  `{Name: "ENVOY_NATS_SOCKS5_PROXY", Required: "no", Description: "SOCKS5 proxy, as `host:port`, that every NATS connection Dispatch makes goes through, each server's host name handed to the proxy unresolved; unset, Dispatch dials NATS directly. A set proxy needs `ENVOY_ALLOW_REMOTE_NATS=1`, since the proxy chooses the server. A deployment that reaches NATS through a tailnet sidecar's SOCKS5 server (tailscaled's `TS_SOCKS5_SERVER`) sets it."}`.
  No reader changes: the server, `redeliver-webhooks` and the agent conversation relay already hand
  the table's lookup to the bus (`main.go:164,185`, `redeliver.go:71`).
- [ ] **Step 3: pass** `go test ./cmd/dispatch/ -run 'Settings|Reader'`; `envoy-dispatch settings`
  lists the row.
- [ ] **Step 4: commit** `feat(dispatch): ENVOY_NATS_SOCKS5_PROXY in the settings table`.

### Task 5.3: the slice's proof

- [ ] CI's exact commands for the Envoy module pass locally (`envoy-go`'s `go vet ./...` and
  `go test ./...` in `packages/envoy`, with `DISPATCH_TEST_DATABASE_URL` from
  `scripts/dev-postgres.sh`); `envoy-infra-go`'s commands in `packages/envoy/infra`
  (`packages/envoy/infra/AGENTS.md`, "Tests").
- [ ] Open the PR stacked on slice 4's with `gh stack`; its body names the setting, R16-R18 and the
  ~10 s negative control; `check-pr-text.sh` passes. After merge, `release-envoy-listener.yaml`'s
  run id and the `legion-envoy-v*` tag, and `release-envoy-infra.yaml`'s `envoy-infra-v*` tag, go on the PR as
  the slice's production check.

---

## Slice 6: `core.DispatchPermissions`

Branch `envoy-infra-dispatch-permissions` in its own clone, stacked on `envoy-bus-socks5`. Task 6.1
first; 6.2 needs 6.1; 6.3 needs 6.2; 6.4 needs 6.2.

### Task 6.1: Dispatch's NATS names move into `bus`

**Files:** modify `packages/envoy/internal/bus/stream.go` (three exported constants beside
`ListenerDurable`, `:36-40`), `packages/envoy/internal/dispatch/delivery/intake.go:227`,
`delivery/jobs.go:57`, `redeliver/sweep.go:46`, `packages/envoy/AGENTS.md` ("Stream definition"
row: the names envoy-infra's grants are built from).

- [ ] **Step 1: failing test.** `bus/listener_names_test.go` gains the three names as literals:
  `bus.DispatchDeliveryDurable == "delivery-events"`, `bus.DispatchJobDurable == "delivery-jobs"`,
  `bus.WebhookRedeliveryBucket == "envoy_webhook_redelivery"`, the independent check slice 4's
  Task 4.1 gave `ListenerDurable`.
- [ ] **Step 2: the names**, each with a comment saying which Dispatch package binds or opens it:

```go
// DispatchDeliveryDurable is the stream's durable consumer Dispatch's delivery intake binds, over
// every GitHub notification (internal/dispatch/delivery).
const DispatchDeliveryDurable = "delivery-events"

// DispatchJobDurable is the stream's durable consumer Dispatch's job intake binds, over a deploy
// run's job events (internal/dispatch/delivery).
const DispatchJobDurable = "delivery-jobs"

// WebhookRedeliveryBucket holds the cursor and records of Dispatch's sweep of the GitHub App
// webhook's failed deliveries (internal/dispatch/redeliver).
const WebhookRedeliveryBucket = "envoy_webhook_redelivery"
```

  The three definitions move onto them: `const deliveryConsumerName = bus.DispatchDeliveryDurable`
  (`intake.go:227`), `jobConsumerName = bus.DispatchJobDurable` (`jobs.go:57`), and
  `Bucket = bus.WebhookRedeliveryBucket` (`sweep.go:46`; `redeliver.Bucket` stays exported for its
  callers). No test under `internal/dispatch` spells the literals (the gate's `git grep`), so
  `bus`'s literal test is the independent check.
- [ ] **Step 3: pass** `go test ./internal/bus/ ./internal/dispatch/delivery/ ./internal/dispatch/redeliver/`.
  `git grep -n -e '"delivery-events"' -e '"delivery-jobs"' -e '"envoy_webhook_redelivery"' -- packages/envoy`
  finds `bus/stream.go` and `bus/listener_names_test.go` only.
- [ ] **Step 4: commit** `refactor(envoy): Dispatch's durable and bucket names exported from bus`.

### Task 6.2: the set, written out by hand in its test

**Files:** modify `packages/envoy/infra/core/permissions.go`, `permissions_test.go`.

**Interfaces:**

```go
// DispatchPermissions is the grant of Dispatch's one NATS user, which envoy-dispatch and the
// envoy-listener beside it share: HostPermissions(machineID), the listener's machine id, then what
// Dispatch itself needs. Publish, in this order: for each durable Dispatch binds (bus.DispatchDeliveryDurable,
// bus.DispatchJobDurable) its info, its create on the filtered form nats.go sends for a consumer
// with one filter subject (CONSUMER.CREATE.<stream>.<durable>.>), its delete (a replacement at its
// ack floor) and its acknowledgements; the KV calls for bus.WebhookRedeliveryBucket, as for a
// listener's buckets; and every session's agent stream control subject
// (contracts.AgentStreamControlSubject), which the conversation relay publishes. Subscribe: every
// session's agent stream frames (contracts.AgentStreamFramesSubject), which the relay reads.
func DispatchPermissions(machineID string) Permissions

// bucketPermissions are the calls nats.go's KV makes on one bucket, in the order the grants list
// them: ListenerPermissions lists them for each of listenerBuckets, DispatchPermissions for the
// redelivery bucket. Extracted from ListenerPermissions unchanged, so the listener's set keeps its
// order byte for byte (its hand-written test holds it).
func bucketPermissions(bucket string) []string
```

- [ ] **Step 1: failing test.** `TestDispatchPermissionsForExampleDispatch` writes the whole set
  for `example-dispatch` by hand: `HostPermissions("example-dispatch")`'s publish entries as the
  existing hand-written test spells them, then exactly these, in order:

```text
$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS.delivery-events
$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.delivery-events.>
$JS.API.CONSUMER.DELETE.ENVOY_NOTIFICATIONS.delivery-events
$JS.ACK.ENVOY_NOTIFICATIONS.delivery-events.>
$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS.delivery-jobs
$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.delivery-jobs.>
$JS.API.CONSUMER.DELETE.ENVOY_NOTIFICATIONS.delivery-jobs
$JS.ACK.ENVOY_NOTIFICATIONS.delivery-jobs.>
$JS.API.STREAM.INFO.KV_envoy_webhook_redelivery
$JS.API.STREAM.CREATE.KV_envoy_webhook_redelivery
$JS.API.DIRECT.GET.KV_envoy_webhook_redelivery.>
$JS.API.STREAM.MSG.GET.KV_envoy_webhook_redelivery
$KV.envoy_webhook_redelivery.>
$JS.API.CONSUMER.CREATE.KV_envoy_webhook_redelivery.>
$JS.API.CONSUMER.DELETE.KV_envoy_webhook_redelivery.>
$JS.FC.KV_envoy_webhook_redelivery.>
agentstream.*.control
```

  and the subscribe entries: the host's (`_INBOX.>`, `notifications.role.> envoy-listener-example-dispatch`,
  `notifications.>`, `agentstream.*.control`), then `agentstream.*.frames`. A second check: every
  entry of `HostPermissions` is in `DispatchPermissions`, in both directions, and the existing
  `ListenerPermissions`/`HostPermissions` tests still pass unchanged.
- [ ] **Step 2: implement** with `bucketPermissions` extracted; pass `go test ./core/`
  (`cd packages/envoy/infra`; `core`'s real-server tests start nats-server through `testnats`).
- [ ] **Step 3: commit** `feat(envoy-infra): core.DispatchPermissions from Envoy's own names`.

### Task 6.3: the `natslive` proof against the real `envoy-dispatch`

**Files:** create `packages/envoy/internal/testpg/testpg.go` (a Postgres test container, the shape
of `internal/testnats`: `func URL(t testing.TB) string` starts
`mirror.gcr.io/pgvector/pgvector@sha256:7b822b0aac60967beb1ea5e576b8602c94c300a157d187f385ae3e0da199b90a`,
the digest CI's Postgres service pins (`.github/workflows/envoy-and-contracts.yaml:635`), through
testcontainers-go with `POSTGRES_PASSWORD=test`, waits for the log line
`database system is ready to accept connections` twice, and returns
`postgres://postgres:test@127.0.0.1:<port>/postgres?sslmode=disable`; the container ends with the
test), `packages/envoy/infra/core/dispatch_permissions_live_test.go` (build tag `natslive`); modify
`.github/workflows/envoy-and-contracts.yaml` (`envoy-infra-go` pulls the pgvector image in the step
that pulls `testnats.Image` and `testnats.HubImage`, for the reason that step gives) and
`packages/envoy/infra/AGENTS.md` ("Tests": the new live test and what it needs).

- [ ] **Step 1: failing live test.** `TestDispatchPermissionsOnRealServers`, on both images as
  `permissions_live_test.go:54-56` runs them, each with `hubServerArgs`:
  1. `buildDispatch(t)` builds `packages/envoy/cmd/dispatch` as `buildListener` builds the
     listener (`permissions_live_test.go:117-127`: `go build` in the command's directory, under the
     root `go.work`).
  2. `startHostServer(t, image, core.DispatchPermissions("example-dispatch"))` (`:129`), and
     `testpg.URL(t)`.
  3. A GitHub fake: an `httptest.Server` on loopback answering every App call the boot and the
     intakes make with the minimum each accepts (`GET /app` with `{"id":1,"slug":"example","name":"Example"}`,
     `GET /app/installations` with `[]`, `GET /app/hook/deliveries` with `[]`, anything else 404
     with its path logged), the shape `packages/dispatch/e2e/run-server.sh:90-109` gives Dispatch
     (`DISPATCH_APP_CLIENT_ID=Iv1.e2efake`, a PEM from `openssl genrsa 2048` base64-encoded, the fake's
     URL as `DISPATCH_GITHUB_API_BASE`); the test generates the key with `crypto/rsa` and records
     every path the fake was asked, so a call the fake did not expect fails the test naming it.
     Then run `envoy-dispatch` with `cmdtest` as `startListener` runs the listener, with the
     environment: `DATABASE_URL=<testpg URL>`, `DISPATCH_AGENT_TOKEN=example-agent-token`,
     `DISPATCH_DEV_SIGNIN=1`, `DISPATCH_INSECURE_COOKIE=1`, `DISPATCH_LISTEN_HOST=127.0.0.1`,
     `DISPATCH_PORT=<free port>`, `DISPATCH_SERVER_URL=http://127.0.0.1:<port>`,
     `NATS_URLS=<server uri>`, `NATS_NKEY_SEED=<seed>`, `DISPATCH_APP_CLIENT_ID`, `DISPATCH_APP_PEM_B64`,
     `DISPATCH_GITHUB_API_BASE=<fake URL>`, `DISPATCH_WEB_DIST=<empty temp dir>` (any configured
     directory is accepted, `main.go:411-416`), `HOME=<temp dir>` and `XDG_CONFIG_HOME=<temp dir>/config`
     (so no user `envoy.json` is read); no `DISPATCH_SIGNING_KEY` (the dev sign-in fence refuses one,
     `main.go:631-633`: the key is per process under dev sign-in); no `ENVOY_ALLOW_REMOTE_NATS`
     (the server URL is loopback and the fence refuses it, `main.go:634-636`); no `ENVOY_URL` (the
     listener's default is reached by nothing in this test). `devSignInLoadedFence` admits an App
     from the environment whose API base is loopback (`main.go:659-676`). Require `/healthz` to
     answer 200 with `"schema_version"` within 120 s: the boot runs every migration, pgvector's
     `create extension` among them (`internal/dispatch/store/migrations/0072_embeddings_core.up.sql:30`,
     cited by the research memo).
  4. As the same user: `js.ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)` and
     `(…, bus.DispatchJobDurable)` answer, each `PushBound`, within 20 s (as `delivery/jobs_test.go:179-182`
     waits), which is the filtered create under the grant. Both intakes run only with the App
     (today `delivery/intake.go:78-82` and `jobs.go:103-107`, each logging that it is idle and
     returning; after the intake fix, `delivery.StartIntakes`, which gates both at once), which is
     why step 3 configures one; both stay bound only with the intake fix on `main` having reached
     this stack from the bottom (the Slices section), which this step therefore proves too.
  5. Subscribe to `notifications.dispatch.issue.>`; `POST /api/v1/projects {"key":"ACME","name":"Acme","actor":{"kind":"session","id":"s1"}}`
     and `POST /api/v1/issues {"project":"ACME","title":"example","actor":{…}}` with
     `Authorization: Bearer example-agent-token` (both routes are `authAny`,
     `internal/dispatch/api/routes_table.go:55,82`); the subscription receives the
     `issue.created` envelope within 10 s: the outbox published under the grant.
  6. `GET /auth/_dev/signin?login=alice@example.com` for a session cookie (the dev sign-in route,
     `main.go:153-155`'s warning names it); subscribe to `contracts.AgentStreamControlSubject("s1")`;
     open `GET /api/v1/agents/s1/stream` with the cookie (`routes_table.go:109`, human-only) and
     read its SSE; the control subscription receives `{"v":1,"type":"watch"}`
     (`agentstream.go:30,97`); publish `{"v":1,"kind":"message","seq":1}` on
     `contracts.AgentStreamFramesSubject("s1")` as the user; the SSE delivers `event: frame` with
     that data (`agent_stream_test.go:95-98`'s shape).
  7. With the App configured the redelivery sweeper opens its bucket itself (`main.go:352-362`,
     `webhookSweeper`; its first sweep lists the fake's empty deliveries): `js.KeyValue(bus.WebhookRedeliveryBucket)`
     as the user answers within 20 s (the bucket exists, created under the grant), Dispatch's
     output holds `dispatch: redelivering the GitHub App webhook's failed deliveries`
     (`main.go:358`), and the fake recorded `GET /app/hook/deliveries`. Then, as the user,
     `bus.OpenKeyValue(js, bus.WebhookRedeliveryBucket)`, a `Get` of the sweep's `cursor` key
     (`redeliver/sweep.go:67`, written by the first sweep `[INFERENCE: the sweep's write order is
     not re-read; if the cursor is written only after a non-empty listing, the test writes and
     reads a key of its own instead]`) and a `Watch` that delivers it: the bucket's calls under the
     grant as the server makes them.
  8. `conn.Publish("outside.example", nil)` is refused, and every violation the server logged names
     `outside.example` and nothing else (`permissions_live_test.go:83-91`'s reading): the server
     refused nothing Dispatch did.

  The listener is not run here: `DispatchPermissions` contains `HostPermissions`, and slice 4's
  test proves the listener under that half.

  Negative control, in the same test, with the same App and fake: the grant without
  `$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.delivery-events.>`; Dispatch still answers `/healthz`
  200 (the intake's bind is not health); `ConsumerInfo` for `delivery-events` answers "consumer not
  found" for 20 s while `delivery-jobs` is `PushBound`; the server logs a Publish Violation whose
  subject is `$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.delivery-events.notifications.github.>`
  (the form R19 names) and no other violation; Dispatch's output holds
  `dispatch delivery: create NATS consumer` (`intake.go:306`).
- [ ] **Step 2: implement `testpg`**, run `go test -tags natslive ./core/` from `packages/envoy/infra`
  with Docker on this machine; pass on both images.
- [ ] **Step 3: the workflow's pull step** names the pgvector image; `go vet -tags pulumilive,natslive ./...`
  passes.
- [ ] **Step 4: commit** `test(envoy-infra): the real envoy-dispatch under core.DispatchPermissions`.

### Task 6.4: `gcp:NatsHub` renders a Dispatch user

**Files:** modify `packages/envoy/infra/gcp/natshub.go` (`NATSUserArgs`, `authorization`),
`natshub_test.go`, `provider/provider_test.go` (`TestSchemaServesTheNatsHubComponentWithASecretAuthKey`
lists the `gcp:NatsUser` type's properties), `packages/envoy/infra/README.md` ("The hub's users",
"Permission sets", the hub example), `docs/site/src/content/docs/dispatch/envoy.md:142-146` (one
clause: the hub renders Dispatch's user too).

- [ ] **Step 1: failing tests.** `NATSUserArgs` (`natshub.go:75-80`) gains
  `DispatchMachineID *string \`pulumi:"dispatchMachineId,optional"\`` with the comment
  `// permissions = core.DispatchPermissions(dispatchMachineId): Dispatch's user, named after the listener beside it`.
  `authorization` (`:185-216`) accepts exactly one of `machineId`, `dispatchMachineId` and
  `permissions`; two or more set is refused naming the user and the fields set; none is refused with
  `name machineId, for its host's permission set, dispatchMachineId, for Dispatch's, or permissions`.
  Tests: `exampleArgs` gains a third user `{name: example-dispatch, publicKey: <U key>, dispatchMachineId: example-dispatch}`;
  the rendered configuration's third user block equals `core.DispatchPermissions("example-dispatch")`
  as `core.Authorization.Render` writes it (compare with a render the test builds through `core`
  directly, as the existing machine-id case is checked); every pair of the three fields is refused
  before any registration; an empty `dispatchMachineId` is refused naming it.
- [ ] **Step 2: implement, pass** `go test ./gcp/ ./provider/`.
- [ ] **Step 3: README.** "The hub's users" lists the third form; "Permission sets" gains
  `DispatchPermissions(machineID)`'s paragraph (what it adds to the host's set, in the words of
  Task 6.2's comment) and says a hub user with `dispatchMachineId` holds it, naming the natslive
  test as its proof; the hub example gains the user. Commit
  `feat(envoy-infra): gcp:NatsHub renders Dispatch's user from dispatchMachineId`.
- [ ] **Step 4: the slice's PR**, stacked on slice 5's. Its body carries the set's seventeen added
  publish entries and the live test's outcome on both images.

---

## Slice 7: `envoy-infra:gcp:Dispatch`

Branch `envoy-infra-gcp-dispatch` in its own clone, stacked on `envoy-infra-dispatch-permissions`.
Task 7.1 first; 7.2 needs 7.1; 7.3 and 7.4 need 7.2 and run in parallel; 7.5 needs 7.3 and 7.4;
7.6 needs 7.5; 7.7 is the consumer's.

### Task 7.1: the Kubernetes and random SDKs

**Files:** modify `packages/envoy/infra/go.mod`, `go.sum`; no list of release paths needs an edit,
since `release-envoy-infra.yaml`'s version step reads what the plugin compiles (`go list -deps`);
`scripts/go-third-party-notices.sh` picks the new modules up at release.

- [ ] `cd packages/envoy/infra && go get github.com/pulumi/pulumi-kubernetes/sdk/v4@v4.34.2 github.com/pulumi/pulumi-random/sdk/v4@v4.21.2 && go mod tidy`.
  Both require `github.com/pulumi/pulumi/sdk/v3 v3.259.0` (their `go.mod` files, read from the
  module proxy 2026-10-14), so `go.mod` keeps `v3.265.0`. `go build ./...` passes. Commit
  `build(envoy-infra): pulumi-kubernetes and pulumi-random SDKs`.

### Task 7.2: the component's inputs, settings and refusals

**Files:** create `packages/envoy/infra/gcp/dispatch.go`, `dispatch_test.go`; modify
`packages/envoy/infra/core/listener_env.go`, `listener_env_test.go` (R29).

**Interfaces:**

```go
const dispatchType = "envoy-infra:gcp:Dispatch"

// DispatchArgs are the inputs of an envoy-infra:gcp:Dispatch. Every field of a plain Go type is a
// value the consumer's program knows when it runs; the pulumi.*Input fields may come from another
// resource or stack.
type DispatchArgs struct {
	Region         string             `pulumi:"region"`         // e.g. us-west1: the cluster's, the database's and the NAT's
	Hostname       string             `pulumi:"hostname"`       // ^[a-z][a-z0-9-]{0,24}[a-z0-9]$: the tailnet name, ENVOY_MACHINE_ID, every child's name
	TailnetDnsName string             `pulumi:"tailnetDnsName"` // ^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.ts\.net$
	Tags           []string           `pulumi:"tags"`           // "tag:<name>", at least one
	TailnetAuthKey pulumi.StringInput `pulumi:"tailnetAuthKey" provider:"secret"`

	EnvoyImage         string `pulumi:"envoyImage"`         // Legion's envoy image, pinned: <name>@sha256:<digest>
	TailscaleImage     string `pulumi:"tailscaleImage"`     // pinned the same way
	CloudSqlProxyImage string `pulumi:"cloudSqlProxyImage"` // pinned the same way

	NatsUrl        pulumi.StringInput `pulumi:"natsUrl"`        // nats://<hub>.<tailnet>.ts.net:4222, from the hub's stack
	NatsSeedSecret pulumi.StringInput `pulumi:"natsSeedSecret"` // projects/<p>/secrets/<s>, written by envoy-infra seed generate

	SignInClientId     string             `pulumi:"signInClientId"`
	SignInClientSecret pulumi.StringInput `pulumi:"signInClientSecret" provider:"secret"`
	SignInDomain       string             `pulumi:"signInDomain"` // DISPATCH_SIGNIN_DOMAIN: a lowercase DNS name

	DatabaseVersion    *string `pulumi:"databaseVersion,optional"`    // default POSTGRES_16
	DatabaseTier       *string `pulumi:"databaseTier,optional"`       // default db-custom-1-3840
	DeletionProtection *bool   `pulumi:"deletionProtection,optional"` // default true (R27)
	Namespace          *string `pulumi:"namespace,optional"`          // default "dispatch"
}

// Dispatch is Dispatch on an Autopilot cluster of its own, with Cloud SQL, reached over a tailnet.
type Dispatch struct {
	pulumi.ResourceState
	Url              pulumi.StringOutput `pulumi:"url"`              // https://<hostname>.<tailnetDnsName>
	ClusterName      pulumi.StringOutput `pulumi:"clusterName"`      // the Autopilot cluster's name
	DatabaseInstance pulumi.StringOutput `pulumi:"databaseInstance"` // the instance's connection name
	Kubeconfig       pulumi.StringOutput `pulumi:"kubeconfig"`       // the exec-plugin kubeconfig the component uses (R20); no secret in it
	Principal        pulumi.StringOutput `pulumi:"principal"`        // the pod's Workload Identity principal, which the consumer grants roles/cloudsql.client (R30)
	AgentToken       pulumi.StringOutput `pulumi:"agentToken" provider:"secret"` // DISPATCH_AGENT_TOKEN, generated
}

// dispatchSettings are the plain inputs, validated, with the defaults applied, and the texts built
// from them alone, so they are known at preview: the Serve configuration and the listener's
// environment.
type dispatchSettings struct {
	hostname, tailnetDNSName, region, namespace string
	tags                                       []string
	images                                     struct{ envoy, tailscale, proxy string }
	signIn                                     struct{ clientID, domain string }
	databaseVersion, databaseTier              string
	deletionProtection                         bool
	serveConfig                                string // serve.json, R28
}

func newDispatchSettings(args *DispatchArgs) (dispatchSettings, error)
```

`core.ListenerConfig` gains `SOCKS5Proxy string // "" leaves ENVOY_NATS_SOCKS5_PROXY unset`;
`CheckPlain` refuses one `bus.CheckSOCKS5Proxy` refuses (Task 5.1's exported check, so the rule is
spelled once, in `bus`); `ListenerEnv` appends `ENVOY_NATS_SOCKS5_PROXY=<value>` after
`ENVOY_ALLOW_REMOTE_NATS` when set, and refuses a `SOCKS5Proxy` with every URL loopback (a set
proxy is remote reach and the bus refuses it without `ENVOY_ALLOW_REMOTE_NATS=1`, which
`ListenerEnv` sets only for a non-loopback URL): the refusal names both.

Refusals, each naming the input, before any registration: `hostname` outside `hostnameRule`
(`natshub.go:45`, which also keeps the hostname a name the cluster and every Kubernetes object can
carry: a DNS label of at most 26 characters); `tailnetDnsName` outside its rule; no `tags`, or one
outside `tagRule` (`:47`);
an image outside `imageRule` (`:50`), by field; an empty `region`; an empty `signInClientId`; a
`signInDomain` that is not a lowercase DNS name (`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`);
an empty `databaseVersion` or `databaseTier`; a `namespace` that is not a DNS label; `tailnetAuthKey`,
`natsUrl`, `natsSeedSecret` or `signInClientSecret` nil; then `providers.Require` for `gcp` and for
`random`, in that order.

- [ ] **Step 1: failing tests** (`dispatch_test.go`, the `mocks` of `natshub_test.go` extended
  with the outputs this component reads: a cluster's `endpoint`, `masterAuth.clusterCaCertificate`
  and `name`; a project lookup's `number` and `projectId` (the `Call` mock answers
  `gcp:organizations/getProject:getProject` with `{"projectId":"example-project","number":"123456789012"}`);
  an address's `name`; an instance's `connectionName`; a random password's `result`): every
  refusal above, each registering nothing; `newDispatchSettings` on valid inputs applies the
  defaults; `core.ListenerEnv` with `SOCKS5Proxy: "127.0.0.1:1055"` renders the line, and `CheckPlain`
  refuses `socks5://127.0.0.1:1055`.
- [ ] **Step 2: implement** `dispatch.go` through `newDispatchSettings` and the refusals, and the
  `core` change. Pass `go test ./core/ ./gcp/ -run 'Dispatch|ListenerEnv'`.
- [ ] **Step 3: commit** `feat(envoy-infra): gcp:Dispatch inputs and refusals`.

### Task 7.3: the cloud resources

**Files:** create `packages/envoy/infra/gcp/dispatch_cloud.go`; extend `dispatch_test.go`.

Resources (pulumi-gcp v10.1.0; every field below was read in
`pulumi-gcp/sdk/v10@v10.1.0/go/gcp/...` on 2026-10-14 unless marked), all children of the component
on the consumer's `gcp` provider, named after `<h>`, the hostname, as the hub names its children:

| Logical name | Type | Inputs |
| --- | --- | --- |
| (invoke) | `organizations.LookupProjectOutput` (`organizations/getProject.go:77`) | no `projectId`: the provider's project; read for `Number` and `ProjectId`, with `pulumi.Provider(<the gcp provider from opts>)` |
| `<h>-network` | `compute.Network` | `AutoCreateSubnetworks: false` |
| `<h>` | `compute.Subnetwork` | `Network`, `Region`, `IpCidrRange: "10.64.0.0/20"`, `PrivateIpGoogleAccess: true` |
| `<h>` | `compute.Router` | `Network`, `Region` |
| `<h>` | `compute.RouterNat` | `Router`, `Region`, `NatIpAllocateOption: "AUTO_ONLY"`, `SourceSubnetworkIpRangesToNat: "ALL_SUBNETWORKS_ALL_IP_RANGES"` — the private nodes' way to ghcr.io, `mirror.gcr.io`, Tailscale's control plane and DERP |
| `<h>-services` | `compute.GlobalAddress` | `Purpose: "VPC_PEERING"`, `AddressType: "INTERNAL"`, `PrefixLength: 16`, `Network` |
| `<h>-services` | `servicenetworking.Connection` (`servicenetworking/connection.go:295-321`) | `Network`, `Service: "servicenetworking.googleapis.com"`, `ReservedPeeringRanges: [address.Name]` (R21) |
| `<h>` | `container.Cluster` (`container/cluster.go:1850-2118`) | `Name: <h>` (explicit; Task 7.4 says why), `Location: region`, `EnableAutopilot: true`, `Network`, `Subnetwork`, `IpAllocationPolicy: &container.ClusterIpAllocationPolicyArgs{}` (GKE picks the secondary ranges `[INFERENCE]`), `PrivateClusterConfig{EnablePrivateNodes: true, EnablePrivateEndpoint: false}` (`pulumiTypes.go:51563-51585`; `MasterIpv4CidrBlock` unset: a new Autopilot cluster's control plane is reached over Private Service Connect, which needs none `[INFERENCE]`), `ReleaseChannel{Channel: "REGULAR"}`, `SecretManagerConfig{Enabled: true}` (`pulumiTypes.go:53137-53142`), `WorkloadIdentityConfig{WorkloadPool: <projectId>.svc.id.goog}`, `DeletionProtection` (R27) |
| `<h>` | `sql.DatabaseInstance` (`sql/databaseInstance.go:870-973`) | no `Name` (generated; Task 7.4 says why), `DatabaseVersion`, `Region`, `DeletionProtection`, `Settings{Tier, Edition: "ENTERPRISE" (R25), AvailabilityType: "ZONAL", DiskType: "PD_SSD", DiskSize: 10, DiskAutoresize: true, IpConfiguration{Ipv4Enabled: false, PrivateNetwork: network.Id, SslMode: "ENCRYPTED_ONLY"} (pulumiTypes.go:4617-4644), BackupConfiguration{Enabled: true, PointInTimeRecoveryEnabled: true, StartTime: "08:00", TransactionLogRetentionDays: 7, BackupRetentionSettings{RetainedBackups: 14}} (pulumiTypes.go:2997-3016)}`, `pulumi.DependsOn([connection])` |
| `<h>-password` | `random.RandomPassword` | `Length: 32`, `Special: false` (R22) |
| `<h>-signing-key` | `random.RandomPassword` | `Length: 64`, `Special: false` |
| `<h>-agent-token` | `random.RandomPassword` | `Length: 48`, `Special: false` |
| `<h>` | `sql.Database` | `Instance`, `Name: "dispatch"` |
| `<h>` | `sql.User` | `Instance`, `Name: "dispatch"`, `Password: password.Result` (secret). A built-in user is a member of `cloudsqlsuperuser`, so migration 0072's `create extension if not exists vector` runs at boot with no bootstrap (the research memo, `sql-users`, `sql-ext`) |
| `<h>-nats-seed-accessor` | `secretmanager.SecretIamMember` | `SecretId: natsSeedSecret`, `Role: "roles/secretmanager.secretAccessor"`, `Member: <principal>` — the component's one GCP IAM write (R30) |

`<principal>` is
`principal://iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/<projectId>.svc.id.goog/subject/ns/<namespace>/sa/<h>`,
the Kubernetes service account's own identity under Workload Identity Federation, granted directly
with no Google service account (the research memo, `gke-wi:97,314,509-521`). It is the `principal`
output. `roles/cloudsql.client` on it is not declared here: it is a project-level grant the
consumer's bootstrap makes, from the same four values (R30).

- [ ] **Step 1: failing tests**: every child above registered with exactly these inputs, as
  `TestNatsHubRegistersEveryChildWithTheListedInputs` checks the hub's (`natshub_test.go`,
  `wantChildren`/`requireChildren`); the database password reaches the `sql.User` and nothing else
  as a secret; the instance depends on the connection; `deletionProtection: false` reaches the
  cluster and the instance; `databaseVersion`/`databaseTier` reach the instance; the principal's
  text for `example-project`, `123456789012`, namespace `dispatch` and hostname `example-dispatch`
  reaches the `SecretIamMember` and the `principal` output; no `projects.IAMMember`, no
  `serviceaccount.*` and no `sql.User` beyond the built-in one is registered (R30).
- [ ] **Step 2: implement, pass, commit** `feat(envoy-infra): gcp:Dispatch's cluster, database and grants`.

### Task 7.4: the Kubernetes resources

**Files:** create `packages/envoy/infra/gcp/dispatch_kube.go`, `testdata/dispatch-kubeconfig.golden`,
`testdata/dispatch-serve.golden`, `testdata/dispatch-pod.golden`; extend `dispatch_test.go`.

The provider: `kubernetes.NewProvider(ctx, <h>+"-kubernetes", &kubernetes.ProviderArgs{Kubeconfig: kubeconfig, EnableServerSideApply: true}, pulumi.Parent(comp))`,
where `kubeconfig` is `pulumi.All(cluster.Endpoint, cluster.MasterAuth.ClusterCaCertificate(), cluster.Name).ApplyT(renderKubeconfig)`
(R20), the golden's text with `<endpoint>`, `<ca>` and `<name>` filled:

```yaml
apiVersion: v1
kind: Config
clusters:
- name: <name>
  cluster:
    server: https://<endpoint>
    certificate-authority-data: <ca>
contexts:
- name: <name>
  context:
    cluster: <name>
    user: <name>
current-context: <name>
users:
- name: <name>
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: gke-gcloud-auth-plugin
      installHint: Install gke-gcloud-auth-plugin (gcloud components install gke-gcloud-auth-plugin)
      provideClusterInfo: true
      interactiveMode: Never
```

Every Kubernetes child takes `pulumi.Parent(comp)`, `pulumi.Provider(k8s)` and
`pulumi.DependsOn([cluster])`, and sets `metadata.name` to the exact name below and
`metadata.namespace` to `<ns>` (default `dispatch`; the Namespace itself names only itself):
pulumi-kubernetes suffixes an object whose `metadata.name` is unset, `pulumi.WithMocks` applies no
such naming, and the principal (`…/subject/ns/<ns>/sa/<h>`), `serviceAccountName`,
`secretProviderClass`, the ConfigMap volume, `TS_KUBE_SECRET` and the Role's `resourceNames` all
name these objects exactly, so a name left to the provider would pass every mock test and grant
the seed to a principal that matches no service account. The logical name equals the
`metadata.name` in every row:

| `metadata.name` (and logical name) | Type (pulumi-kubernetes v4.34.2) | What it holds |
| --- | --- | --- |
| `<ns>` | `core/v1.Namespace` | |
| `<h>` | `core/v1.ServiceAccount` | the pod's identity; no annotation (the principal above is its own) |
| `<h>-tailscale-state` | `rbac/v1.Role` + `rbac/v1.RoleBinding` | `secrets`: `create`; `secrets` with `resourceNames: [<h>-tailscale-state]`: `get`, `update`, `patch` (what containerboot's `TS_KUBE_SECRET` store needs); the binding's subject is the ServiceAccount `<h>` in `<ns>` |
| `<h>-tailnet-auth-key` | `core/v1.Secret` | `stringData: {TS_AUTHKEY: tailnetAuthKey}` (secret) |
| `<h>-dispatch` | `core/v1.Secret` | `stringData`: `DATABASE_URL=postgres://dispatch:<password>@127.0.0.1:5432/dispatch?sslmode=disable` (the proxy encrypts; the hop is loopback), `DISPATCH_AGENT_TOKEN`, `DISPATCH_SIGNING_KEY`, `DISPATCH_SIGNIN_CLIENT_SECRET` (all secret) |
| `<h>-serve` | `core/v1.ConfigMap` | `serve.json`: `{"TCP":{"443":{"HTTPS":true}},"Web":{"<h>.<tailnet>:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8766"}}}}}` (R28; the golden) |
| `<h>-nats-seed` | `apiextensions.CustomResource` `secrets-store.csi.x-k8s.io/v1` `SecretProviderClass` | `spec.provider: gke`; `spec.parameters.secrets`: a YAML list with one entry, `resourceName: <natsSeedSecret>/versions/latest`, `path: nats-seed` |
| `<h>` | `apps/v1.Deployment` | below |

The two Google Cloud resources an operator names by hand are named the same way: the cluster
sets `Name: <h>` (explicit, so `gcloud container clusters describe <h>` and the kubeconfig's
context name are the hostname), and the Cloud SQL instance leaves `Name` unset and takes the
provider's generated name, because Cloud SQL keeps a deleted instance's name unavailable for a
while `[INFERENCE: Cloud SQL documents a reuse delay of up to a week]` and R27's
tear-down-and-rebuild of a first deployment would otherwise wait on it; the instance's connection
name is the `databaseInstance` output, which the consumer's acceptance reads with
`pulumi stack output databaseInstance` rather than by a fixed name. The network, subnetwork,
router, NAT, address and database keep logical names only, as the hub's children do.

The Deployment: `replicas: 1`, `strategy: {type: Recreate}`, selector and template label
`app: <h>`; pod `serviceAccountName: <h>`, `terminationGracePeriodSeconds: 30`
(`cmd/dispatch/shutdown.go:18`, the memo's citation; `deploy/compose/dispatch.compose.yml:18`'s
`stop_grace_period`), volumes `nats-seed` (`csi: {driver: secrets-store-gke.csi.k8s.io, readOnly: true, volumeAttributes: {secretProviderClass: <h>-nats-seed}}`)
and `serve` (`configMap: <h>-serve`). Every container's requests equal its limits (Guaranteed); the
pod's CPU totals 1.5, a multiple of Autopilot's 0.25 vCPU increment, and its memory ratio stays
inside 1:1 to 1:6.5 (the research memo, `gke-res`):

| Container | Image | Command and arguments | Environment | Mounts | Resources | Probe |
| --- | --- | --- | --- | --- | --- | --- |
| `tailscale` (an `initContainers` entry with `restartPolicy: Always`: a native sidecar, started before the containers and kept running, so the SOCKS5 proxy and the node exist before any NATS client dials; Dispatch and the listener each exit after ten failed dial attempts, `cmd/dispatch/main.go:164-168`, `cmd/listener/main.go:526-528`, `internal/bus/nats.go:219-274`, and a first join slower than that would otherwise restart them) | `tailscaleImage` | the image's entrypoint (containerboot) | `TS_AUTHKEY` from the auth-key Secret, `TS_AUTH_ONCE=true`, `TS_KUBE_SECRET=<h>-tailscale-state`, `TS_USERSPACE=true`, `TS_SOCKS5_SERVER=127.0.0.1:1055`, `TS_SERVE_CONFIG=/etc/tailscale/serve.json`, `TS_HOSTNAME=<h>`, `TS_EXTRA_ARGS=--advertise-tags=<tags, comma-joined>`, `TS_ENABLE_HEALTH_CHECK=true`, `TS_LOCAL_ADDR_PORT=127.0.0.1:9002` (each on Tailscale's Docker parameters page, read 2026-10-14) | `serve` at `/etc/tailscale` | 100m / 128Mi | `startupProbe` `exec: wget -qO- http://127.0.0.1:9002/healthz` (busybox `wget` in the Alpine image `[INFERENCE]`), period 5 s, failure threshold 36 (three minutes for the first join, within which the kubelet starts the next container only once it passes); readiness the same probe at period 10 s |
| `cloud-sql-proxy` (the second `initContainers` entry with `restartPolicy: Always`, started after the Tailscale container's startup probe passes) | `cloudSqlProxyImage` | `--private-ip --structured-logs --address=127.0.0.1 --port=5432 <instance connectionName>` | | | 200m / 256Mi | none; `securityContext: {runAsNonRoot: true}` |
| `envoy-dispatch` | `envoyImage` | `command: ["/usr/local/bin/envoy-dispatch"]` (the image's entrypoint is the listener, `Dockerfile:128`) | `HOME=/home/envoy` (as the compose file sets it); from the Secret: `DATABASE_URL`, `DISPATCH_AGENT_TOKEN`, `DISPATCH_SIGNING_KEY`, `DISPATCH_SIGNIN_CLIENT_SECRET`; literal: `DISPATCH_SIGNIN_ISSUER=https://accounts.google.com`, `DISPATCH_SIGNIN_CLIENT_ID`, `DISPATCH_SIGNIN_DOMAIN`, `DISPATCH_SERVER_URL=https://<h>.<tailnet>`, `DISPATCH_LISTEN_HOST=127.0.0.1`, `NATS_URLS=<natsUrl>`, `ENVOY_ALLOW_REMOTE_NATS=1`, `NATS_NKEY_SEED_FILE=/var/run/secrets/envoy/nats-seed`, `ENVOY_NATS_SOCKS5_PROXY=127.0.0.1:1055`, `ENVOY_URL=http://127.0.0.1:9020` (every name a row of `settings.go:40-117`; `DISPATCH_WEB_DIST` is the image's, `Dockerfile:119`) | `nats-seed` at `/var/run/secrets/envoy`, read-only | 1000m / 4Gi | readiness `exec: curl -fsS http://127.0.0.1:8766/healthz`, period 10 s, timeout 3 s, failure threshold 3, initial delay 5 s (the compose check's numbers, `dispatch.compose.yml:18-24`); no liveness probe (`/healthz` reads the database) |
| `envoy-listener` | `envoyImage` | the image's entrypoint | `core.ListenerEnv(ListenerConfig{MachineID: <h>, NATSURLs: [natsUrl], Port: 9020, SOCKS5Proxy: "127.0.0.1:1055"})` inside the `ApplyT` over `natsUrl` (as `host:Listener` does, `host/listener.go:147-151`; `ENVOY_ALLOW_REMOTE_NATS=1` comes out of it for the hub's URL, and the set proxy requires it, Task 5.1), plus `NATS_NKEY_SEED_FILE=/var/run/secrets/envoy/nats-seed`; `ENVOY_LISTEN_HOST` unset (loopback, R23) | `nats-seed`, read-only | 200m / 512Mi | readiness `exec: curl -fsS http://127.0.0.1:9020/healthz` (200 `starting` counts as ready for the probe; "healthy" is what the proof reads) |

The two native sidecars are listed in start order; the pod's two `containers` start once both
sidecars' startup probes pass (the Auth Proxy has none, so it counts as started at once). No
`securityContext` on the three Envoy and Tailscale containers (R24); no Service (R23); no PodDisruptionBudget (one
replica; Autopilot respects one for an hour and evicts anyway, the research memo, `gke-upg`).

- [ ] **Step 1: failing tests**: the kubeconfig golden from mock outputs `example-endpoint`,
  `ZXhhbXBsZS1jYQ==` and `example-dispatch`; the serve golden; the pod golden (the Deployment's
  `spec.template` as the mock recorded it, JSON, every secret value replaced by `<secret>` by
  `plain`, `natshub_test.go`'s helper); the Kubernetes provider registered once, as a child, with the
  kubeconfig; every Kubernetes child on that provider and after the cluster, with `metadata.name`
  equal to the table's name and `metadata.namespace` equal to `<ns>` (the Namespace excepted),
  asserted per registration; the cluster's `Name` is `example-dispatch` and the instance registers
  no `Name`; the Deployment's `initContainers` are `tailscale` then `cloud-sql-proxy`, each with
  `restartPolicy: Always`, and `tailscale` carries the startup probe; the four secret values
  (`tailnetAuthKey`, the password inside `DATABASE_URL`, the signing key, the agent token, the
  client secret) reach the two Secrets' `stringData` as secrets and no plain value of any
  registration holds them (`requireAuthKeyOnlyInTheSecretVersion`'s shape); `namespace: ops`
  renames the Namespace and the principal; `tags` reach `TS_EXTRA_ARGS`; the outputs `url`,
  `clusterName`, `databaseInstance`, `kubeconfig`, `principal` and `agentToken` (secret) resolve.
- [ ] **Step 2: implement, pass, commit** `feat(envoy-infra): gcp:Dispatch's pod on its own cluster`.

### Task 7.5: registration, schema, Construct

**Files:** modify `packages/envoy/infra/provider/provider.go:27` (`infer.ComponentF(gcp.NewDispatch)`),
`provider/provider_test.go` (a `TestSchemaServesTheDispatchComponentWithItsSecrets`: the
component is listed, `tailnetAuthKey`, `signInClientSecret` secret inputs, `agentToken` a secret
output, no object type added); create `gcp/dispatch_construct_test.go`.

- [ ] **Step 1: failing tests.** Construct, through `integration.NewServer` with the mocks behind it
  (`gcp/construct_test.go:29-46`): `tailnetAuthKey`, `natsUrl`, `natsSeedSecret` and
  `signInClientSecret` each sent as another resource's output, then unknown under preview, and the
  component registers with the texts it can build from plain inputs known (the serve ConfigMap, the
  kubeconfig unknown until the cluster answers); a plain field (`hostname`) sent as an output is
  refused naming it (`construct_test.go:80-109`'s case). The schema test.
- [ ] **Step 2: pass** `go test ./provider/ ./gcp/`, then the whole module's CI commands
  (`AGENTS.md`, "Tests"). Commit `feat(envoy-infra): serve gcp:Dispatch`.

### Task 7.6: documentation

**Files:** modify `packages/envoy/infra/README.md` (new sections after "Updating a hub": "What
`gcp:Dispatch` deploys" (the two tables above, in the README's shape), "Dispatch's inputs" (a table
with each refusal), "The pod" (R23, R24, R26 with the census recipe:
`kubectl -n dispatch run census --rm -it --restart=Never --image=<envoyImage> --overrides='{"spec":{"serviceAccountName":"<h>"}}' --command -- /usr/local/bin/envoy-dispatch census`
with `DATABASE_URL` from the Secret), "Signing in" (the Google client: a web client whose redirect
URI is `https://<h>.<tailnet>/auth/callback`, the `ts.net` host accepted since `ts.net` is on the
public suffix list; the tailnet's HTTPS certificates must be enabled), "Updating Dispatch" (a digest
or environment change recreates the one pod, about a minute of downtime; a `region` change replaces
everything; `deletionProtection`), "Backups" (Cloud SQL's automated backups, 14 kept, point-in-time
recovery over 7 days; a deleted instance takes its backups with it), "What Dispatch's pod may
do" (R30: the one resource-level grant the component makes and the role that covers making it;
the `roles/cloudsql.client` grant the consumer makes to `principal`, whose form is spelled out,
before the first apply; the Kubernetes RBAC on the state Secret; what the Auth Proxy logs
without the grant), "Providers" gains the `random` provider and the component-built Kubernetes
one (R20: the kubeconfig's exec plugin, how each machine installs it — the SDK component, the
distribution package, `setup-gcloud` with `install_components` in CI — and the token-form
kubeconfig as the documented alternative, with its two costs: a provider that updates on every
run and a token in state);
"What a first Google Cloud deployment proves" gains the items listed below; "Examples" gains "A
Dispatch, in YAML" with placeholders); `packages/envoy/infra/AGENTS.md` (Layout: `gcp/dispatch*.go`
and the three goldens; Rules: the Kubernetes provider a component builds is explicit and the
component still calls `providers.Require` for `gcp` and `random`; Tests: the new goldens and
Construct tests); `docs/site/src/content/docs/dispatch/envoy.md:142-146` (one clause naming
`gcp:Dispatch`); `packages/envoy/cmd/dispatch/README.md` (a sentence pointing a Google Cloud
deployment at the component).

"What a first Google Cloud deployment proves" gains, in the README's list form:

- Autopilot admits the pod as written: no privileged container, the Tailscale container in
  userspace as a native sidecar with a startup probe, the Auth Proxy as a native sidecar
  (`restartPolicy: Always` on both), Guaranteed resources at the pod's totals, and no mutation of
  requests by Autopilot's admission (`kubectl get pod -o yaml`); the pod's containers show zero
  restarts after the first join, since nothing dials NATS before the node is up.
- Tailscale Serve carries Dispatch's SSE streams (`/api/v1/events`, `/api/v1/agents/<id>/stream`)
  and document websockets (`/ws/doc/<room>`, `/ws/delivery`) at `https://<h>.<tailnet>`, and
  obtains the node's certificate.
- The Secret Manager add-on mounts the seed at `/var/run/secrets/envoy/nats-seed` under the
  service account's direct principal grant, with no Google service account.
- The Auth Proxy reaches the instance over its private IP through the peering, with
  `ENCRYPTED_ONLY` on the instance, under the `roles/cloudsql.client` grant the consumer's
  bootstrap made to `principal` (R30).
- The built-in user creates `vector` at Dispatch's first boot (migration 0072), and `/healthz`
  reports the schema version.
- tailscaled's SOCKS5 server resolves the hub's MagicDNS name for a `CONNECT` that carries it
  unresolved: the listener answers `"status":"healthy"` and Dispatch's outbox publishes.
- `gke-gcloud-auth-plugin` authenticates the Kubernetes provider under the credentials each machine
  runs `pulumi` with (R20).
- Google Cloud accepts the cluster with `ipAllocationPolicy: {}` and no `masterIpv4CidrBlock`, and
  the Cloud NAT lets the private nodes pull from ghcr.io and `mirror.gcr.io` and let tailscaled
  reach Tailscale's control plane and DERP relays.
- Dispatch's containers run as a non-zero uid (`kubectl exec … -- id -u`), the image's `envoy`.
- Google accepts the `ts.net` redirect URI for the sign-in client.

- [ ] Write them; `bun run build` in `docs/site` passes and `check-private-names.sh` passes on the
  built site. Commit `docs(envoy-infra): gcp:Dispatch`.

### Task 7.7: pre-merge proof, in the first deployment

- [ ] The slice's acceptance (E17, E18 below) runs in the first Google Cloud deployment's repository,
  per its own plan, against this branch's built plugin and CLI and the integration-branch image
  ("Which image runs where" under Slices: `envoy-dispatch-integration`, slice 6's head merging
  `dispatch-google-signin`, built by `release-envoy-listener.yaml`'s `workflow_dispatch` and pinned
  by digest), after that deployment's hub holds Dispatch's user (Task 6.4) and its tailnet policy
  carries `tag:example-dispatch`. That plan's tester
  records the result there; this PR gets a summary of outcomes and durations with placeholders only:
  no project, tailnet, host name, address or account.
- [ ] The slice's PR, stacked on slice 6's; after merge, `release-envoy-infra.yaml`'s `envoy-infra-v*` tag goes
  on the PR, and the deployment's plan pins it.

---

## Hardening ledger

Items to verify, each with the row that settles it and where its result goes:

- nats-server's permission matching of the filtered consumer create: the published subject's last
  token is a literal `>` (`$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.delivery-events.notifications.github.>`),
  and the grant's `…delivery-events.>` must match it on 2.10 and on the hub image (E14; the natslive
  test on both images, Task 6.3).
- tailscaled's userspace SOCKS5 server resolving a MagicDNS name sent as a domain name (E17; the
  listener healthy in the first deployment).
- `gke-gcloud-auth-plugin`'s credential source: on the laptop under `CLOUDSDK_ACTIVE_CONFIG_NAME`,
  in CI under the credential file `google-github-actions/auth` writes (E17 and the deployment's
  CI preview; R20).
- `roles/editor` covering the Kubernetes API writes (the Namespace, the RBAC objects, the
  Deployment) for the consumer's deploy identity, with no `roles/container.*` role added (E17's
  first CI apply; R30).
- `ipAllocationPolicy: {}` with no `masterIpv4CidrBlock` on an Autopilot cluster with private nodes
  (E17; the first `pulumi up`).
- The Secret Manager add-on's `SecretProviderClass` CRD present once the cluster is created, so the
  custom resource's create needs no second apply (E17).
- Autopilot's admission of a native sidecar init container, and of Guaranteed requests at the
  pod's totals without mutation (E17).
- Serve's handling of SSE and websockets (E17).
- The pod's uid is non-zero without a `securityContext` (E17; R24). If a future rule needs
  `runAsNonRoot`, the image pins its user's uid (`useradd -r -u <n>`, `Dockerfile:96`) first.
- `ENCRYPTED_ONLY` with the Auth Proxy's `--private-ip` connection (E17).
- The 10 s negative control in Task 5.1 (the bus's ten dial attempts) stays under the `envoy-go`
  job's timeout with the rest of `internal/bus` (E12; CI).
- Cloud SQL's reuse delay for a deleted instance's name, the reason the instance keeps a generated
  name (Task 7.4): `[INFERENCE]` until a first deployment is torn down and rebuilt (E17; R27).
- IAM accepting a binding to the `<project>.svc.id.goog` principal before the first Workload
  Identity cluster of the project exists (the consumer's bootstrap grants `roles/cloudsql.client`
  before `gcp:Dispatch` creates the cluster; E17's first apply, hardening ledger of the consumer's
  plan too).
- The redelivery sweep writing its `cursor` key on an empty first listing (Task 6.3 step 7; E14).

## End-to-end verification plan

The numbering continues `docs/plans/2026-10-14-envoy-infra-plan.md`'s table.

| Scenario | Slice | Class | Surface and transport | Identity and grant | Driver |
| --- | --- | --- | --- | --- | --- |
| E12. The bus reaches NATS through a SOCKS5 proxy, the host unresolved; unset, no proxy is dialed; a dead proxy fails naming it; a malformed value is refused before any dial; a reconnect goes through the proxy | 5 | infrastructure | in-process SOCKS5 server and `testnats` on the runner's Docker | CI runner | Task 5.1, `envoy-go` |
| E13. Dispatch reads `ENVOY_NATS_SOCKS5_PROXY` through its table | 5 | infrastructure | `go test ./cmd/dispatch/` | CI runner | Task 5.2 |
| E14. The real `envoy-dispatch` under `core.DispatchPermissions` refuses nothing on 2.10 and on the hub image; without the delivery durable's create it never binds the durable and the server names the subject | 6 | infrastructure | real nats-server and Postgres containers, this branch's `envoy-dispatch` | CI runner | Task 6.3, `envoy-infra-go` natslive |
| E15. `gcp:NatsHub` renders a Dispatch user from `dispatchMachineId`; two of the three forms are refused before registration | 6 | infrastructure | `go test ./gcp/` under mocks | CI runner | Task 6.4 |
| E16. `gcp:Dispatch` registers every child with the listed inputs, secrets only in the Secrets, the goldens, Construct's handling of outputs and unknowns, every refusal | 7 | infrastructure | `go test ./gcp/ ./provider/` | CI runner | Tasks 7.2-7.5 |
| E17. The first Google Cloud deployment: `pulumi up` creates the cluster, the database and the pod; the pod is `4/4` Ready; both `/healthz` answer from inside the pod; the Tailscale node joins as `tag:example-dispatch`; a member on the tailnet opens `https://<h>.<tailnet>` and signs in with a Workspace account; an account outside the domain is refused; a personal token minted in the UI drives the `dispatch` CLI; an issue's events reach a tailnet client subscribed to `notifications.dispatch.>` on the hub; the SPA's live updates (SSE) and a document's editor (websocket) work through Serve; `kubectl delete pod` recreates the one pod and the site is back inside two minutes; the instance has no public IP and backups and PITR on | 7 | infrastructure | the deployment's own stack, applied from the laptop with the branch's plugin, then from CI with the release | its operator; prerequisites: the hub applied with Dispatch's user, the policy with `tag:example-dispatch`, `roles/cloudsql.client` granted to the pod's principal by the consumer's bootstrap, the Google client, HTTPS enabled on the tailnet, `gke-gcloud-auth-plugin` installed | the deployment's plan (Task 7.7) |
| E18. Negative controls in the first deployment: a request to `https://<h>.<tailnet>` from a device off the tailnet is refused (no route); the pod's service account cannot read a secret it was not granted (`kubectl exec … -- curl` to Secret Manager for another secret answers 403); the hub refuses a client with no seed (slice 4's E9) | 7 | infrastructure | the deployment, from the laptop | its operator | the deployment's plan |

Legion's own end-to-end stages (`scripts/e2e/stage*.sh`) exercise none of the changed paths. The
Envoy changes (the bus dialer, Dispatch's setting, the exported names) are covered by `envoy-go`,
`envoy-smoke` and `envoy-infra-go`, which run on these PRs.

## Skill catalog

Skills available (listed from the directories): Legion `skills/` (`ce-simplify-code`,
`thermonuclear-code-quality`, `thermonuclear-deep-review`, `envoy`, `legion-*`, `dispatch*`); the
dotfiles skills (`opening-a-pr`, `landing-a-pr`, `using-docker`, `updating-docs`,
`review-judgment`, `analyze`, `gh-stack`); superpowers (`test-driven-development`,
`verification-before-completion`, `systematic-debugging`).

| Step | Worker | Required skills |
| --- | --- | --- |
| Tasks 5.1-5.2, 6.1-6.4, 7.1-7.5 | `deep` (implementer) | `test-driven-development`, `using-docker` (the natslive and `testpg` containers), `verification-before-completion`, `systematic-debugging` when a test fails unexpectedly |
| Task 7.6 | `deep` | `updating-docs` |
| Opening and landing each PR; Tasks 5.3, 6.4 step 4, 7.7 | `deep` (implementer) | `opening-a-pr`, `landing-a-pr`, `gh-stack` |
| Slice acceptance (E17, E18) | `deep` (tester, in the deployment's repository) | `verification-before-completion`, `envoy` |
| Task and final review | `reviewer` | `review-judgment` |
| Final pass per PR | coordinator | `ce-simplify-code` (once), then `thermonuclear-deep-review`, `thermonuclear-code-quality` |

## Contract change census

New contracts (`ENVOY_NATS_SOCKS5_PROXY`, `core.DispatchPermissions`, `gcp:NatsUser.dispatchMachineId`,
`gcp:Dispatch`) tighten nothing existing. Changes to existing ones:

- `bus.connectWithContext` gains a parameter: `git grep -n connectWithContext -- packages/envoy/internal/bus`
  → `nats.go:212,217,662`, `connect_deadline_test.go:22`, `stream_config_test.go:103,132`, all in
  the package; the three test calls gain a `nil` dialer. Unset, the connect dials as before (E12's
  second case). `refuseRemoteNATS` gains the proxy rule: a set `ENVOY_NATS_SOCKS5_PROXY` without
  `ENVOY_ALLOW_REMOTE_NATS=1` is refused; no deployment sets the proxy today, so nothing deployed
  changes behaviour.
- Dispatch's settings table gains a row; the all-or-none rules of other settings are untouched
  (`settings_test.go`'s existing cases pass verbatim). Rollout: immediate; an older image ignores the
  variable, so a deployment that sets it before pinning slice 5's image dials NATS directly and
  fails to reach a hub only the proxy reaches, which its listener's `/healthz` shows.
- `delivery-events`, `delivery-jobs` and `envoy_webhook_redelivery` are spelled in `bus` and read
  from there: `git grep -n -e '"delivery-events"' -e '"delivery-jobs"' -e '"envoy_webhook_redelivery"' -- packages/envoy`
  → `delivery/intake.go:227`, `delivery/jobs.go:57`, `redeliver/sweep.go:46` only (no test under
  `internal/dispatch` spells them, so Task 6.1's literal test in `bus` is the one independent
  check). The names are unchanged, so every deployed durable and bucket is still the one Dispatch
  binds.
- `core.ListenerConfig` gains a field with a zero-value default; `host:Listener` renders the same
  environment as before (`core/listener_env_test.go`'s existing cases pass verbatim).
- `gcp:NatsUser` gains an optional property; a user naming `machineId` alone renders as before
  (the hub's existing tests pass verbatim), so an applied hub previews no change.

## Permission source census

| Permission | Holder | Granted by | Enforced by | Test that fails without a holder |
| --- | --- | --- | --- | --- |
| NATS publish/subscribe for Dispatch and the listener beside it | Dispatch's nkey user | the consumer's `users` entry with `dispatchMachineId`, rendered by `core.DispatchPermissions` through `gcp:NatsHub` | nats-server `authorization` | E14's live test and its negative control; E17 |
| `roles/secretmanager.secretAccessor` on the NATS seed secret | the pod's Kubernetes service account, as its Workload Identity principal | `gcp:Dispatch`'s `SecretIamMember` | Secret Manager IAM | E17 (the seed mounts only if granted), E18 |
| `roles/cloudsql.client` | the same principal | the consumer's bootstrap program, applied by its project owner, from the principal's documented form (R30) | Cloud SQL Admin API (the Auth Proxy's connection) | E17 (`/healthz` reads the database through the proxy; without it the proxy logs `cloudsql.instances.connect`) |
| `secrets` create, and get/update/patch on the Tailscale state Secret | the same Kubernetes service account | `gcp:Dispatch`'s Role and RoleBinding | the Kubernetes API server | E17 (the node keeps its identity across `kubectl delete pod`) |
| `secretmanager.secrets.setIamPolicy` on the seed secret (to create the accessor) | the consumer's deploy identity | its bootstrap's `roles/secretmanager.admin` | GCP IAM | E17's first `pulumi up` from CI |
| Writing the Namespace, RBAC objects and Deployment through the cluster's API | the consumer's deploy identity | its bootstrap's `roles/editor` (`container.*`), as the cluster's creator `[INFERENCE]` | GKE IAM, the API server | E17's first `pulumi up` from CI |
| Joining the tailnet as `tag:example-dispatch` | the pod's Tailscale node, with the consumer's auth key | the consumer's tailnet policy (`tagOwners`) and its minted key | Tailscale | E17 (the node appears tagged) |
| Reaching `tag:example-dispatch:443`; Dispatch reaching `tag:example-hub:4222` | members and `tag:example-host` devices; the Dispatch node | the consumer's tailnet policy grants | Tailscale | E17, E18 |
| Signing in | a verified account of the configured Workspace domain | the Google client the consumer creates, `signInClientId`/`signInClientSecret`/`signInDomain` | Dispatch's domain rule (the sign-in plan's D5) | E17 (an outside account refused) |
| Reading the Kubernetes API as the operator | whoever runs `pulumi` or `kubectl` | `gke-gcloud-auth-plugin` under that machine's gcloud credentials | GKE IAM | E17 |

## Evidence

Read 2026-10-14 at the slice 4 head (`envoy-infra-gcp` at 5194ca07):
`packages/envoy/internal/bus/nats.go:22-64,102-110,149-208,211-294,319-340,359-443,662`,
`connect_deadline_test.go:22`, `stream_config_test.go:103,132`, `reach.go:25-63`, `nkey.go:14-67`, `kv.go:28-45`, `stream.go:13-40,65-80,113-119,234-264`;
`cmd/dispatch/settings.go:11-21,40-127,181-187`, `settings_test.go:134-147,427-450,711-727,1149-1153`;
`cmd/dispatch/main.go:139-203,299-383,408-448,624-676`; `packages/dispatch/e2e/run-server.sh:90-112`; `internal/dispatch/delivery/intake.go:223-246,274-281,298-355`,
`delivery/jobs.go:56-61,129-134`, `delivery/jobs_test.go:179-182`; `internal/dispatch/redeliver/sweep.go:43-82`;
`internal/dispatch/agentstream/agentstream.go:29-30,65,97`; `internal/dispatch/api/routes_table.go:55,82,109`,
`api/agent_stream.go:55-58,220-223`; `internal/dispatch/outbox/publisher.go:30,304,631-633,651-654`;
`internal/dispatch/store/storetest/storetest.go:33-55`; `cmd/listener/durable.go:43-48,122-137`,
`cmd/listener/apiauth.go:87-97`, `internal/config/config.go:14-63`; `docker/Dockerfile:90-128`;
`scripts/dev-postgres.sh:14-17`; `docker/Dockerfile:95-128`; `.github/workflows/envoy-and-contracts.yaml:632-645,766-783,877-939`;
`infra/AGENTS.md` (whole), `infra/README.md:261-320,471-504,940-977`, `infra/go.mod` (whole; x/net at `:191`), `packages/daemon/go.mod:117`,
`infra/core/permissions.go` (whole), `core/listener_env.go` (whole), `core/permissions_live_test.go:1-130`,
`infra/gcp/natshub.go` (whole), `gcp/natshub_test.go` and `gcp/construct_test.go` (declarations),
`gcp/export_test.go`, `infra/internal/providers/require.go`, `infra/provider/provider.go`,
`provider/provider_test.go:17-60`. Dependencies: nats.go v1.50.0 `nats.go:296-301,507-513,555-556,1434-1442,1521-1527,2232-2300,2699-2723`,
`jsm.go:488-535`, `js.go:154-171`; `golang.org/x/net@v0.59.0/internal/socks/client.go:96-103,130`,
`internal/socks/socks.go:69-71,151,168-171`, `proxy/socks5.go:17-20`, `proxy/dial.go:13-15`;
pulumi-gcp v10.1.0 `container/cluster.go:1850-2118`, `container/pulumiTypes.go:19623,51563-51585,52497-52505,53137-53142`,
`sql/databaseInstance.go:548,619,870-973`, `sql/pulumiTypes.go:1654-1747,2997-3016,4617-4644`,
`servicenetworking/connection.go:295-321`, `organizations/getProject.go:42-80`;
pulumi-kubernetes SDK v4.34.2 and pulumi-random SDK v4.21.2 `go.mod` (the module proxy, 2026-10-14:
each requires `pulumi/sdk/v3 v3.259.0`); Tailscale's Docker configuration parameters page
(`TS_AUTH_ONCE`, `TS_AUTHKEY`, `TS_ENABLE_HEALTH_CHECK`, `TS_HOSTNAME`, `TS_KUBE_SECRET`,
`TS_LOCAL_ADDR_PORT`, `TS_SERVE_CONFIG`, `TS_SOCKS5_SERVER`, `TS_USERSPACE`, `TS_EXTRA_ARGS`;
validated by Tailscale 2026-03-23, read 2026-10-14). The research memos of 2026-10-14 (kept with
the coordinator's plan, not in this repository) for the Google Cloud facts cited as "the research
memo".
