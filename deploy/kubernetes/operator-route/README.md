# The operator's model route

Legion carries no model, provider or route knowledge. Everything a Legion pod needs to reach a model
is the operator's, delivered through `runtime.kubernetes.pod` and `provider_keys`
(`docs/kubernetes.md`, "Operator configuration"). This directory is an example of it:
[`pod.yml`](pod.yml) is the pieces an operator supplies, in the shape the daemon loads, and
[`models.yml`](models.yml) and [`overlay.yml`](overlay.yml) are the two files its ConfigMap holds,
on the model names every other agent uses. The Go live harnesses run on all three.

## What an operator supplies

- **A ConfigMap** with two keys, named where `pod.yml` names it (`legion-operator-route`):
  `models.yml`, an Oh My Pi provider file, and `overlay.yml`, an Oh My Pi settings overlay.
- **A mount path for each.** `models.yml` goes where the worker image's Oh My Pi profile reads it,
  `/home/legion/.omp/profiles/legion/agent/models.yml`. `overlay.yml` goes anywhere the operator
  chooses, and `pod.env`'s `PI_CONFIG_FILES` names that path.
- **The credential the provider's key command reads, as a mounted file.** `models.yml`'s `apiKey`
  is a command (`!cat <path>`), and `pod.yml` mounts the directory holding it at
  `/var/run/operator`.
- **The model endpoint's base URL**, which the operator puts in place of `models.yml`'s
  `${MODEL_BASE_URL}` placeholder in their own copy, so the repository holds no endpoint.
- **The audience its model endpoint accepts on the pod's projected ServiceAccount token**, which the
  operator puts in place of `pod.yml`'s `${MODEL_TOKEN_AUDIENCE}` placeholder when copying `pod.yml`
  into `legion.yaml`, so the repository holds no audience either.

Every mount in `pod.yml` is read-only: `read_only` defaults to true, and none of them sets it false.
A mount added later goes beside these paths, never beneath one: the container runtime must create
a nested mount's mountpoint inside the read-only mount above it, and runc refuses with a read-only
filesystem error.

## Standing it up

Copy `models.yml` and `overlay.yml` into a directory of the operator's own (for example
`~/.local/state/legion-model-config`), put the model endpoint's base URL in place of
`${MODEL_BASE_URL}` in that copy of `models.yml`, and write both into the ConfigMap,
`legion-operator-route` in namespace `legion`:

```bash
deploy/kubernetes/operator-route/apply.sh --context <kube context> <directory>
```

`--context` is required, and the output names the cluster it wrote to. `apply.sh` refuses a
`models.yml` still holding the placeholder. Then put `pod.yml` under `runtime.kubernetes.pod` in
the deployment's `legion.yaml`, with the gateway's audience in place of `${MODEL_TOKEN_AUDIENCE}`,
and run `legion start --config <file> --check-config`, which applies the daemon's own collision
checks and refuses a token audience still holding the placeholder. Every deployment whose pods
mount `legion-operator-route` in that namespace reads the same copy.

## Changing a role's model

The two files in that directory are the operator's, and `apply.sh` is the only writer of the
ConfigMap. To change the model a role uses, edit that role's line under `modelRoles` in
`overlay.yml` and run the same `apply.sh` command; nothing in this repository changes. Pods started
after that read the new file. A running pod keeps the files it started with until it restarts,
because `pod.yml` mounts both by `subPath`, which the kubelet never refreshes.

The ConfigMap carries no `legion.dev/project` label. A live harness run creates its own copy under a
run-scoped name and deletes only objects labelled with its own run's project, so it never touches
this one.

## Signing in as the Legion machine user

A gateway that admits Cognito user tokens takes a token from `legion model-token`, which the worker
image carries at `/opt/legion/go/bin/legion`. Oh My Pi runs it as the model `apiKey` and runs it
again after a 401. It signs in with Cognito's custom authentication (`InitiateAuth` with
`CUSTOM_AUTH`, then `RespondToAuthChallenge`), answering the challenge with the pod's projected
service-account token. It prints the access token and caches it, owner-only, on the pod's
memory-backed state volume until five minutes before it expires, then signs in again. It never
stores or uses the refresh token Cognito also returns. A refused sign-in exits non-zero with the
reason on stderr and prints no token. A token it cannot cache is still printed, with the cache
failure on stderr: the image probe's pod has no state volume.

Every value is the operator's, passed as flags in their own `models.yml`:

```yaml
    apiKey: "!/opt/legion/go/bin/legion model-token --region <pool region> --client-id <app client id> --username <machine user> --service-account-token-file /var/run/operator/token --cache-file /var/run/legion/state/model-token"
```

The service-account token is the projected token `pod.yml` already mounts at
`/var/run/operator/token`, with the operator's dedicated sign-in audience in place of
`${MODEL_TOKEN_AUDIENCE}`; `legion start --check-config` refuses that placeholder unfilled. Set
`X-Api-Key` to the same command. The examples here keep the gateway's current route and model names
until the operator switches their own copies.

## How the pieces compose

`runtime.kubernetes.pod` delivers **files and variables**; `provider_keys` delivers **variables from
a Secret**, and they meet nowhere:

- The route's credential is a **file**, which `models.yml`'s key command reads. It needs no
  `provider_keys` entry, and using one would put the key in the agent's environment for no gain.
- `provider_keys` names variables Oh My Pi or the extension reads that must come from the providers
  Secret. The shim exports each into the Oh My Pi child's environment only, after the pod's own
  variables, so a `provider_keys` name that collides with one of `pod.env` is refused at config load.
- `pod.env`'s `PI_CONFIG_FILES` names the operator's overlay. Legion's own pod baseline is written
  first in that list, so this overlay outranks it, and both outrank a repository's `.omp/config.yml`.
