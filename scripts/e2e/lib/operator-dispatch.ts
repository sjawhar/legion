// scripts/e2e/lib/operator-dispatch.ts: the operator's Dispatch, as a command run from the
// operator's shell reaches it. The envoy.json is the one `envoyConfigPath("command")` chooses, and
// the URL and bearer are what `sessionDispatchSettings` resolves from it, the working directory's
// `.envoy/envoy.json` and the environment (packages/envoy-client/src/session-settings.ts), so no
// script keeps its own copy of the which-file or same-server rule. lib/operator-dispatch.sh runs it.
//
//   bun scripts/e2e/lib/operator-dispatch.ts
//     prints one JSON object: {"file","url","token"} when Dispatch is on, and {"file","notice"}
//     when nothing configures it or a file turns it off, exiting 0; {"error"} with exit 1 when a
//     file, a URL or a token prevents it (the error never carries the token). A file it found and
//     did not read, a checkout's `.opencode/envoy.json`, is named on stderr with its rename first.
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

if (import.meta.main) {
  try {
    process.stdout.write(`${JSON.stringify(operatorDispatch(process.env, process.cwd()))}\n`);
  } catch (error) {
    process.stdout.write(`${JSON.stringify({ error: messageFor(error) })}\n`);
    process.exitCode = 1;
  }
}
