# The operator's model route

Legion carries no model, provider or route knowledge. Everything a Legion pod needs to reach a model
is the operator's, delivered through `runtime.kubernetes.pod` and `provider_keys`
(`docs/kubernetes.md`, "Operator configuration"). [`pod.yml`](pod.yml) is the contract: the pieces
an operator supplies, in the shape the daemon loads. The Go live harnesses run on it, with the
`models.yml` and `overlay.yml` beside it.

## What an operator supplies

- **A ConfigMap** with two keys, named where `pod.yml` names it (`legion-operator-route`):
  `models.yml`, an Oh My Pi provider file, and `overlay.yml`, an Oh My Pi settings overlay.
- **A mount path for each.** `models.yml` goes where the worker image's Oh My Pi profile reads it,
  `/home/legion/.omp/profiles/legion/agent/models.yml`. `overlay.yml` goes anywhere the operator
  chooses, and `pod.env`'s `PI_CONFIG_FILES` names that path.
- **The credential the provider's key command reads, as a mounted file.** `models.yml`'s `apiKey`
  is a command (`!cat <path>`), and `pod.yml` mounts the directory holding it at
  `/var/run/operator`.
- **The model endpoint's base URL**, which `apply.sh` puts in place of `models.yml`'s
  `${MODEL_BASE_URL}` placeholder, so the repository holds no endpoint.

Every mount in `pod.yml` is read-only: `read_only` defaults to true, and none of them sets it false.
A mount added later goes beside these paths, never beneath one: the container runtime must create
a nested mount's mountpoint inside the read-only mount above it, and runc refuses with a read-only
filesystem error.

## Standing it up

```bash
deploy/kubernetes/operator-route/apply.sh --context <kubectl context> --base-url https://<endpoint>
```

`--context` is required, and the output names the cluster it wrote to. Then put `pod.yml` under
`runtime.kubernetes.pod` in the deployment's `legion.yaml` and run
`legion start --check-config <file>`, which applies the daemon's own collision checks.

`pod.yml` mounts both files by `subPath`, and the kubelet never refreshes a `subPath` mount. A
changed ConfigMap therefore reaches only pods created after the change; a running pod keeps the
files it started with until it is relaunched.

The ConfigMap carries no `legion.dev/project` label. A live harness run creates its own copy under a
run-scoped name and deletes only objects labelled with its own run's project, so it never touches
this one.

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
