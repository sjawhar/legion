// scripts/e2e/lib/operator-dispatch.ts: the operator's Dispatch, as a command run from the
// operator's shell reaches it. The envoy.json is the one `envoyConfigPath("command")` chooses, and
// the URL and bearer are what `sessionDispatchSettings` resolves from it, the working directory's
// `.envoy/envoy.json` and the environment (packages/envoy-client/src/session-settings.ts), so no
// script keeps its own copy of the which-file or same-server rule. lib/operator-dispatch.sh runs it.
//
//   bun scripts/e2e/lib/operator-dispatch.ts <bearer file>
//     prints one JSON object: {"file","url"} when Dispatch is on, having written the bearer to
//     <bearer file>, which it creates owner-only (0600) and never over a file already there;
//     {"file","notice"} when nothing configures it or a file turns it off, writing nothing, exiting
//     0; {"error"} with exit 1 when a file, a URL or a token prevents it, or the bearer file cannot
//     be created. No output carries the bearer, so an agent that runs the command puts none in its
//     transcript. A file it found and did not read, a checkout's `.opencode/envoy.json`, is named
//     on stderr with its rename first. Without <bearer file> it exits 2 with its usage on stderr.
import { closeSync, openSync, writeSync } from "node:fs";
import type { DispatchEnvironment } from "../../../packages/envoy-client/src/dispatch-config";
import { messageFor } from "../../../packages/envoy-client/src/errors";
import {
  sessionConfigFile,
  sessionDispatchSettings,
} from "../../../packages/envoy-client/src/session-settings";

export type OperatorDispatch =
  | { readonly file: string; readonly url: string; readonly token: string }
  | { readonly file: string; readonly notice: string };

/** The operator's Dispatch from `env` and the checkout at `cwd`. Throws naming what prevents it:
 * a relative path the rule would use, a file that cannot be read, or a URL or token that does not
 * resolve. Each file the read found and did not read is written to stderr first, as the `dispatch`
 * command says it. */
export function operatorDispatch(env: DispatchEnvironment, cwd: string): OperatorDispatch {
  const file = sessionConfigFile("command", env);
  if (file.path === null) throw new Error(file.error);
  const { dispatch, warnings } = sessionDispatchSettings(file, env, { cwd });
  for (const warning of warnings) process.stderr.write(`dispatch: ${warning}\n`);
  if (dispatch.enabled) return { file: file.path, url: dispatch.url, token: dispatch.token };
  if (dispatch.error !== null) throw new Error(dispatch.error);
  return { file: file.path, notice: dispatch.notice };
}

/** Writes `bearer` to a new owner-only file at `path`, refusing one that is already there. */
function holdBearer(path: string, bearer: string): void {
  let fd: number;
  try {
    fd = openSync(path, "wx", 0o600);
  } catch (error) {
    throw new Error(`could not create the bearer file ${path}: ${messageFor(error)}`);
  }
  try {
    writeSync(fd, bearer);
  } finally {
    closeSync(fd);
  }
}

if (import.meta.main) {
  const bearerFile = process.argv[2];
  if (bearerFile === undefined || bearerFile === "") {
    process.stderr.write("usage: bun scripts/e2e/lib/operator-dispatch.ts <bearer file>\n");
    process.exit(2);
  }
  try {
    const resolved = operatorDispatch(process.env, process.cwd());
    if ("token" in resolved) {
      holdBearer(bearerFile, resolved.token);
      process.stdout.write(`${JSON.stringify({ file: resolved.file, url: resolved.url })}\n`);
    } else {
      process.stdout.write(`${JSON.stringify(resolved)}\n`);
    }
  } catch (error) {
    process.stdout.write(`${JSON.stringify({ error: messageFor(error) })}\n`);
    process.exitCode = 1;
  }
}
