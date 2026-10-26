// @bun
// ../envoy-client/src/errors.ts
function messageFor(error) {
  return error instanceof Error ? error.message : String(error);
}
function hasErrnoCode(error, code) {
  return error instanceof Error && "code" in error && error.code === code;
}

// src/session-state.ts
import { readFileSync } from "fs";
import { mkdir, readdir, rename, rm, stat, utimes, writeFile } from "fs/promises";
import { dirname, join } from "path";

// src/claude-session.ts
function configuredValue(value) {
  return value !== undefined && value.trim().length > 0 ? value : undefined;
}
function claudeSessionId(environment) {
  const id = configuredValue(environment.ENVOY_SESSION_ID) ?? configuredValue(environment.CLAUDE_CODE_SESSION_ID);
  if (id !== undefined)
    return id;
  throw new Error("Envoy channel requires ENVOY_SESSION_ID or CLAUDE_CODE_SESSION_ID; Claude Code did not provide a session identity");
}
function claudeProjectDirectory(environment, fallback) {
  return configuredValue(environment.CLAUDE_PROJECT_DIR) ?? fallback;
}

// src/session-state.ts
async function writeAtomicStateFile(file, contents) {
  await mkdir(dirname(file), { recursive: true, mode: 448 });
  const temporary = `${file}.${process.pid}.${crypto.randomUUID()}.tmp`;
  await writeFile(temporary, contents, { mode: 384 });
  await rename(temporary, file);
}
async function touchStateFile(file) {
  const now = new Date;
  await utimes(file, now, now);
}
async function pruneStateDirectory(directory, isStale) {
  const entries = await readdir(directory).catch((error) => {
    if (hasErrnoCode(error, "ENOENT"))
      return [];
    throw error;
  });
  for (const entry of entries) {
    const file = join(directory, entry);
    if (!await isStale(file))
      continue;
    await rm(file, { recursive: true, force: true });
  }
}
var STATE_FILE_MAX_AGE_MS = 30 * 24 * 60 * 60 * 1000;
async function writtenLongAgo(file) {
  const modified = await stat(file).then((stats) => stats.mtimeMs, () => {
    return;
  });
  return modified !== undefined && Date.now() - modified > STATE_FILE_MAX_AGE_MS;
}
var TITLES_DIRECTORY = "titles";
function sessionTitleFile(stateDirectory, sessionId) {
  return join(stateDirectory, TITLES_DIRECTORY, `${encodeURIComponent(sessionId)}.txt`);
}
async function writeChosenTitle(file, title) {
  await writeAtomicStateFile(file, `${title}
`);
}
async function removeChosenTitle(file) {
  await rm(file, { force: true });
}
function readChosenTitleSync(file) {
  try {
    return readFileSync(file, "utf8");
  } catch {
    return;
  }
}
async function recordChosenTitle(sessionId, title) {
  const pluginData = configuredValue(process.env["CLAUDE_PLUGIN_DATA"]);
  if (pluginData === undefined)
    return;
  const file = sessionTitleFile(pluginData, sessionId);
  const chosen = configuredValue(title);
  if (chosen === undefined)
    await removeChosenTitle(file);
  else
    await writeChosenTitle(file, chosen);
}
async function pruneStaleSessionTitles(stateDirectory) {
  await pruneStateDirectory(join(stateDirectory, TITLES_DIRECTORY), writtenLongAgo);
}

// hooks/session-title-hook.ts
function chosenTitlePayload(raw) {
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) {
    throw new Error(`expected a JSON object, received ${JSON.stringify(raw)}`);
  }
  const { session_id: sessionId, session_title: title } = raw;
  if (typeof sessionId !== "string" || sessionId.length === 0) {
    throw new Error(`session_id must be a non-empty string, received ${JSON.stringify(sessionId)}`);
  }
  if (title !== undefined && typeof title !== "string") {
    throw new Error(`session_title must be a string, received ${JSON.stringify(title)}`);
  }
  return { sessionId, title };
}
try {
  const { sessionId, title } = chosenTitlePayload(JSON.parse(await Bun.stdin.text()));
  await recordChosenTitle(sessionId, title);
} catch (error) {
  process.stderr.write(`envoy: could not record the session title \u2014 ${messageFor(error)}
`);
}
