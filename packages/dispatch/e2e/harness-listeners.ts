import { randomBytes } from "node:crypto";
import { fileURLToPath } from "node:url";

import { usesFakeBroker } from "./harness-broker";
import type { HarnessListenerKey } from "./harness-ports";
import { harnessProcessCount } from "./harness-processes";
import { usesRealBroker } from "./harness-real-broker";
import { harnessSignIn, oidcSignIn } from "./harness-signin";

/** One process the harness starts and how it reports its port. */
export interface HarnessListener {
  /** Its entry in `harnessPorts` and `harnessVariables` (e2e/harness-ports.ts), whose variable,
   *  set, is the port it binds; unset, the OS chooses and it reports. */
  readonly key: HarnessListenerKey;
  /** What a message calls it. */
  readonly name: string;
  /** Its log file's name, without `.log`, when its output goes to a directory. */
  readonly id: string;
  readonly argv: readonly string[];
  /** Variables this process gets on top of the harness's environment: in two-process mode, each
   *  Dispatch server's own inputs to run-server.sh, and the dev OIDC issuer's to
   *  run-signin-issuer.sh. */
  readonly env?: Readonly<Record<string, string>>;
  /** Set for a row whose port a row started before it must name: in two-process mode Dispatch A's
   *  and B's, which the round-robin proxy forwards to and which serve the proxy's origin, so the
   *  proxy starts first. With the row's port variable unset, e2e/harness-start.ts holds a port the
   *  OS chose for it before any row starts (a `held-port.py keep`), exports it under that
   *  variable, and hands the row the keeper's unix socket in `keeperEnv`; set, it hands the row
   *  that port in `portEnv`. */
  readonly heldAhead?: { readonly keeperEnv: string; readonly portEnv: string };
  /** The line it prints once it serves, whose one group is the port it serves on. */
  readonly report: RegExp;
  /** A path that answers 200 once it is ready, when listening is not enough. */
  readonly health?: string;
  /** How long it may take to report and be ready. */
  readonly bootMs: number;
  /** Whether a deployed run (`PLAYWRIGHT_BASE_URL`) starts it. */
  readonly deployed: boolean;
}

const script = (name: string) => fileURLToPath(new URL(`./${name}`, import.meta.url));

/** run-server.sh builds the server before it listens, and a cold `go build` on a busy machine is
 *  the long pole, so the server gets ten minutes. */
const serverBootMs = 600_000;
const listenerBootMs = 30_000;

/** A run-server.sh row: the server's slog line, written with the address it serves on (a held
 *  socket's, or the one it bound), is `msg="dispatch: listening" addr=127.0.0.1:<port>`. */
const dispatchServer = {
  argv: ["bash", script("run-server.sh")],
  bootMs: serverBootMs,
  deployed: false,
  health: "/healthz",
  report: /msg="dispatch: listening" addr=127\.0\.0\.1:(\d+)(?:\s|$)/,
} as const;

// Two-process mode (e2e/harness-processes.ts): the round-robin proxy on DISPATCH_E2E_PORT, which
// every helper addresses, then Dispatch A and B, each a run-server.sh against the one
// DATABASE_URL, with B on DISPATCH_E2E_WEB_DIST_B's build when it is set (run-server.sh reads that
// path from packages/envoy, as the server reads DISPATCH_WEB_DIST). Both serve the proxy's origin,
// so they start after it, on ports held for them before it started (`heldAhead`). Both sign
// cookies with one key, so a person signed in through whichever answered stays signed in on the
// other: under dev sign-in the key their one shared value derives, under DISPATCH_E2E_SIGNIN=oidc
// (e2e/harness-signin.ts) one DISPATCH_SIGNING_KEY, beside the dev OIDC issuer's client secret.
// Each is generated here, once per run, since each run-server.sh is a process of its own. Every
// importer generates its own, and only the process that starts the servers hands its own to them.
const clientSecret = randomBytes(32).toString("base64url");

function dispatchListeners(): HarnessListener[] {
  if (harnessProcessCount === 1) {
    return [{ ...dispatchServer, id: "dispatch", key: "dispatch", name: "the Dispatch server" }];
  }
  const sharedKey = randomBytes(32).toString("base64url");
  const webDistB = process.env.DISPATCH_E2E_WEB_DIST_B;
  const heldAhead = {
    keeperEnv: "DISPATCH_E2E_LISTEN_FROM",
    portEnv: "DISPATCH_E2E_SERVER_PORT",
  } as const;
  const signInInputs: Record<string, string> =
    harnessSignIn === "oidc"
      ? {
          DISPATCH_E2E_SIGNIN_CLIENT_ID: oidcSignIn.clientId,
          DISPATCH_E2E_SIGNIN_CLIENT_SECRET: clientSecret,
          DISPATCH_E2E_SIGNIN_GROUP: oidcSignIn.group,
          DISPATCH_E2E_SIGNING_KEY: sharedKey,
        }
      : { DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY: sharedKey };
  const inputs = (webDist: string | undefined) => ({
    ...signInInputs,
    ...(webDist ? { DISPATCH_E2E_SERVER_WEB_DIST: webDist } : {}),
  });
  return [
    {
      argv: ["bun", script("round-robin-proxy.ts")],
      bootMs: listenerBootMs,
      deployed: false,
      id: "round-robin-proxy",
      key: "dispatch",
      name: "the round-robin proxy",
      report: /^round-robin proxy on http:\/\/127\.0\.0\.1:(\d+) for /,
    },
    {
      ...dispatchServer,
      env: inputs(undefined),
      heldAhead,
      id: "dispatch-a",
      key: "dispatchA",
      name: "Dispatch A",
    },
    {
      ...dispatchServer,
      env: inputs(webDistB),
      heldAhead,
      id: "dispatch-b",
      key: "dispatchB",
      name: "Dispatch B",
    },
  ];
}

// Under DISPATCH_E2E_SIGNIN=oidc, the dev OIDC issuer both Dispatch servers sign people in
// through (e2e/run-signin-issuer.sh), started before them since each takes its URL, built from
// the port it reports, as DISPATCH_SIGNIN_ISSUER. It serves the code flow for Dispatch's client,
// with the secret both servers are handed, and signs in oidcSignIn's person, a member of the group
// they admit, at every authorization.
function signInIssuerListeners(): HarnessListener[] {
  if (harnessSignIn !== "oidc") return [];
  return [
    {
      argv: ["bash", script("run-signin-issuer.sh")],
      bootMs: serverBootMs,
      deployed: false,
      env: {
        DISPATCH_E2E_SIGNIN_AS: oidcSignIn.person,
        DISPATCH_E2E_SIGNIN_CLIENT_ID: oidcSignIn.clientId,
        DISPATCH_E2E_SIGNIN_CLIENT_SECRET: clientSecret,
        DISPATCH_E2E_SIGNIN_GROUP: oidcSignIn.group,
      },
      id: "signin-issuer",
      key: "signinIssuer",
      name: "the dev OIDC issuer",
      report: /issuer listening url=http:\/\/127\.0\.0\.1:(\d+)$/,
    },
  ];
}

// The real secrets broker and the dev OIDC issuer it trusts (e2e/run-broker.sh), which
// e2e/broker-approval.e2e.ts drives an approval on, started only by a run that can reach that
// spec (e2e/harness-real-broker.ts). The issuer goes first: it is the broker's ServiceAccount
// token issuer and the provider a person signs in to the broker at, and the broker discovers it
// before it binds. The broker names it by DEV_OIDC_ISSUER_PORT, which e2e/harness-start.ts
// exports from the issuer's report into the environment of every row after it, so no row here
// has to carry an input of its own. Both follow the Dispatch server, whose own
// DISPATCH_E2E_PORT the broker names in the machine-login link on its page; neither serves that
// server, which keeps relaying the fake broker.
// Each half reports on stderr, and each report is the line that follows its own bind: the issuer
// writes `dev-oidc-issuer: issuer listening url=http://127.0.0.1:<port>`, as it does for
// run-signin-issuer.sh, and cmd/broker writes `broker listening addr=127.0.0.1:<port>` through
// slog's own default logger, which the Go `log` package prints as
// `<date> <time> INFO broker listening addr=127.0.0.1:<port>` — no `msg=` key, unlike the
// Dispatch server's, which installs a handler of its own.
function realBrokerListeners(): HarnessListener[] {
  if (!usesRealBroker) return [];
  return [
    {
      argv: ["bash", script("run-broker.sh"), "issuer"],
      bootMs: serverBootMs,
      deployed: false,
      id: "dev-oidc-issuer",
      key: "devIssuer",
      name: "the real broker's dev OIDC issuer",
      report: /issuer listening url=http:\/\/127\.0\.0\.1:(\d+)$/,
    },
    {
      argv: ["bash", script("run-broker.sh"), "broker"],
      bootMs: serverBootMs,
      deployed: false,
      id: "real-broker",
      key: "realBroker",
      name: "the real secrets broker",
      report: /\bINFO broker listening addr=127\.0\.0\.1:(\d+)(?:\s|$)/,
    },
  ];
}

// Every listener the harness can start, in the order e2e/harness-start.ts starts them: the fakes,
// then under DISPATCH_E2E_SIGNIN=oidc the dev OIDC issuer, then the Dispatch server (in
// two-process mode the round-robin proxy and the two servers behind it), whose environment names
// the fakes' ports and the issuer's, then the real broker's dev issuer and the real broker, then
// the plain-HTTP proxy, which forwards to the server's origin. A deployed run starts only those
// marked `deployed`: its Dispatch server is already up, and the fake GitHub serves only a server
// this run starts. The fake broker is listed exactly when the harness switch points this run's
// server at it (e2e/harness-broker.ts), never for a deployed run, and the real broker and its
// issuer exactly when a spec of this run can reach them (e2e/harness-real-broker.ts).
// e2e/harness-start.ts is the only starter: e2e/run.ts (Playwright), `bun run dispatch:local`
// (e2e/local-instance.ts) and the docs media harness all start this table through it.
export const harnessListeners: readonly HarnessListener[] = [
  {
    argv: ["bun", script("fake-envoy.ts")],
    bootMs: listenerBootMs,
    deployed: true,
    id: "fake-envoy",
    key: "fakeEnvoy",
    name: "the fake Envoy",
    report: /^fake envoy listener on 127\.0\.0\.1:(\d+)$/,
  },
  {
    argv: ["bun", script("fake-github.ts")],
    bootMs: listenerBootMs,
    deployed: false,
    id: "fake-github",
    key: "fakeGithub",
    name: "the fake GitHub",
    report: /^fake github listener on 127\.0\.0\.1:(\d+)$/,
  },
  ...(usesFakeBroker
    ? [
        {
          argv: ["bun", script("fake-broker.ts")],
          bootMs: listenerBootMs,
          deployed: false,
          id: "fake-broker",
          key: "fakeBroker" as const,
          name: "the fake secrets broker",
          report: /^fake broker listener on 127\.0\.0\.1:(\d+)$/,
        },
      ]
    : []),
  ...signInIssuerListeners(),
  ...dispatchListeners(),
  ...realBrokerListeners(),
  {
    argv: ["bun", script("plain-http-proxy.ts")],
    bootMs: listenerBootMs,
    deployed: true,
    id: "plain-http-proxy",
    key: "plainHttp",
    name: "the plain-HTTP proxy",
    report: /^plain-HTTP proxy on http:\/\/dispatch-e2e\.test:(\d+) for /,
  },
];

/** The rows a run targets: every one for a local run, the `deployed` ones for a deployed run
 *  (`PLAYWRIGHT_BASE_URL`, read when this is called). */
export function targetedListeners(): readonly HarnessListener[] {
  return process.env.PLAYWRIGHT_BASE_URL
    ? harnessListeners.filter((listener) => listener.deployed)
    : harnessListeners;
}
