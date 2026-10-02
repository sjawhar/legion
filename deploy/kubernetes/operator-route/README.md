# The operator's model route

Legion carries no model names, provider routes, or gateway endpoint. An operator supplies the route
through `runtime.kubernetes.pod`; this directory is its public example. [`pod.yml`](pod.yml) supplies
the ConfigMap mount, and [`models.yml`](models.yml) and [`overlay.yml`](overlay.yml) are the ConfigMap
files. The examples retain the current model names so the operator can change names independently.

## What an operator supplies

- **A ConfigMap** with two keys, named where `pod.yml` names it (`legion-operator-route`):
  `models.yml`, an Oh My Pi provider file, and `overlay.yml`, an Oh My Pi settings overlay.
- **A mount path for each.** `models.yml` goes where the worker image's Oh My Pi profile reads it,
  `/home/legion/.omp/profiles/legion/agent/models.yml`. `overlay.yml` goes anywhere the operator
  chooses, and `pod.env`'s `PI_CONFIG_FILES` names that path.
- **The model endpoint's base URL**, which the operator puts in place of `models.yml`'s
  `${MODEL_BASE_URL}` placeholder in their own copy, so the repository holds no endpoint.
- **Optional `runtime.kubernetes.model_login`** when the gateway requires the standard Cognito
  machine-user sign-in. Operator configuration names only the command that reads the login document
  and the in-pod token file. The command document is one JSON object with non-empty
  `user_pool_id`, `region`, `username`, `password`, and `client_id` fields; the daemon derives the
  Cognito endpoint and client from that document. The document and its output never enter a pod
  specification, argument list, environment, or log.

```yaml
runtime:
  kubernetes:
    model_login:
      login_command: aws secretsmanager get-secret-value --secret-id <machine-login-document-id> --query SecretString --output text
      token_file: /var/run/legion/state/model-token
```

The command runs as the daemon on the operator host; the AWS CLI example uses that host's default
AWS credentials to read the Secrets Manager document.

The daemon signs in with `USER_PASSWORD_AUTH`, keeps the password and refresh token in memory, and
refreshes its access token with `REFRESH_TOKEN_AUTH` before it expires. It sends each access token to
connected worker shims; each shim atomically replaces `token_file` mode `0600`. `models.yml` reads
that file with `!cat`, which Oh My Pi reruns after a 401. `GET /legion/v1/state` reports
`modelLogin` as `pending`, `ready`, or `error` without exposing any credential material. A failed
login also emits the daemon's error log.

`legion start --config <file> --check-config` validates this shape and the token-file path without
running `login_command`.

## Standing it up

Copy `models.yml` and `overlay.yml` into a directory of the operator's own (for example
`~/.local/state/legion-model-config`), put the model endpoint's base URL in place of
`${MODEL_BASE_URL}` in that copy of `models.yml`, and write both into the ConfigMap
`legion-operator-route` in namespace `legion`:

```bash
deploy/kubernetes/operator-route/apply.sh --context <kube context> <directory>
```

`--context` is required, and the output names the cluster it wrote to. `apply.sh` refuses a
`models.yml` still holding the placeholder. Put `pod.yml` below `runtime.kubernetes.pod` and the
optional machine-login section alongside it in the deployment's `legion.yaml`, then run
`legion start --config <file> --check-config`. Every deployment whose pods mount
`legion-operator-route` in that namespace reads the same ConfigMap.

## Changing a role's model

The two files in that directory are the operator's, and `apply.sh` is the only writer of the
ConfigMap. To change the model a role uses, edit that role's line under `modelRoles` in `overlay.yml`
and run the same `apply.sh` command; nothing in this repository changes. Pods started after that
read the new file. A running pod keeps the files it started with until it restarts, because
`pod.yml` mounts both by `subPath`, which the kubelet never refreshes.

The ConfigMap carries no `legion.dev/project` label. A live harness run creates its own copy under a
run-scoped name and deletes only objects labelled with its own run's project, so it never touches
this one.

## How the pieces compose

`runtime.kubernetes.pod`, `runtime.kubernetes.model_login`, and `provider_keys` have separate
credential boundaries:

- The route's `models.yml` reads the access-token **file** `model_login.token_file`. The daemon
  writes it through the worker stream; it needs no `provider_keys` entry and no projected
  ServiceAccount token.
- `model_login` is daemon-only. A pod holds the current short-lived access token file, never the
  machine password or refresh token.
- `provider_keys` names variables Oh My Pi or the extension reads that must come from the providers
  Secret. The shim exports each into the Oh My Pi child's environment only, after the pod's own
  variables, so a `provider_keys` name that collides with one of `pod.env` is refused at config load.
- `pod.env`'s `PI_CONFIG_FILES` names the operator's overlay. Legion's own pod baseline is written
  first in that list, so this overlay outranks it, and both outrank a repository's `.omp/config.yml`.
