import { afterAll, afterEach, beforeAll, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import http2 from "node:http2";
import https from "node:https";
import net, { type AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import tls from "node:tls";

// The probe runs as an operator runs it, a process of its own, against a local TLS server that
// does or does not advertise SETTINGS_ENABLE_CONNECT_PROTOCOL. The certificate is made per run and
// handed to the probe through NODE_EXTRA_CA_CERTS, so the probe verifies it as it would a proxy's.

const probe = join(import.meta.dir, "h2-settings.ts");
const scratch = mkdtempSync(join(tmpdir(), "h2-settings-"));
const certFile = join(scratch, "cert.pem");
const keyFile = join(scratch, "key.pem");
const closers: (() => void)[] = [];
let tlsFiles: { readonly cert: Buffer; readonly key: Buffer };

beforeAll(() => {
  const made = Bun.spawnSync([
    "openssl",
    "req",
    "-x509",
    "-newkey",
    "ec",
    "-pkeyopt",
    "ec_paramgen_curve:prime256v1",
    "-nodes",
    "-keyout",
    keyFile,
    "-out",
    certFile,
    "-days",
    "1",
    "-subj",
    "/CN=localhost",
    "-addext",
    "subjectAltName=DNS:localhost,IP:127.0.0.1",
  ]);
  if (made.exitCode !== 0) throw new Error(`openssl: ${made.stderr.toString()}`);
  tlsFiles = { cert: readFileSync(certFile), key: readFileSync(keyFile) };
});

afterEach(() => {
  for (const close of closers.splice(0)) close();
});

afterAll(() => rmSync(scratch, { force: true, recursive: true }));

/** Listens `server` on a loopback port and returns it; the row's end closes it. */
async function listen(server: net.Server): Promise<number> {
  const listening = Promise.withResolvers<void>();
  server.listen(0, "127.0.0.1", () => listening.resolve());
  await listening.promise;
  closers.push(() => server.close());
  return (server.address() as AddressInfo).port;
}

async function run(
  args: readonly string[],
  env: Record<string, string> = { NODE_EXTRA_CA_CERTS: certFile }
): Promise<{ readonly code: number; readonly stdout: string; readonly stderr: string }> {
  const child = Bun.spawn([process.execPath, probe, ...args], {
    env: { ...process.env, ...env },
    stderr: "pipe",
    stdout: "pipe",
  });
  const [code, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  return { code, stderr, stdout };
}

test("a server advertising ENABLE_CONNECT_PROTOCOL prints its SETTINGS and exits 1", async () => {
  const port = await listen(
    http2.createSecureServer({ ...tlsFiles, settings: { enableConnectProtocol: true } })
  );
  const result = await run([`https://localhost:${port}/`]);
  expect(result.code).toBe(1);
  const answer = JSON.parse(result.stdout);
  expect(answer).toMatchObject({ alpn: "h2", url: `https://localhost:${port}/` });
  expect(answer.settings.enableConnectProtocol).toBe(true);
  expect(answer.settings.initialWindowSize).toEqual(expect.any(Number));
  expect(result.stderr).toBe(
    `h2-settings: https://localhost:${port} advertises SETTINGS_ENABLE_CONNECT_PROTOCOL: a browser would run Dispatch's live socket over HTTP/2, where the INFRA-258 hang can return\n`
  );
});

test("a server that does not advertise it prints its SETTINGS and exits 0", async () => {
  const port = await listen(http2.createSecureServer({ ...tlsFiles, settings: {} }));
  // By address, so the probe sends no SNI and the certificate's IP name is the one checked.
  const result = await run([`https://127.0.0.1:${port}`]);
  expect(result.code).toBe(0);
  expect(result.stderr).toBe("");
  const answer = JSON.parse(result.stdout);
  expect(answer).toMatchObject({ alpn: "h2", url: `https://127.0.0.1:${port}/` });
  expect(answer.settings.enableConnectProtocol).toBe(false);
});

test("a server that speaks no HTTP/2 exits 0 with no SETTINGS", async () => {
  const port = await listen(https.createServer(tlsFiles, (_request, response) => response.end()));
  const result = await run([`https://localhost:${port}`]);
  expect(result.code).toBe(0);
  expect(result.stderr).toBe("");
  expect(JSON.parse(result.stdout)).toEqual({
    alpn: false,
    settings: null,
    url: `https://localhost:${port}/`,
  });
});

test("a certificate the probe cannot verify is no answer, exit 2", async () => {
  const port = await listen(
    http2.createSecureServer({ ...tlsFiles, settings: { enableConnectProtocol: true } })
  );
  const result = await run([`https://localhost:${port}`], {});
  expect(result.code).toBe(2);
  expect(result.stdout).toBe("");
  expect(result.stderr).toStartWith(`h2-settings: https://localhost:${port}: `);
});

// The handshake and the server's first SETTINGS share one 10 s deadline: a server that chooses h2
// and then sends nothing, and one that accepts the connection and never answers the handshake, are
// no answer. Both run at once, so the row waits the deadline once.
test("a server that sends no SETTINGS within 10 s is no answer, exit 2", async () => {
  const chosen: (string | false)[] = [];
  const silentH2 = tls.createServer({ ...tlsFiles, ALPNProtocols: ["h2"] });
  // Holds the connection and writes nothing on it: no SETTINGS, no close.
  silentH2.on("secureConnection", (socket) => chosen.push(socket.alpnProtocol ?? false));
  const silentTcp = net.createServer();
  const ports = [await listen(silentH2), await listen(silentTcp)];

  const runs = await Promise.all(
    ports.map(async (port) => {
      const started = Date.now();
      const result = await run([`https://localhost:${port}`]);
      return { ...result, ms: Date.now() - started, port };
    })
  );
  // The probe offered h2 and the silent server chose it, so its wait was for the SETTINGS alone.
  expect(chosen).toEqual(["h2"]);
  for (const result of runs) {
    expect(result.code).toBe(2);
    expect(result.stdout).toBe("");
    expect(result.stderr).toBe(
      `h2-settings: https://localhost:${result.port} sent no SETTINGS within 10s\n`
    );
    expect(result.ms).toBeGreaterThanOrEqual(10_000);
    expect(result.ms).toBeLessThan(15_000);
  }
}, 20_000);

test("anything but one https URL is a usage error, exit 2", async () => {
  for (const args of [[], ["http://localhost:1"], ["https://a.example", "https://b.example"]]) {
    const result = await run(args);
    expect(result.code).toBe(2);
    expect(result.stdout).toBe("");
    expect(result.stderr).toContain("usage: bun scripts/e2e/lib/h2-settings.ts <https-url>");
  }
});
