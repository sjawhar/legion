// scripts/e2e/lib/recording-proxy.ts: a local reverse proxy in front of a model provider that keeps
// every request and response body, for a live proof that must show what reached the model (the
// answer loop's rig, scripts/e2e/answer-loop.sh: the requests a resumed agent sends, the reasoning
// and cache marks the daemon's loop sends, the usage a cost line is recomputed from).
//
//   bun scripts/e2e/lib/recording-proxy.ts --port <port> --upstream <url> --record <file.jsonl>
//     [--hold-dir <dir>] [--stand-in-dir <dir>]
//
// It prints `recording-proxy listening on 127.0.0.1:<port>` once it serves (--port 0 picks a free
// port and prints it), and forwards every request to --upstream with the path and query it came
// with, streaming the answer back as it arrives. A caller names itself by a path prefix,
// `/tag/<label>`, which the proxy strips before forwarding: a base URL of
// `http://127.0.0.1:<port>/tag/daemon` reaches `<upstream>/v1/messages` as `daemon`. A path without
// the prefix is recorded under the label `-`.
//
// Each exchange, once its answer has ended, is one JSON line appended to --record: its sequence
// number, label, method, path, query, the request's and response's headers, both bodies (a JSON
// body parsed, any other kept as text, a streamed answer as the text it streamed), status, how it
// was answered (`forwarded` or `stand-in`) and its duration. The credential headers (x-api-key,
// authorization, proxy-authorization, cookie, set-cookie) are recorded as `<redacted>`: the key
// reaches the upstream and never the record.
//
// Two controls, each a file named for a label, let a proof time a step against a model call:
//   --hold-dir   while <dir>/<label> exists, a request of that label whose ordinal (1 for the
//                label's first request of the proxy's life, 2 for its second, …) is above the
//                number the file holds (empty: 0) waits before it is forwarded, and its ordinal is
//                appended to <dir>/<label>.held as it starts waiting. Writing a larger number lets
//                that many through; removing the file lets every one through. A proof stops the
//                loop between two model calls this way (an operator resume, or a daemon killed,
//                after a tool call and before the reply);
//   --stand-in-dir  when <dir>/<label>.json exists, a request of that label is answered with that
//                file's Messages response instead of being forwarded, as a server-sent stream when
//                the request asks for one: the one model step a proof cannot get from a real model
//                (a reply not in the loop's form).
import { appendFileSync, existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

export interface ProxyOptions {
  port: number;
  upstream: string;
  record: string;
  holdDir?: string;
  standInDir?: string;
  /** Called with each entry once it is appended to the record. */
  onRecord?: (entry: Record<string, unknown>) => void;
  /** Called when a request starts waiting on its hold file. */
  onHold?: (label: string) => void;
}

const redactedHeaders: Record<string, true> = {
  "x-api-key": true,
  authorization: true,
  "proxy-authorization": true,
  cookie: true,
  "set-cookie": true,
};
/** Request headers a forward drops: the hop's own, and the encoding, so the answer arrives as text
 * the record can keep. */
const droppedRequestHeaders: Record<string, true> = {
  host: true,
  connection: true,
  "content-length": true,
  "accept-encoding": true,
  "transfer-encoding": true,
};
/** Response headers the client is not handed: fetch already decoded the body and set its length. */
const droppedResponseHeaders: Record<string, true> = {
  "content-encoding": true,
  "content-length": true,
  "transfer-encoding": true,
};
const labelPattern = /^\/tag\/([A-Za-z0-9._-]+)(\/.*)?$/;

/** The label a request names by its `/tag/<label>` prefix, and the path left once it is cut. */
export function splitLabel(pathname: string): { label: string; path: string } {
  const match = labelPattern.exec(pathname);
  if (!match) return { label: "-", path: pathname };
  return { label: match[1] ?? "-", path: match[2] ?? "/" };
}

/** The headers as the record keeps them: lowercased names, each credential `<redacted>`. */
export function recordedHeaders(headers: Headers): Record<string, string> {
  const out: Record<string, string> = {};
  headers.forEach((value, name) => {
    out[name] = redactedHeaders[name.toLowerCase()] ? "<redacted>" : value;
  });
  return out;
}

/** A body as the record keeps it: parsed when it is JSON, else the text itself. */
export function recordedBody(text: string): unknown {
  if (text === "") return null;
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

/** A stand-in answer: a Messages response the proof writes itself. */
export interface StandInMessage {
  content?: { type?: string; text?: string; input?: unknown; [key: string]: unknown }[];
  usage?: { output_tokens?: number };
  stop_reason?: string;
  [key: string]: unknown;
}

/** A Messages response as the server-sent events Anthropic streams it in, for a stand-in answer to a
 * request that asked for a stream: each content block's text or JSON whole in one delta. */
export function messageAsEvents(message: StandInMessage): string {
  const content = message.content ?? [];
  const usage = message.usage ?? {};
  const events: [string, unknown][] = [
    ["message_start", { type: "message_start", message: { ...message, content: [] } }],
  ];
  content.forEach((block, index) => {
    if (block.type === "text") {
      events.push([
        "content_block_start",
        { type: "content_block_start", index, content_block: { type: "text", text: "" } },
      ]);
      events.push([
        "content_block_delta",
        {
          type: "content_block_delta",
          index,
          delta: { type: "text_delta", text: block.text ?? "" },
        },
      ]);
    } else if (block.type === "tool_use") {
      events.push([
        "content_block_start",
        { type: "content_block_start", index, content_block: { ...block, input: {} } },
      ]);
      events.push([
        "content_block_delta",
        {
          type: "content_block_delta",
          index,
          delta: { type: "input_json_delta", partial_json: JSON.stringify(block.input ?? {}) },
        },
      ]);
    } else {
      events.push([
        "content_block_start",
        { type: "content_block_start", index, content_block: block },
      ]);
    }
    events.push(["content_block_stop", { type: "content_block_stop", index }]);
  });
  events.push([
    "message_delta",
    {
      type: "message_delta",
      delta: { stop_reason: message.stop_reason ?? "end_turn", stop_sequence: null },
      usage: { output_tokens: usage.output_tokens ?? 0 },
    },
  ]);
  events.push(["message_stop", { type: "message_stop" }]);
  return events
    .map(([event, data]) => `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`)
    .join("");
}

/** Holds a request of `label` while its hold file exists and names an ordinal below `ordinal`. */
async function waitWhileHeld(
  options: ProxyOptions,
  label: string,
  ordinal: number
): Promise<boolean> {
  if (!options.holdDir) return false;
  const file = join(options.holdDir, label);
  // A file removed between two reads lets the request through: the read throws, and that is release.
  const holding = () => {
    try {
      return ordinal > Number(readFileSync(file, "utf8").trim() || "0");
    } catch {
      return false;
    }
  };
  if (!holding()) return false;
  appendFileSync(`${file}.held`, `${ordinal}\n`);
  options.onHold?.(label);
  while (holding()) await Bun.sleep(200);
  return true;
}

export function startProxy(options: ProxyOptions) {
  let seq = 0;
  const ordinals: Record<string, number> = {};
  const upstream = options.upstream.replace(/\/+$/, "");
  const write = (entry: Record<string, unknown>) => {
    appendFileSync(options.record, `${JSON.stringify(entry)}\n`);
    options.onRecord?.(entry);
  };
  return Bun.serve({
    hostname: "127.0.0.1",
    port: options.port,
    // A model call may run for minutes before its first byte; the proxy never times one out.
    idleTimeout: 0,
    async fetch(request) {
      const started = Date.now();
      const url = new URL(request.url);
      const { label, path } = splitLabel(url.pathname);
      const number = ++seq;
      const ordinal = (ordinals[label] ?? 0) + 1;
      ordinals[label] = ordinal;
      const requestText = await request.text();
      const base = {
        seq: number,
        at: new Date(started).toISOString(),
        label,
        ordinal,
        method: request.method,
        path,
        query: url.search,
        request_headers: recordedHeaders(request.headers),
        request_body: recordedBody(requestText),
      };
      const held = await waitWhileHeld(options, label, ordinal);
      const standIn = options.standInDir ? join(options.standInDir, `${label}.json`) : undefined;
      if (standIn && existsSync(standIn)) {
        // The stand-in file is the proof's own, written in StandInMessage's shape.
        const message: StandInMessage = JSON.parse(readFileSync(standIn, "utf8"));
        const asked = recordedBody(requestText);
        const streamed =
          asked !== null && typeof asked === "object" && "stream" in asked && asked.stream === true;
        const text = streamed ? messageAsEvents(message) : JSON.stringify(message);
        const contentType = streamed ? "text/event-stream" : "application/json";
        write({
          ...base,
          held,
          mode: "stand-in",
          status: 200,
          response_headers: { "content-type": contentType },
          response_body: streamed ? text : message,
          duration_ms: Date.now() - started,
        });
        return new Response(text, { status: 200, headers: { "content-type": contentType } });
      }
      const headers = new Headers();
      request.headers.forEach((value, name) => {
        if (!droppedRequestHeaders[name.toLowerCase()]) headers.set(name, value);
      });
      headers.set("accept-encoding", "identity");
      let response: Response;
      try {
        response = await fetch(`${upstream}${path}${url.search}`, {
          method: request.method,
          headers,
          body: requestText === "" ? undefined : requestText,
          redirect: "manual",
        });
      } catch (error) {
        const reason = error instanceof Error ? error.message : String(error);
        write({
          ...base,
          held,
          mode: "forwarded",
          status: 502,
          error: reason,
          duration_ms: Date.now() - started,
        });
        return new Response(
          JSON.stringify({ error: `recording-proxy: upstream unreachable: ${reason}` }),
          {
            status: 502,
            headers: { "content-type": "application/json" },
          }
        );
      }
      const responseHeaders = new Headers();
      response.headers.forEach((value, name) => {
        if (!droppedResponseHeaders[name.toLowerCase()]) responseHeaders.set(name, value);
      });
      const finish = (text: string, error?: string) =>
        write({
          ...base,
          held,
          mode: "forwarded",
          status: response.status,
          response_headers: recordedHeaders(response.headers),
          response_body: recordedBody(text),
          ...(error ? { error } : {}),
          duration_ms: Date.now() - started,
        });
      if (!response.body) {
        finish("");
        return new Response(null, { status: response.status, headers: responseHeaders });
      }
      const [toClient, toRecord] = response.body.tee();
      (async () => {
        const decoder = new TextDecoder();
        let text = "";
        try {
          for await (const chunk of toRecord) text += decoder.decode(chunk, { stream: true });
          finish(text + decoder.decode());
        } catch (error) {
          finish(text, error instanceof Error ? error.message : String(error));
        }
      })();
      return new Response(toClient, { status: response.status, headers: responseHeaders });
    },
  });
}

function parseArgs(argv: string[]): ProxyOptions | string {
  const values: Record<string, string> = {};
  for (let i = 0; i < argv.length; i += 2) {
    const name = argv[i];
    const value = argv[i + 1];
    if (!name?.startsWith("--") || value === undefined) return `bad argument ${name ?? ""}`;
    values[name.slice(2)] = value;
  }
  const known: Record<string, true> = {
    port: true,
    upstream: true,
    record: true,
    "hold-dir": true,
    "stand-in-dir": true,
  };
  for (const name of Object.keys(values)) if (!known[name]) return `unknown argument --${name}`;
  const port = Number(values.port);
  if (!Number.isInteger(port) || port < 0 || port > 65535) return "--port must be a port number";
  if (!values.upstream?.match(/^https?:\/\//)) return "--upstream must be an http or https URL";
  if (!values.record) return "--record is required";
  return {
    port,
    upstream: values.upstream,
    record: values.record,
    holdDir: values["hold-dir"],
    standInDir: values["stand-in-dir"],
  };
}

if (import.meta.main) {
  const options = parseArgs(process.argv.slice(2));
  if (typeof options === "string") {
    console.error(`recording-proxy: ${options}`);
    console.error(
      "usage: bun scripts/e2e/lib/recording-proxy.ts --port <port> --upstream <url> --record <file.jsonl> [--hold-dir <dir>] [--stand-in-dir <dir>]"
    );
    process.exit(2);
  }
  const server = startProxy(options);
  console.log(`recording-proxy listening on 127.0.0.1:${server.port}`);
}
