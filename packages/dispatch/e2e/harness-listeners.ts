import { fileURLToPath } from "node:url";

import { usesFakeBroker } from "./harness-broker";
import { type HarnessListenerKey, harnessVariables } from "./harness-ports";

/** One process the harness starts, beside its port's variable and how it reports that port. */
export interface HarnessListener {
  /** Its entry in `harnessPorts` (e2e/harness-ports.ts). */
  readonly key: HarnessListenerKey;
  /** Its port's variable: set, the port it binds; unset, the OS chooses and it reports. */
  readonly variable: string;
  /** What a message calls it. */
  readonly name: string;
  /** Its log file's name, without `.log`, when its output goes to a directory. */
  readonly id: string;
  readonly argv: readonly string[];
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

// Every listener the harness can start, in the order e2e/harness-start.ts starts them: the fakes,
// then the Dispatch server, whose environment names the fakes' ports, then the plain-HTTP proxy,
// which forwards to the server's origin. A deployed run starts only those marked `deployed`: its
// Dispatch server is already up, and the fake GitHub serves only a server this run starts. The
// fake broker is listed exactly when the harness switch points this run's server at it
// (e2e/harness-broker.ts), never for a deployed run. e2e/harness-start.ts is the only starter:
// e2e/run.ts (Playwright), `bun run dispatch:local` (e2e/local-instance.ts) and the docs media
// harness all start this table through it.
export const harnessListeners: readonly HarnessListener[] = [
  {
    argv: ["bun", script("fake-envoy.ts")],
    bootMs: listenerBootMs,
    deployed: true,
    id: "fake-envoy",
    key: "fakeEnvoy",
    name: "the fake Envoy",
    report: /^fake envoy listener on 127\.0\.0\.1:(\d+)$/,
    variable: harnessVariables.fakeEnvoy,
  },
  {
    argv: ["bun", script("fake-github.ts")],
    bootMs: listenerBootMs,
    deployed: false,
    id: "fake-github",
    key: "fakeGithub",
    name: "the fake GitHub",
    report: /^fake github listener on 127\.0\.0\.1:(\d+)$/,
    variable: harnessVariables.fakeGithub,
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
          variable: harnessVariables.fakeBroker,
        },
      ]
    : []),
  {
    argv: ["bash", script("run-server.sh")],
    bootMs: serverBootMs,
    deployed: false,
    health: "/healthz",
    id: "dispatch",
    key: "dispatch",
    name: "the Dispatch server",
    // The server's slog line, written with the address it serves on (a held socket's, or the one
    // it bound): `msg="dispatch: listening" addr=127.0.0.1:<port>`.
    report: /msg="dispatch: listening" addr=127\.0\.0\.1:(\d+)(?:\s|$)/,
    variable: harnessVariables.dispatch,
  },
  {
    argv: ["bun", script("plain-http-proxy.ts")],
    bootMs: listenerBootMs,
    deployed: true,
    id: "plain-http-proxy",
    key: "plainHttp",
    name: "the plain-HTTP proxy",
    report: /^plain-HTTP proxy on http:\/\/dispatch-e2e\.test:(\d+) for /,
    variable: harnessVariables.plainHttp,
  },
];
