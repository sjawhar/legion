import { STATUS_CODES } from "node:http";
import { parseArgs } from "node:util";

/** The kinds of answer a gateway in front of Dispatch gives in its place. */
const BODIES = ["html", "empty", "text", "json"] as const;

type StandInBody = (typeof BODIES)[number];

export interface StandInOptions {
  readonly status: number;
  readonly body: StandInBody;
  readonly port: number;
}

/**
 * `bin/stand-in-gateway.ts`'s flags, `--flag value` or `--flag=value` in any order: `--status`
 * (200-599, what `Response` accepts; 502 when omitted), `--body` (one of `BODIES`; `html` when
 * omitted) and `--port` (0 for an ephemeral port, the default). An unknown flag, a flag without
 * a value and a value out of range each throw, naming it.
 */
export function parseStandInArguments(argv: readonly string[]): StandInOptions {
  const { values } = parseArgs({
    args: [...argv],
    strict: true,
    allowPositionals: false,
    options: {
      status: { type: "string", default: "502" },
      body: { type: "string", default: "html" },
      port: { type: "string", default: "0" },
    },
  });
  const body = BODIES.find((kind) => kind === values.body);
  if (body === undefined) {
    throw new Error(`--body must be html, empty, text or json, not ${values.body}`);
  }
  return {
    status: integerIn("--status", values.status, 200, 599),
    body,
    port: integerIn("--port", values.port, 0, 65_535),
  };
}

function integerIn(flag: string, value: string, low: number, high: number): number {
  const number = Number(value);
  if (!Number.isInteger(number) || number < low || number > high) {
    throw new Error(`${flag} must be an integer from ${low} to ${high}, not ${value}`);
  }
  return number;
}

/**
 * The answer a gateway of each kind gives under `status`: nginx's error page, nothing at all,
 * Envoy proxy's one-line reset, or JSON that is not Dispatch's `{code, error}`.
 */
export function standInAnswer(status: number, body: StandInBody): Response {
  switch (body) {
    case "empty":
      return new Response(null, { status });
    case "text":
      return new Response("upstream connect error or disconnect/reset before headers", {
        status,
        headers: { "content-type": "text/plain" },
      });
    case "json":
      return new Response(JSON.stringify({ message: "upstream request timeout" }), {
        status,
        headers: { "content-type": "application/json" },
      });
    case "html": {
      const title = `${status} ${STATUS_CODES[status] ?? "Gateway"}`;
      return new Response(
        `<html>\r\n<head><title>${title}</title></head>\r\n<body>\r\n<center><h1>${title}</h1>` +
          "</center>\r\n<hr><center>nginx/1.25.3</center>\r\n</body>\r\n</html>\r\n",
        { status, headers: { "content-type": "text/html" } }
      );
    }
  }
}
