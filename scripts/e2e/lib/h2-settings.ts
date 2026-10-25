// Whether a Dispatch front proxy advertises HTTP/2 extended CONNECT, which the live socket's design
// rests on it not doing (dispatch://INFRA-258).
//
//   bun scripts/e2e/lib/h2-settings.ts <https-url>
//
// It offers `h2` and `http/1.1` over TLS, as a browser does, and prints one JSON line: the URL, the
// protocol the server chose (`alpn`), and, when that is `h2`, the SETTINGS the server sent as Node's
// HTTP/2 client reads them (`settings`). A server advertising SETTINGS_ENABLE_CONNECT_PROTOCOL
// lets a browser open the live socket as a stream of a shared HTTP/2 connection rather than a
// connection of its own (RFC 8441), and the never-ending request INFRA-258 fixed could come back:
// exit 1. One that does not, or that speaks no HTTP/2 at all: exit 0. A usage error, or a server
// the probe could not read the answer from: exit 2.
//
// Certificates are verified, so a proxy behind a private CA needs `NODE_EXTRA_CA_CERTS`.
import http2 from "node:http2";
import { isIP } from "node:net";
import tls from "node:tls";

const USAGE = "usage: bun scripts/e2e/lib/h2-settings.ts <https-url>";
/** How long the TLS handshake and the server's first SETTINGS may take together. */
const DEADLINE_MS = 10_000;

/** What the probe prints. */
interface Answer {
  readonly url: string;
  /** The protocol the server chose from the two offered, false when it chose none. */
  readonly alpn: string | false;
  /** The server's SETTINGS, null when it chose no HTTP/2. */
  readonly settings: http2.Settings | null;
}

function fail(message: string, code: number): never {
  process.stderr.write(`h2-settings: ${message}\n`);
  process.exit(code);
}

/** The https URL the one argument names, or a usage error. */
function target(argv: readonly string[]): URL {
  const [raw, ...extra] = argv;
  if (raw === undefined || extra.length > 0) fail(USAGE, 2);
  let parsed: URL;
  try {
    parsed = new URL(raw);
  } catch {
    fail(`not a URL: ${raw}\n${USAGE}`, 2);
  }
  if (parsed.protocol !== "https:") fail(`not an https URL: ${raw}\n${USAGE}`, 2);
  return parsed;
}

const url = target(process.argv.slice(2));
const host = url.hostname.replace(/^\[(.*)\]$/u, "$1");

const deadline = setTimeout(
  () => fail(`${url.origin} sent no SETTINGS within ${DEADLINE_MS / 1_000}s`, 2),
  DEADLINE_MS
);
const answer = Promise.withResolvers<Answer>();
const socket = tls.connect({
  ALPNProtocols: ["h2", "http/1.1"],
  host,
  port: url.port === "" ? 443 : Number(url.port),
  // SNI takes a host name, never an address.
  ...(isIP(host) === 0 ? { servername: host } : {}),
});
socket.once("error", (error) => fail(`${url.origin}: ${error.message}`, 2));
socket.once("secureConnect", () => {
  const alpn = socket.alpnProtocol ?? false;
  if (alpn !== "h2") {
    socket.destroy();
    answer.resolve({ alpn, settings: null, url: url.href });
    return;
  }
  // The session takes over the socket the handshake chose h2 on.
  const session = http2.connect(url, { createConnection: () => socket });
  session.once("error", (error) => fail(`${url.origin}: ${error.message}`, 2));
  session.once("remoteSettings", (settings) => {
    session.close();
    answer.resolve({ alpn, settings, url: url.href });
  });
});

const read = await answer.promise;
clearTimeout(deadline);
process.stdout.write(`${JSON.stringify(read)}\n`);
if (read.settings?.enableConnectProtocol === true) {
  fail(
    `${url.origin} advertises SETTINGS_ENABLE_CONNECT_PROTOCOL: a browser would run Dispatch's live socket over HTTP/2, where the INFRA-258 hang can return`,
    1
  );
}
process.exit(0);
