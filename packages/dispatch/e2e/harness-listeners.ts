import { randomBytes } from "node:crypto";
import { fileURLToPath } from "node:url";

import { usesFakeBroker } from "./harness-broker";
import { type HarnessPort, harnessPorts } from "./harness-ports";
import { harnessProcessCount } from "./harness-processes";

/** One process the harness starts, beside the port it listens on and that port's variable. */
export interface HarnessListener extends HarnessPort {
  /** What a message calls it. */
  readonly name: string;
  readonly argv: readonly string[];
  /** Variables this process gets on top of the caller's environment: in two-process mode, each
   *  Dispatch server's own inputs to run-server.sh. */
  readonly env?: Readonly<Record<string, string>>;
  /** Whether a deployed run (`PLAYWRIGHT_BASE_URL`) starts it. */
  readonly deployed: boolean;
}

const script = (name: string) => fileURLToPath(new URL(`./${name}`, import.meta.url));

// Two-process mode (e2e/harness-processes.ts): Dispatch A and B, each a run-server.sh on its own
// port against the one DATABASE_URL, with B on DISPATCH_E2E_WEB_DIST_B's build when it is set
// (run-server.sh reads that path from packages/envoy, as the server reads DISPATCH_WEB_DIST),
// and the round-robin proxy on DISPATCH_E2E_PORT, which every helper addresses. Both servers sign
// cookies with the key their one shared value derives, so a person signed in through whichever
// answered stays signed in on the other; it is generated here, once per run, since each
// run-server.sh is a process of its own. Every importer generates one, and only the process that
// starts the servers hands its own to both.
function dispatchListeners(): HarnessListener[] {
  const server = ["bash", script("run-server.sh")];
  if (harnessProcessCount === 1) {
    return [
      { ...harnessPorts.dispatch, argv: server, deployed: false, name: "the Dispatch server" },
    ];
  }
  const sharedKey = randomBytes(32).toString("base64url");
  const webDistB = process.env.DISPATCH_E2E_WEB_DIST_B;
  const inputs = (port: HarnessPort, webDist: string | undefined) => ({
    DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY: sharedKey,
    DISPATCH_E2E_SERVER_PORT: String(port.port),
    ...(webDist ? { DISPATCH_E2E_SERVER_WEB_DIST: webDist } : {}),
  });
  return [
    {
      ...harnessPorts.dispatchA,
      argv: server,
      deployed: false,
      env: inputs(harnessPorts.dispatchA, undefined),
      name: "Dispatch A",
    },
    {
      ...harnessPorts.dispatchB,
      argv: server,
      deployed: false,
      env: inputs(harnessPorts.dispatchB, webDistB),
      name: "Dispatch B",
    },
    {
      ...harnessPorts.dispatch,
      argv: ["bun", script("round-robin-proxy.ts")],
      deployed: false,
      name: "the round-robin proxy",
    },
  ];
}

// Every listener the harness can start, with its port beside that port's variable, both from
// e2e/harness-ports.ts. A deployed run starts only those marked `deployed`: its Dispatch server is
// already up, and the fake GitHub serves only a server this run starts. The fake broker is listed
// exactly when the harness switch points this run's server at it (e2e/harness-broker.ts), never
// for a deployed run. e2e/playwright.config.ts probes and starts the rows a run needs, and
// `bun run dispatch:local` (e2e/local-instance.ts) starts every row of the one-server harness, so
// the two run one harness.
export const harnessListeners: readonly HarnessListener[] = [
  {
    ...harnessPorts.fakeEnvoy,
    argv: ["bun", script("fake-envoy.ts")],
    deployed: true,
    name: "the fake Envoy",
  },
  {
    ...harnessPorts.plainHttp,
    argv: ["bun", script("plain-http-proxy.ts")],
    deployed: true,
    name: "the plain-HTTP proxy",
  },
  {
    ...harnessPorts.fakeGithub,
    argv: ["bun", script("fake-github.ts")],
    deployed: false,
    name: "the fake GitHub",
  },
  ...(usesFakeBroker
    ? [
        {
          ...harnessPorts.fakeBroker,
          argv: ["bun", script("fake-broker.ts")],
          deployed: false,
          name: "the fake secrets broker",
        },
      ]
    : []),
  ...dispatchListeners(),
];
