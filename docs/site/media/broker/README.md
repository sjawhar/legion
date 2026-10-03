# Secrets broker media

The scripts that make the secrets broker's screenshots and narrated walkthrough. Everything they
show runs on one machine on example data: the Dispatch e2e workspace (whose signed-in human is
`alice`), a broker on example rules whose one secret, `DEMO_API_KEY`, holds a made-up value, and an
agent machine whose hostname is `example-host-build`. No credential, private hostname or
production service is involved.

| File | What it is |
| --- | --- |
| `rig.sh` | Starts the whole stack and stays in the foreground, or runs a command against it (`rig.sh -- <command>`) and then stops it. |
| `seed.ts` | Seeds the Dispatch e2e workspace, the same one the e2e suite builds. |
| `agent.ts` | Drives the agent machine from a script: a machine login, a secret request. |
| `agent/` | What the agent machine mounts: its shell prompt and the demo command, `check-demo-key.sh`. |
| `screenshots.sh`, `screenshots.spec.ts` | Retake the screenshots into `docs/site/src/assets/broker/`. |
| `walkthrough.sh`, `walkthrough.record.ts` | Record the walkthrough's raw footage into `docs/site/public/media/broker-walkthrough.src/raw/`. |
| `playwright.config.ts` | The Playwright config both specs run under; it starts no server, since the rig has. |

## The rig

```bash
bash docs/site/media/broker/rig.sh
```

It builds the broker, `agent-secrets` and `agent-secrets-helper`; starts Postgres (a throwaway
container, or the server `DATABASE_URL` names); starts the broker on a local rules file and its
development secrets file; starts the Dispatch e2e harness's three servers with the server pointed
at the broker (`packages/dispatch/e2e/run-server.sh`'s `DISPATCH_E2E_AGENT_SECRETS_URL`), and seeds
the workspace; and starts the agent machine, a container running `agent-secrets-helper` for
`alice`. It prints the addresses, then waits; Ctrl-C stops everything it started.

It needs `docker`, `go`, `bun`, `psql`, `curl` and `openssl`. Its inputs are optional:
`DATABASE_URL` (a database it may truncate, as the e2e harness requires; the broker's database is
created beside it), the harness ports `DISPATCH_E2E_PORT`, `FAKE_ENVOY_PORT` and `FAKE_GITHUB_PORT`
(8777, 9021, 9022 by default), and `BROKER_RIG_NAME`, the prefix of its containers.

Dispatch identifies the person by the `X-Dispatch-User` header, so a browser reaches it through a
script that sets it (as both specs do) or an extension that adds the header.

From a shell on the agent machine (`docker exec -it legion-docs-broker-agent bash`), the flow the
walkthrough shows is:

```bash
agent-secrets launcher login                    # prints a code; approve it at /credentials/machine
agent-secrets register --wait 10 --exec -- bash # a session, registered as an agent's session is
agent-secrets DEMO_API_KEY --reason "Publish the docs preview" -- ./check-demo-key.sh
```

## Screenshots

```bash
bash docs/site/media/broker/screenshots.sh
```

Boots the rig, approves a machine login and a secret request through Dispatch, and writes
`machine-login.png`, `inbox-credential-request.png`, `credential-request.png`,
`credential-request-approved.png` and `live-grants.png` into `docs/site/src/assets/broker/`. Each
shot is taken only after the state it shows is asserted, and the run fails unless the approved
request's command ran with the secret.

## The walkthrough

The narrated video is `docs/site/public/media/broker-walkthrough.mp4`; its sources and the
rebuild steps are in `docs/site/public/media/broker-walkthrough.src/README.md`. Recording the raw
footage again:

```bash
bash docs/site/media/broker/walkthrough.sh
```
