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
| `screenshots.sh`, `screenshots.spec.ts` | Retake the screenshots into `docs/site/public/media/broker/`. |
| `walkthrough.sh`, `walkthrough.record.ts` | Record the walkthrough's raw footage into `walkthrough/raw/`. |
| `walkthrough/` | The walkthrough's footage, cut, narration and build, which writes `docs/site/public/media/broker/walkthrough.mp4`. |
| `playwright.config.ts` | The Playwright config both specs run under; it starts no server, since the rig has. |

## The rig

```bash
DATABASE_URL=<url> bash docs/site/media/broker/rig.sh
```

It builds the broker, `agent-secrets` and `agent-secrets-helper`; creates the broker's database
beside `DATABASE_URL`'s and starts the broker on a local rules file and its development secrets
file; starts the Dispatch e2e harness's three servers on `DATABASE_URL` with the server pointed at
the broker (`packages/dispatch/e2e/run-server.sh`'s `DISPATCH_E2E_AGENT_SECRETS_URL`), and seeds
the workspace; and starts the agent machine, `agent-secrets-helper` for `alice` under the hostname
`example-host-build`, alone in a UTS namespace of its own, with the agent's shells on this
machine. It prints the addresses, then waits; Ctrl-C stops everything it started and drops the
broker's database.

It needs `go`, `bun`, `psql`, `curl`, `openssl`, `setsid`, and passwordless `sudo` with `unshare`
and `setpriv`. Its one input, `DATABASE_URL`, names a Postgres database on this machine that it may
empty, as the e2e harness requires: a `postgres://` URL whose host is a loopback name or address or
a unix socket directory. The rig refuses any other before it runs a single `psql`, and it ignores
the variables that could send its SQL or seed elsewhere (`PGHOST`, `PGHOSTADDR`, `PGSERVICE`, and a
deployed e2e run's `PLAYWRIGHT_BASE_URL`, `PLAYWRIGHT_DATABASE_URL` and `E2E_AGENT_TOKEN`). The
broker's database is created beside it under a name of the run's own and dropped on exit. The rig
picks its ports itself (`scripts/e2e/lib/rig.sh`'s `pick_port`), so two rigs on one machine never
meet.

Postgres from the distribution's package serves, unpacked rather than installed (Ubuntu 24.04
shown). It listens on no TCP port, only on a socket in a directory no other account can enter, and
admits only the account that runs it (peer authentication), so nobody else on the machine reaches
its superuser:

```bash
apt-get download postgresql-16 && dpkg-deb -x postgresql-16_*.deb /tmp/pgroot
pgbin=/tmp/pgroot/usr/lib/postgresql/16/bin
install -d -m 700 /tmp/pgsock
$pgbin/initdb -D /tmp/pgdata -U "$(id -un)" --auth=peer
$pgbin/pg_ctl -D /tmp/pgdata -l /tmp/pg.log -o "-c listen_addresses='' -c unix_socket_directories=/tmp/pgsock" -w start
psql "postgres:///postgres?host=/tmp/pgsock" -c "create database dispatch"
DATABASE_URL="postgres:///dispatch?host=/tmp/pgsock" bash docs/site/media/broker/rig.sh
```

`$pgbin/pg_ctl -D /tmp/pgdata stop` and removing `/tmp/pgroot`, `/tmp/pgdata` and `/tmp/pgsock` undo
it.

Dispatch signs a person in with its session cookie, which the harness server mints at its dev
sign-in route: open the sign-in address the rig prints, `<Dispatch>/auth/_dev/signin?login=alice`,
in the browser (by `127.0.0.1`, never `localhost`, which the server refuses). Both specs sign in the
same way, through the e2e harness's `signIn` (`packages/dispatch/e2e/users.ts`).

From a shell on the agent machine (`agent-exec bash`, where `agent-exec` is the script the rig
prints, and hands a command in `BROKER_RIG_AGENT_EXEC`), the flow the walkthrough shows is:

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
`credential-request-approved.png` and `live-grants.png` into `docs/site/public/media/broker/`. Each
shot is taken only after the state it shows is asserted, and the run fails unless the approved
request's command ran with the secret.

## The walkthrough

The narrated video is `docs/site/public/media/broker/walkthrough.mp4`; its sources, the rebuild
steps and its review are in `walkthrough/README.md`. Recording
the raw footage again:

```bash
bash docs/site/media/broker/walkthrough.sh
```
