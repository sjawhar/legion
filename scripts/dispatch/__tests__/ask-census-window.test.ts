import { afterEach, describe, expect, test } from "bun:test";
import { existsSync } from "node:fs";
import { mkdir, mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { approvalAsk, edited, handedBack, opened, recordedAt } from "./approval-events.ts";

const script = join(import.meta.dir, "..", "ask-census.ts");
const servers: Array<{ stop: (force?: boolean) => void }> = [];

afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true);
});

describe("ask census since #1671", () => {
  test("counts in-window hand-backs of an approval request opened before the window", async () => {
    // Since #1671 one approval row follows the document for its whole life, so a request opened
    // before the window and handed back twice inside it, with no human turn, is the loop the
    // census exists to flag, even though no ask was opened in the window.
    const events = [
      opened(1, 1),
      edited(2, 2, 1, { version: 1 }),
      handedBack(3, 2),
      edited(4, 3, 2, { version: 2 }),
      edited(5, 3, 2, { version: 3 }, "Proposes an hourly export."),
      handedBack(6, 3, "Proposes an hourly export."),
    ];
    const server = Bun.serve({
      hostname: "127.0.0.1",
      port: 0,
      fetch(request) {
        const url = new URL(request.url);
        if (url.pathname === "/api/v1/issues") return Response.json([{ key: "TEST-1" }]);
        if (url.pathname === "/api/v1/issues/TEST-1/asks") {
          return Response.json([approvalAsk(3, 3, "Proposes an hourly export.")]);
        }
        if (url.pathname === "/api/v1/issues/TEST-1/events") {
          return Response.json(url.searchParams.get("after") === "0" ? events : []);
        }
        return new Response("not found", { status: 404 });
      },
    });
    servers.push(server);

    // DISPATCH_TOKEN_FILE outranks DISPATCH_TOKEN, so a pane's own token file would reach the stub.
    const { DISPATCH_TOKEN_FILE: _paneTokenFile, ...environment } = process.env;
    const census = Bun.spawn(
      ["bun", script, "--from", recordedAt(2), "--to", recordedAt(7), "--project", "TEST"],
      {
        env: {
          ...environment,
          DISPATCH_URL: `http://127.0.0.1:${server.port}`,
          DISPATCH_TOKEN: "token",
        },
        stdout: "pipe",
        stderr: "pipe",
      }
    );
    const [stdout, stderr, exitCode] = await Promise.all([
      new Response(census.stdout).text(),
      new Response(census.stderr).text(),
      census.exited,
    ]);

    expect(stderr).toBe("");
    expect(exitCode).toBe(0);
    const flagged = stdout.slice(stdout.indexOf("Approval rounds above the human-turn budget"));
    // No ask opened in the window, two arrivals (the hand-backs), no human turn.
    expect(flagged).toMatch(/│ TEST-1\s*│ artifact-1\s*│ 0\s*│ 2\s*│ 0\s*│ true\s*│/);
  });
  test("refuses while only the OpenCode-era user file is there, naming the move and sending nothing", async () => {
    let requests = 0;
    const server = Bun.serve({
      hostname: "127.0.0.1",
      port: 0,
      fetch() {
        requests += 1;
        return Response.json([]);
      },
    });
    servers.push(server);

    const home = await mkdtemp(join(tmpdir(), "ask-census-"));
    const marker = join(home, "old-token-ran");
    const oldConfig = join(home, ".config", "opencode", "envoy.json");
    await mkdir(join(home, ".config", "opencode"), { recursive: true });
    await writeFile(
      oldConfig,
      JSON.stringify({
        dispatch: {
          enabled: true,
          serverUrl: `http://127.0.0.1:${server.port}`,
          token: `!touch ${marker}; echo token`,
        },
      })
    );
    const {
      CLAUDE_CONFIG_DIR: _claudeConfigDir,
      DISPATCH_TOKEN: _dispatchToken,
      DISPATCH_TOKEN_FILE: _dispatchTokenFile,
      DISPATCH_URL: _dispatchUrl,
      ENVOY_CONFIG_FILE: _envoyConfigFile,
      OMP_PROFILE: _ompProfile,
      OPENCODE_CONFIG_DIR: _openCodeConfigDir,
      PI_CODING_AGENT_DIR: _piCodingAgentDir,
      XDG_CONFIG_HOME: _xdgConfigHome,
      ...environment
    } = process.env;
    const census = Bun.spawn(
      ["bun", script, "--from", recordedAt(2), "--to", recordedAt(7), "--project", "TEST"],
      {
        cwd: home,
        env: { ...environment, HOME: home },
        stdout: "pipe",
        stderr: "pipe",
      }
    );
    const [stderr, exitCode] = await Promise.all([
      new Response(census.stderr).text(),
      census.exited,
    ]);

    expect(stderr).toContain(
      `Dispatch is not configured: no envoy.json at ${join(home, ".config", "legion", "envoy.json")}: Dispatch is off; ${oldConfig} is not read: mkdir -p ~/.config/legion && mv ~/.config/opencode/envoy.json ~/.config/legion/`
    );
    expect(exitCode).toBe(1);
    expect(existsSync(marker)).toBe(false);
    expect(requests).toBe(0);
  });
  test("names a checkout's unread .opencode/envoy.json on stderr and uses the user file's server", async () => {
    let requests = 0;
    const server = Bun.serve({
      hostname: "127.0.0.1",
      port: 0,
      fetch(request) {
        requests += 1;
        if (new URL(request.url).pathname === "/api/v1/issues") return Response.json([]);
        return new Response("not found", { status: 404 });
      },
    });
    servers.push(server);

    const home = await mkdtemp(join(tmpdir(), "ask-census-"));
    const checkout = await mkdtemp(join(tmpdir(), "ask-census-checkout-"));
    await mkdir(join(home, ".config", "legion"), { recursive: true });
    await writeFile(
      join(home, ".config", "legion", "envoy.json"),
      JSON.stringify({
        dispatch: { enabled: true, serverUrl: `http://127.0.0.1:${server.port}`, token: "token" },
      })
    );
    const old = join(checkout, ".opencode", "envoy.json");
    await mkdir(join(checkout, ".opencode"));
    await writeFile(
      old,
      JSON.stringify({
        dispatch: { enabled: true, serverUrl: "http://127.0.0.1:1", token: "checkout-token" },
      })
    );
    const {
      CLAUDE_CONFIG_DIR: _claudeConfigDir,
      DISPATCH_TOKEN: _dispatchToken,
      DISPATCH_TOKEN_FILE: _dispatchTokenFile,
      DISPATCH_URL: _dispatchUrl,
      ENVOY_CONFIG_FILE: _envoyConfigFile,
      OMP_PROFILE: _ompProfile,
      OPENCODE_CONFIG_DIR: _openCodeConfigDir,
      PI_CODING_AGENT_DIR: _piCodingAgentDir,
      XDG_CONFIG_HOME: _xdgConfigHome,
      ...environment
    } = process.env;
    const census = Bun.spawn(
      ["bun", script, "--from", recordedAt(2), "--to", recordedAt(7), "--project", "TEST"],
      {
        cwd: checkout,
        env: { ...environment, HOME: home },
        stdout: "pipe",
        stderr: "pipe",
      }
    );
    const [stderr, exitCode] = await Promise.all([
      new Response(census.stderr).text(),
      census.exited,
    ]);

    expect(stderr).toBe(
      `dispatch: ${old} is not read: mkdir -p ${join(checkout, ".envoy")} && mv ${old} ${join(checkout, ".envoy", "envoy.json")}\n`
    );
    expect(exitCode).toBe(0);
    expect(requests).toBeGreaterThan(0);
  });
});
