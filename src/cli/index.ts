#!/usr/bin/env bun
import { resolveTeamId } from "./team-resolver";
import { startDaemon } from "../daemon/index";
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

export type Command = "start" | "stop" | "status" | "attach" | "teams";

interface ParsedArgs {
  command: Command;
  args: {
    team?: string;
    workspace?: string;
    stateDir?: string;
    issue?: string;
  };
}

/**
 * Parse command-line arguments.
 */
export function parseArgs(argv: string[]): ParsedArgs {
  if (argv.length === 0) {
    throw new Error("No command provided. Usage: legion <command> [args]");
  }

  const command = argv[0];
  const validCommands: Command[] = ["start", "stop", "status", "attach", "teams"];

  if (!validCommands.includes(command as Command)) {
    throw new Error(`Unknown command: ${command}`);
  }

  const args: ParsedArgs["args"] = {};
  let i = 1;

  // Parse positional arguments based on command
  if (command === "start" || command === "stop" || command === "status") {
    if (i >= argv.length) {
      throw new Error(`${command} requires a team argument`);
    }
    args.team = argv[i++];
  } else if (command === "attach") {
    if (i >= argv.length) {
      throw new Error(`${command} requires an issue argument`);
    }
    args.issue = argv[i++];
  }

  // Parse options
  while (i < argv.length) {
    const arg = argv[i];

    if (arg === "--workspace" || arg === "-w") {
      if (i + 1 >= argv.length) {
        throw new Error(`Option ${arg} requires a value`);
      }
      args.workspace = argv[++i];
      i++;
    } else if (arg === "--state-dir") {
      if (i + 1 >= argv.length) {
        throw new Error(`Option ${arg} requires a value`);
      }
      args.stateDir = argv[++i];
      i++;
    } else {
      throw new Error(`Unknown option: ${arg}`);
    }
  }

  // Set defaults
  if (command === "start" && !args.workspace) {
    args.workspace = process.cwd();
  }

  return { command: command as Command, args };
}

/**
 * Start the Legion swarm.
 */
async function cmdStart(team: string, workspace: string, stateDir?: string): Promise<void> {
  const teamId = await resolveTeamId(team);

  const resolvedStateDir = stateDir ?? path.join(os.homedir(), ".legion", teamId);

  console.log(`Starting Legion for team: ${teamId}`);
  console.log(`Workspace: ${workspace}`);
  console.log(`State directory: ${resolvedStateDir}`);

  // Create state directory
  fs.mkdirSync(resolvedStateDir, { recursive: true });

  // Start daemon
  const handle = await startDaemon({
    teamId,
    legionDir: workspace,
    stateFilePath: path.join(resolvedStateDir, "workers.json"),
  });

  console.log(`Daemon started on port ${handle.config.daemonPort}`);
  console.log(`\nTo check status: legion status ${team}`);
  console.log(`To stop: legion stop ${team}`);

  // Keep process alive
  await new Promise(() => {});
}

/**
 * Stop the Legion swarm.
 */
async function cmdStop(team: string, stateDir?: string): Promise<void> {
  const teamId = await resolveTeamId(team);
  const resolvedStateDir = stateDir ?? path.join(os.homedir(), ".legion", teamId);

  console.log(`Stopping Legion for team: ${teamId}`);

  // Read state file to find daemon port
  const stateFilePath = path.join(resolvedStateDir, "workers.json");
  if (!fs.existsSync(stateFilePath)) {
    console.log("No state file found. Daemon may not be running.");
    return;
  }

  // Try to connect to daemon and shut it down
  const daemonPort = 13370; // Default port
  try {
    const response = await fetch(`http://127.0.0.1:${daemonPort}/shutdown`, {
      method: "POST",
    });
    if (response.ok) {
      console.log("Daemon stopped successfully.");
    } else {
      console.log("Failed to stop daemon. It may not be running.");
    }
  } catch (error) {
    console.log("Could not connect to daemon. It may not be running.");
  }
}

/**
 * Show Legion swarm status.
 */
async function cmdStatus(team: string, stateDir?: string): Promise<void> {
  const teamId = await resolveTeamId(team);
  const resolvedStateDir = stateDir ?? path.join(os.homedir(), ".legion", teamId);

  console.log(`Legion Status: ${teamId}`);
  console.log("=".repeat(40));

  // Try to connect to daemon
  const daemonPort = 13370; // Default port
  try {
    const response = await fetch(`http://127.0.0.1:${daemonPort}/workers`);
    if (response.ok) {
      const workers = (await response.json()) as Array<{ id: string; port: number }>;
      console.log(`Daemon: RUNNING (port ${daemonPort})`);
      console.log(`Workers: ${workers.length}`);
      for (const worker of workers) {
        console.log(`  - ${worker.id} (port ${worker.port})`);
      }
    } else {
      console.log("Daemon: NOT RUNNING");
    }
  } catch (error) {
    console.log("Daemon: NOT RUNNING");
  }

  // Check state file
  const stateFilePath = path.join(resolvedStateDir, "workers.json");
  if (fs.existsSync(stateFilePath)) {
    const stat = fs.statSync(stateFilePath);
    const age = Math.floor((Date.now() - stat.mtimeMs) / 1000);
    console.log(`\nState file: ${stateFilePath}`);
    console.log(`Last updated: ${age}s ago`);
  } else {
    console.log(`\nState file: NOT FOUND`);
  }
}

/**
 * Attach to a worker session.
 */
async function cmdAttach(issue: string): Promise<void> {
  console.log(`Attaching to worker for issue: ${issue}`);

  // Query daemon for worker port
  const daemonPort = 13370; // Default port
  try {
    const response = await fetch(`http://127.0.0.1:${daemonPort}/workers`);
    if (!response.ok) {
      console.error("Could not connect to daemon. Is it running?");
      process.exit(1);
    }

    const workers = (await response.json()) as Array<{ id: string; port: number }>;
    const worker = workers.find((w) => w.id === issue.toLowerCase());

    if (!worker) {
      console.error(`No worker found for issue: ${issue}`);
      console.log(`\nAvailable workers:`);
      for (const w of workers) {
        console.log(`  - ${w.id}`);
      }
      process.exit(1);
    }

    console.log(`Found worker on port ${worker.port}`);
    console.log(`Attaching with: opencode attach http://localhost:${worker.port}`);

    // Spawn opencode attach
    const child = spawn("opencode", ["attach", `http://localhost:${worker.port}`], {
      stdio: "inherit",
    });

    child.on("exit", (code) => {
      process.exit(code ?? 0);
    });
  } catch (error) {
    console.error("Failed to attach:", error);
    process.exit(1);
  }
}

/**
 * List and cache Linear teams.
 */
async function cmdTeams(): Promise<void> {
  console.log("Fetching teams from Linear via OpenCode...");

  // Use opencode run to get teams via Linear MCP
  const child = spawn(
    "opencode",
    [
      "run",
      "--dangerously-skip-permissions",
      "--output-format",
      "json",
      "--max-turns",
      "3",
      "Use mcp__linear__list_teams to list all teams. " +
        "Then output ONLY a JSON object where each key is the team's key " +
        "(uppercase letters like LEG, ENG) and the value is {id, name}. " +
        "Find the team key from the response - it's typically uppercase letters. " +
        'Example format: {"LEG": {"id": "uuid-here", "name": "Legion"}}. ' +
        "No markdown, just raw JSON.",
    ],
    { stdio: ["inherit", "pipe", "inherit"] }
  );

  let stdout = "";
  child.stdout?.on("data", (data) => {
    stdout += data.toString();
  });

  await new Promise<void>((resolve, reject) => {
    child.on("exit", (code) => {
      if (code !== 0) {
        reject(new Error(`opencode run exited with code ${code}`));
      } else {
        resolve();
      }
    });
    child.on("error", reject);
  });

  // Parse OpenCode's response
  try {
    const response = JSON.parse(stdout);
    const content = response.result || "";

    // Find JSON object - might be nested or contain multiple objects
    const start = content.indexOf("{");
    if (start === -1) {
      throw new Error(`No JSON found in: ${content.slice(0, 200)}`);
    }

    // Find matching closing brace
    let depth = 0;
    let end = start;
    for (let i = start; i < content.length; i++) {
      const char = content[i];
      if (char === "{") {
        depth++;
      } else if (char === "}") {
        depth--;
        if (depth === 0) {
          end = i + 1;
          break;
        }
      }
    }

    const teamsJson = content.slice(start, end);
    const teams = JSON.parse(teamsJson);

    // Validate structure
    if (!teams || typeof teams !== "object") {
      throw new Error(`Invalid teams format: ${teamsJson.slice(0, 200)}`);
    }

    // Check if it's a single team (missing key wrapper)
    if ("id" in teams && "name" in teams && Object.keys(teams).length <= 3) {
      throw new Error(
        `OpenCode returned a single team without the key. ` +
          `Please manually create ~/.legion/teams.json with format:\n` +
          `{"TEAMKEY": ${teamsJson}}`
      );
    }

    // Save to cache
    const cacheDir = path.join(os.homedir(), ".legion");
    fs.mkdirSync(cacheDir, { recursive: true });
    const cacheFile = path.join(cacheDir, "teams.json");

    fs.writeFileSync(cacheFile, JSON.stringify(teams, null, 2));

    console.log(`\nCached ${Object.keys(teams).length} teams to ${cacheFile}:\n`);
    for (const [key, team] of Object.entries(teams)) {
      if (
        typeof team === "object" &&
        team !== null &&
        "name" in team &&
        "id" in team
      ) {
        console.log(`  ${key}: ${(team as any).name} (${(team as any).id})`);
      } else {
        console.log(`  ${key}: ${team}`);
      }
    }
  } catch (error) {
    throw new Error(
      `Failed to parse teams response: ${error instanceof Error ? error.message : String(error)}`
    );
  }
}

/**
 * Main CLI entry point.
 */
async function main(): Promise<void> {
  try {
    const parsed = parseArgs(process.argv.slice(2));

    switch (parsed.command) {
      case "start":
        await cmdStart(
          parsed.args.team!,
          parsed.args.workspace!,
          parsed.args.stateDir
        );
        break;
      case "stop":
        await cmdStop(parsed.args.team!, parsed.args.stateDir);
        break;
      case "status":
        await cmdStatus(parsed.args.team!, parsed.args.stateDir);
        break;
      case "attach":
        await cmdAttach(parsed.args.issue!);
        break;
      case "teams":
        await cmdTeams();
        break;
    }
  } catch (error) {
    console.error(`Error: ${error instanceof Error ? error.message : String(error)}`);
    process.exit(1);
  }
}

// Run if executed directly
if (import.meta.main) {
  void main();
}
