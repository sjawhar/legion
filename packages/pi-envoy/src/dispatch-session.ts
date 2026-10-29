// The one builder of the session's Dispatch, for every request this plugin makes on the session's
// own behalf: the extension's calls (`extensions/envoy.ts`) and AskGate's reads of a document
// (`extensions/askgate/askgate.ts`). The configuration is resolved at each call, so a Dispatch that
// moved — a new `dispatch.serverUrl` in the session's envoy.json, or a checkout's own
// `.envoy/envoy.json` under the directory a gated command runs in — takes effect without a reload.
import { activeDispatchSettings } from "@legion/envoy-client/dispatch-config";
import { dispatchServerUrls } from "@legion/envoy-client/dispatch-execute";
import { DispatchClient } from "@legion/envoy-client/dispatch-http";
import {
  type SessionConfigFile,
  sessionDispatchSettings,
} from "@legion/envoy-client/session-settings";

/** A Dispatch client for one directory, and the addresses a dashboard-URL `ref` may be on. */
export interface DispatchSession {
  readonly client: DispatchClient;
  readonly serverUrls: readonly string[];
}

/**
 * The session's Dispatch as `file` and the configuration under `cwd` resolve it (a checkout's own
 * `.envoy/envoy.json` names its own server), with `signal` on every request of it, so the caller's
 * stop or timeout ends each one; undefined when Dispatch is not configured. A configuration that
 * cannot be read throws, as `activeDispatchSettings` does.
 */
export function sessionDispatchClient(
  file: SessionConfigFile,
  cwd: string,
  signal: AbortSignal
): DispatchSession | undefined {
  const config = activeDispatchSettings(
    sessionDispatchSettings(file, process.env, { cwd }).dispatch
  );
  return config === null
    ? undefined
    : {
        client: new DispatchClient(config.url, config.token, fetch, signal),
        serverUrls: dispatchServerUrls(config),
      };
}
