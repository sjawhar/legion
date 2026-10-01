import { STATUS_CODES } from "node:http";

/** The kinds of answer a gateway in front of Dispatch gives in its place. */
const BODIES = ["html", "empty", "text", "json"] as const;

export type StandInBody = (typeof BODIES)[number];

export interface StandInOptions {
  readonly status: number;
  readonly body: StandInBody;
  readonly port: number;
}

/**
 * `bin/stand-in-gateway.ts`'s flags, as `--flag value` pairs in any order: `--status` (200-599,
 * what `Response` accepts; 502 when omitted), `--body` (one of `BODIES`; `html` when omitted) and
 * `--port` (0 for an ephemeral port, the default). Throws naming the first flag it cannot read.
 */
export function parseStandInArguments(argv: readonly string[]): StandInOptions {
  let status = 502;
  let body: StandInBody = "html";
  let port = 0;
  for (let index = 0; index < argv.length; index += 2) {
    const flag = argv[index];
    const value = argv[index + 1];
    if (value === undefined) throw new Error(`${flag} needs a value`);
    if (flag === "--status") {
      status = integerIn(flag, value, 200, 599);
    } else if (flag === "--port") {
      port = integerIn(flag, value, 0, 65_535);
    } else if (flag === "--body") {
      const kind = BODIES.find((candidate) => candidate === value);
      if (kind === undefined) {
        throw new Error(`--body must be html, empty, text or json, not ${value}`);
      }
      body = kind;
    } else {
      throw new Error(`unknown option ${flag} ${value}`);
    }
  }
  return { status, body, port };
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
