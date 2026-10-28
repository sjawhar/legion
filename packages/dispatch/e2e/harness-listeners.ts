import { randomBytes } from "node:crypto";
import { fileURLToPath } from "node:url";

import { usesFakeBroker } from "./harness-broker";
import type { HarnessListenerKey } from "./harness-ports";
import { binaryB, harnessProcessCount } from "./harness-processes";

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
   *  Dispatch server's own inputs to run-server.sh. */
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
// so they start after it, on ports held for them before it started (`heldAhead`). B runs the
// envoy-dispatch DISPATCH_E2E_BINARY_B names when it is set (a path from packages/envoy too), in
// place of the one run-server.sh builds from this tree: a previous release's build, so a browser
// drives two builds of the server at once (e2e/cross-build.multiprocess.e2e.ts). Such a build
// cannot serve on a held socket, so it binds DISPATCH_E2E_PORT_B, which the run must then set.
// Both sign cookies with the key their one shared value derives, so a person signed in through
// whichever answered stays signed in on the other; it is generated here, once per run, since each
// run-server.sh is a process of its own. Every importer generates one, and only the process that
// starts the servers hands its own to both. A previous release knows no shared key and signs with
// its own (run-server.sh).
function dispatchListeners(): HarnessListener[] {
  if (harnessProcessCount === 1) {
    return [{ ...dispatchServer, id: "dispatch", key: "dispatch", name: "the Dispatch server" }];
  }
  const sharedKey = randomBytes(32).toString("base64url");
  const webDistB = process.env.DISPATCH_E2E_WEB_DIST_B;
  if (binaryB !== undefined && !process.env.DISPATCH_E2E_PORT_B) {
    throw new Error(
      "DISPATCH_E2E_BINARY_B names a server that cannot serve on a held socket. " +
        "Set DISPATCH_E2E_PORT_B to a free port for it."
    );
  }
  const heldAhead = {
    keeperEnv: "DISPATCH_E2E_LISTEN_FROM",
    portEnv: "DISPATCH_E2E_SERVER_PORT",
  } as const;
  const inputs = (webDist: string | undefined, binary: string | undefined) => ({
    DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY: sharedKey,
    ...(webDist ? { DISPATCH_E2E_SERVER_WEB_DIST: webDist } : {}),
    ...(binary ? { DISPATCH_E2E_SERVER_BINARY: binary } : {}),
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
      env: inputs(undefined, undefined),
      heldAhead,
      id: "dispatch-a",
      key: "dispatchA",
      name: "Dispatch A",
    },
    {
      ...dispatchServer,
      env: inputs(webDistB, binaryB),
      heldAhead,
      id: "dispatch-b",
      key: "dispatchB",
      name: "Dispatch B",
    },
  ];
}

// Every listener the harness can start, in the order e2e/harness-start.ts starts them: the fakes,
// then the Dispatch server (in two-process mode the round-robin proxy and the two servers
// behind it), whose environment names the fakes' ports, then the plain-HTTP proxy, which forwards
// to the server's origin. A deployed run starts only those marked `deployed`: its Dispatch server
// is already up, and the fake GitHub serves only a server this run starts. The fake broker is
// listed exactly when the harness switch points this run's server at it (e2e/harness-broker.ts),
// never for a deployed run. e2e/harness-start.ts is the only starter: e2e/run.ts (Playwright),
// `bun run dispatch:local` (e2e/local-instance.ts) and the docs media harness all start this
// table through it.
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
  ...dispatchListeners(),
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
