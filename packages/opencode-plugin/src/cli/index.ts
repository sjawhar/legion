#!/usr/bin/env bun
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { defineCommand, runMain } from "citty";

const CONFIG_DIR = path.join(os.homedir(), ".config", "opencode");
const CONFIG_FILE = path.join(CONFIG_DIR, "opencode-legion.json");
const PLUGIN_NAME = "opencode-legion";

interface LegionConfig {
  agents?: Record<string, { model: string }>;
  categories: Record<string, { model: string }>;
}

const DEFAULT_CONFIG: LegionConfig = {
  agents: {},
  categories: {
    "visual-engineering": { model: "google/gemini-3-pro" },
    ultrabrain: { model: "anthropic/claude-opus-4-6" },
    deep: { model: "openai/gpt-5.2-codex" },
    artistry: { model: "anthropic/claude-opus-4-6" },
    quick: { model: "anthropic/claude-sonnet-4-20250514" },
    "unspecified-low": { model: "anthropic/claude-sonnet-4-20250514" },
    "unspecified-high": { model: "anthropic/claude-opus-4-6" },
    writing: { model: "anthropic/claude-sonnet-4-20250514" },
  },
};

function findOpencodeConfig(): string | null {
  const cwd = process.cwd();
  const candidates = ["opencode.json", "opencode.jsonc"];

  for (const candidate of candidates) {
    const fullPath = path.join(cwd, candidate);
    if (fs.existsSync(fullPath)) {
      return fullPath;
    }
  }

  return null;
}

function readJsonFile(filePath: string): unknown {
  const content = fs.readFileSync(filePath, "utf-8");
  return JSON.parse(content);
}

function writeJsonFile(filePath: string, data: unknown): void {
  fs.writeFileSync(filePath, JSON.stringify(data, null, 2) + "\n", "utf-8");
}

async function cmdInit(dryRun: boolean): Promise<void> {
  console.log("Initializing opencode-legion plugin...\n");

  // 1. Find opencode.json(c)
  const opencodeConfigPath = findOpencodeConfig();
  if (!opencodeConfigPath) {
    console.error("Error: No opencode.json or opencode.jsonc found in current directory.");
    console.error("Run this command from your OpenCode project root.");
    process.exit(1);
  }

  console.log(`Found OpenCode config: ${opencodeConfigPath}`);

  // 2. Read and update opencode config
  const opencodeConfig = readJsonFile(opencodeConfigPath) as {
    plugins?: string[];
    [key: string]: unknown;
  };

  if (!opencodeConfig.plugins) {
    opencodeConfig.plugins = [];
  }

  const hasPlugin = opencodeConfig.plugins.includes(PLUGIN_NAME);
  const hasOhMyOpencode = opencodeConfig.plugins.includes("oh-my-opencode");

  if (dryRun) {
    console.log("\n[DRY RUN] Would perform the following actions:");
    console.log(`- Create config: ${CONFIG_FILE}`);
    if (!hasPlugin) {
      console.log(`- Add "${PLUGIN_NAME}" to plugins in ${opencodeConfigPath}`);
    } else {
      console.log(`- Plugin "${PLUGIN_NAME}" already in ${opencodeConfigPath}`);
    }
    if (hasOhMyOpencode) {
      console.log(`- Remove "oh-my-opencode" from plugins (optional)`);
    }
    return;
  }

  // 3. Create config directory and file
  fs.mkdirSync(CONFIG_DIR, { recursive: true });
  writeJsonFile(CONFIG_FILE, DEFAULT_CONFIG);
  console.log(`✓ Created config: ${CONFIG_FILE}`);

  // 4. Update opencode.json
  if (!hasPlugin) {
    opencodeConfig.plugins.push(PLUGIN_NAME);
    writeJsonFile(opencodeConfigPath, opencodeConfig);
    console.log(`✓ Added "${PLUGIN_NAME}" to plugins`);
  } else {
    console.log(`✓ Plugin "${PLUGIN_NAME}" already configured`);
  }

  // 5. Optionally remove oh-my-opencode
  if (hasOhMyOpencode) {
    console.log(
      '\nNote: "oh-my-opencode" is still in your plugins list. Remove it manually if desired.'
    );
  }

  console.log("\n✓ Installation complete!");
  console.log("\nNext steps:");
  console.log(`  1. Edit ${CONFIG_FILE} to customize model assignments`);
  console.log("  2. Run 'opencode-legion status' to verify configuration");
}

async function cmdStatus(): Promise<void> {
  console.log("OpenCode Legion Configuration");
  console.log("=".repeat(40));

  if (!fs.existsSync(CONFIG_FILE)) {
    console.log("\nNo configuration found.");
    console.log("Run 'opencode-legion init' to create default config.");
    return;
  }

  const config = readJsonFile(CONFIG_FILE) as LegionConfig;

  console.log(`\nConfig file: ${CONFIG_FILE}`);

  if (config.agents && Object.keys(config.agents).length > 0) {
    console.log("\nAgent Overrides:");
    for (const [agent, settings] of Object.entries(config.agents)) {
      console.log(`  ${agent}: ${settings.model}`);
    }
  } else {
    console.log("\nAgent Overrides: (none)");
  }

  console.log("\nCategory Models:");
  for (const [category, settings] of Object.entries(config.categories)) {
    console.log(`  ${category}: ${settings.model}`);
  }
}

const initCommand = defineCommand({
  meta: { name: "init", description: "Initialize opencode-legion plugin" },
  args: {
    "dry-run": {
      type: "boolean",
      description: "Show what would be created",
      default: false,
    },
  },
  async run({ args }) {
    await cmdInit(args["dry-run"]);
  },
});

const statusCommand = defineCommand({
  meta: { name: "status", description: "Show current configuration" },
  async run() {
    await cmdStatus();
  },
});

const mainCommand = defineCommand({
  meta: {
    name: "opencode-legion",
    version: "0.1.0",
    description: "OpenCode Legion plugin CLI",
  },
  subCommands: { init: initCommand, status: statusCommand },
});

if (import.meta.main) {
  runMain(mainCommand);
}
