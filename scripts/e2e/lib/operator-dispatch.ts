// scripts/e2e/lib/operator-dispatch.ts: the operator's Dispatch, as a command run from the
// operator's shell reaches it. The envoy.json is the one `envoyConfigPath("command")` chooses, and
// the URL and bearer are what `dispatchSettings` resolves from it, the working directory's
// `.envoy/envoy.json` and the environment (packages/envoy-client/src/dispatch-config.ts), so no
// script keeps its own copy of the which-file or same-server rule. lib/operator-dispatch.sh runs it.
//
//   bun scripts/e2e/lib/operator-dispatch.ts
//     prints one JSON object: {"file","url","token"} when Dispatch is on, and {"file","notice"}
//     when nothing configures it or a file turns it off, exiting 0; {"error"} with exit 1 when a
//     file, a URL or a token prevents it (the error never carries the token). A file it found and
//     did not read, a checkout's `.opencode/envoy.json`, is named on stderr with its rename first.
import {
  type DispatchEnvironment,
  dispatchSettings,
} from "../../../packages/envoy-client/src/dispatch-config";
import { envoyConfigPath, readEnvoyConfig } from "../../../packages/envoy-client/src/envoy-config";
import { messageFor } from "../../../packages/envoy-client/src/errors";

export type OperatorDispatch =
  | { readonly file: string; readonly url: string; readonly token: string }
  | { readonly file: string; readonly notice: string };

/** The operator's Dispatch from `env` and the checkout at `cwd`. Throws naming what prevents it:
 * a relative path the rule would use, a file that cannot be read, or a URL or token that does not
 * resolve. Each file the read found and did not read is written to stderr first, as the `dispatch`
 * command says it. */
export function operatorDispatch(env: DispatchEnvironment, cwd: string): OperatorDispatch {
  const file = envoyConfigPath("command", env);
  const config = readEnvoyConfig(file, env, { cwd });
  if (config.kind === "read") {
    for (const warning of config.warnings) process.stderr.write(`dispatch: ${warning}\n`);
  }
  const resolution = dispatchSettings(env, config);
  if (resolution.enabled) return { file, url: resolution.url, token: resolution.token };
  if (resolution.error !== null) throw new Error(resolution.error);
  return { file, notice: resolution.notice };
}

if (import.meta.main) {
  try {
    process.stdout.write(`${JSON.stringify(operatorDispatch(process.env, process.cwd()))}\n`);
  } catch (error) {
    process.stdout.write(`${JSON.stringify({ error: messageFor(error) })}\n`);
    process.exitCode = 1;
  }
}
