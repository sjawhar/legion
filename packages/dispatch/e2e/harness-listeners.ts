import { fileURLToPath } from "node:url";

import { usesFakeBroker } from "./harness-broker";
import { type HarnessPort, harnessPorts } from "./harness-ports";

/** One process the harness starts, beside the port it listens on and that port's variable. */
export interface HarnessListener extends HarnessPort {
  /** What a message calls it. */
  readonly name: string;
  readonly argv: readonly string[];
  /** Whether a deployed run (`PLAYWRIGHT_BASE_URL`) starts it. */
  readonly deployed: boolean;
}

const script = (name: string) => fileURLToPath(new URL(`./${name}`, import.meta.url));

// Every listener the harness can start, with its port beside that port's variable, both from
// e2e/harness-ports.ts. A deployed run starts only those marked `deployed`: its Dispatch server is
// already up, and the fake GitHub serves only a server this run starts. The fake broker is listed
// exactly when the harness switch points this run's server at it (e2e/harness-broker.ts), never
// for a deployed run. e2e/playwright.config.ts probes and starts the rows a run needs, and
// `bun run dispatch:local` (e2e/local-instance.ts) starts every row, so the two run one harness.
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
  {
    ...harnessPorts.dispatch,
    argv: ["bash", script("run-server.sh")],
    deployed: false,
    name: "the Dispatch server",
  },
];
