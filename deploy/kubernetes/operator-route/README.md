# The operator's model route

Legion carries no model, provider or route knowledge. Everything a Legion pod needs to reach a model
is the operator's, delivered through `runtime.kubernetes.pod` and `provider_keys`
(`docs/kubernetes.md`, "Operator configuration"). This directory is one operator's whole answer —
the route the Trajectory Labs cluster runs on — expressed with nothing but that affordance:

| file | what it is |
| :--- | :--- |
| [`models.yml`](models.yml) | the Oh My Pi provider: the model gateway's endpoint, keyed by the pod's projected ServiceAccount token, and the model ids the gateway serves |
| [`overlay.yml`](overlay.yml) | the Oh My Pi settings: which models are enabled, which providers are disabled, and the model role each agent Legion's prompts dispatch resolves through |
| [`pod.yml`](pod.yml) | the `runtime.kubernetes.pod` block: the env, the ConfigMap and projected-token volumes, their mounts, and the ServiceAccount |
| [`apply.sh`](apply.sh) | creates or updates the ConfigMap `pod.yml` mounts, substituting the operator's gateway endpoint for `models.yml`'s placeholder |

## Standing it up

```bash
deploy/kubernetes/operator-route/apply.sh --context <admin context> --base-url https://<gateway>/anthropic
```

Then paste [`pod.yml`](pod.yml) under `runtime.kubernetes.pod` in the deployment's `legion.yaml` and
run `legion start --check-config <file>`, which applies the same collision checks the daemon does at
load. The ConfigMap carries no `legion.dev/project` label, so a live harness run sharing the
namespace — whose teardown deletes by that label, and which creates its own copy of the route under
a run-scoped name — never touches it.

## One route or one per project

**The ServiceAccount and the audience are the cluster's, not a project's.** agent-c's two admission
policies name one of each for the whole `legion` namespace: `legion-sandbox-pods` admits a projected
token only on ServiceAccount `legion-worker`, alone in its volume, for audience `middleman-legion`,
lasting at most 3600 s; `legion-middleman-audience` refuses that audience to any pod the
agent-sandbox controller did not create in that namespace as that ServiceAccount. The gateway side
matches: middleman's Legion provider grants its Legion model access to every token of the cluster's
issuer and that audience, with no subject field — so there is no per-project identity to hold apart
even if a second ServiceAccount were admitted.

A second Legion project therefore uses **this same** `service_account` and the same projected-token
source. What it may hold apart is the pair of files: a project that wants different models or roles
applies its own ConfigMap under another name and names that name in its own `pod.yml`. One durable
copy is the default, and this is it.

## What a real deployment has that a harness run does not

- **A durable name and lifetime.** `legion-operator-route`, applied once and left, rather than
  `legion-operator-route-<run>` created and deleted around one run.
- **No placeholder left in the applied object.** `apply.sh` substitutes the endpoint; the file in
  the repository keeps `${MODEL_BASE_URL}` so no gateway address is committed.
- **Its own providers Secret**, when the deployment sets `provider_keys`: `legion-<project>-providers`,
  created by the operator, never by the daemon (`docs/kubernetes.md`, "The providers Secret").

## How the pieces compose

`runtime.kubernetes.pod` delivers **files and variables**; `provider_keys` delivers **variables from
a Secret**, and they meet nowhere:

- The route's credential is a **file** — the projected token at `/var/run/operator/token`, which
  `models.yml`'s `!cat` key command reads. It needs no `provider_keys` entry, and using one would put
  the key in the agent's environment for no gain.
- `provider_keys` names variables Oh My Pi or the extension reads that must come from the providers
  Secret. The shim exports each into the Oh My Pi child's environment only, after the pod's own
  variables, so a `provider_keys` name that collides with one of `pod.env` is refused at config load.
- `pod.env`'s `PI_CONFIG_FILES` names the operator's overlay. Legion's own pod baseline is written
  first in that list, so this overlay outranks it, and both outrank a repository's `.omp/config.yml`.
