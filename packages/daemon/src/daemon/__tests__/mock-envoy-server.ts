/**
 * Mock Envoy server for integration tests.
 *
 * Implements the subset of the Envoy listener HTTP API that the daemon uses:
 *   POST /v1/interests/subscribe
 *   POST /v1/interests/unsubscribe
 *   POST /v1/messages/publish
 *   POST /v1/roles/set
 *   GET  /healthz
 *
 * Each test gets its own server on an ephemeral port — full isolation from
 * the live Envoy listener and from other test suites.
 */

export interface SubscribeCall {
  session_id: string;
  topics: string[];
}

export interface UnsubscribeCall {
  session_id: string;
  topics: string[];
}

export interface PublishCall {
  topic: string;
  message: string;
  source?: string;
  source_session?: string;
}

export interface RoleSetCall {
  session_id: string;
  role: string;
}

export interface MockEnvoyServer {
  /** Base URL for the mock server (e.g. "http://127.0.0.1:54321") */
  url: string;
  /** Stop the server */
  stop: () => void;
  /** All recorded subscribe calls */
  subscribeCalls: SubscribeCall[];
  /** All recorded unsubscribe calls */
  unsubscribeCalls: UnsubscribeCall[];
  /** All recorded publish calls */
  publishCalls: PublishCall[];
  /** All recorded role-set calls */
  roleSetCalls: RoleSetCall[];
  /** In-memory interest registry: session_id → topics[] */
  interests: Map<string, string[]>;
  /** In-memory role registry: role → session_id */
  roles: Map<string, string>;
  /** Override status code for subscribe responses (default 200) */
  subscribeStatus: number;
  /** Override status code for unsubscribe responses (default 200) */
  unsubscribeStatus: number;
  /** Override status code for publish responses (default 200) */
  publishStatus: number;
  /** Override status code for role-set responses (default 200) */
  roleSetStatus: number;
  /** If set, subscribe requests will throw this error instead of responding */
  subscribeError: Error | null;
  /** Reset all recorded calls and state */
  reset: () => void;
}

export function createMockEnvoyServer(): MockEnvoyServer {
  const state: MockEnvoyServer = {
    url: "",
    stop: () => {},
    subscribeCalls: [],
    unsubscribeCalls: [],
    publishCalls: [],
    roleSetCalls: [],
    interests: new Map(),
    roles: new Map(),
    subscribeStatus: 200,
    unsubscribeStatus: 200,
    publishStatus: 200,
    roleSetStatus: 200,
    subscribeError: null,
    reset() {
      state.subscribeCalls.length = 0;
      state.unsubscribeCalls.length = 0;
      state.publishCalls.length = 0;
      state.roleSetCalls.length = 0;
      state.interests.clear();
      state.roles.clear();
      state.subscribeStatus = 200;
      state.unsubscribeStatus = 200;
      state.publishStatus = 200;
      state.roleSetStatus = 200;
      state.subscribeError = null;
    },
  };

  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch(req) {
      const url = new URL(req.url);

      if (req.method === "GET" && url.pathname === "/healthz") {
        return Response.json({ status: "healthy" });
      }

      if (req.method === "POST" && url.pathname === "/v1/interests/subscribe") {
        return (req.json() as Promise<SubscribeCall>).then((body) => {
          state.subscribeCalls.push(body);
          if (state.subscribeStatus === 200) {
            // Realistic behavior: merge topics into interest registry
            const existing = state.interests.get(body.session_id) ?? [];
            const merged = [...new Set([...existing, ...body.topics])];
            state.interests.set(body.session_id, merged);
            return Response.json(
              {
                session_id: body.session_id,
                topics: merged,
              },
              { status: 200 }
            );
          }
          return new Response("{}", { status: state.subscribeStatus });
        });
      }

      if (req.method === "POST" && url.pathname === "/v1/interests/unsubscribe") {
        return (req.json() as Promise<UnsubscribeCall>).then((body) => {
          state.unsubscribeCalls.push(body);
          if (state.unsubscribeStatus === 200) {
            if (body.topics.length === 0) {
              // Empty topics = full unsubscribe (remove all)
              state.interests.delete(body.session_id);
            } else {
              const existing = state.interests.get(body.session_id) ?? [];
              const remaining = existing.filter((t) => !body.topics.includes(t));
              if (remaining.length > 0) {
                state.interests.set(body.session_id, remaining);
              } else {
                state.interests.delete(body.session_id);
              }
            }
            return Response.json("ok", { status: 200 });
          }
          return new Response("{}", { status: state.unsubscribeStatus });
        });
      }

      if (req.method === "POST" && url.pathname === "/v1/messages/publish") {
        return (req.json() as Promise<PublishCall>).then((body) => {
          state.publishCalls.push(body);
          if (state.publishStatus === 200) {
            return Response.json(
              {
                event_id: `mock-${Date.now()}`,
                topic: body.topic,
                message: body.message,
              },
              { status: 200 }
            );
          }
          return new Response("{}", { status: state.publishStatus });
        });
      }

      if (req.method === "POST" && url.pathname === "/v1/roles/set") {
        return (req.json() as Promise<RoleSetCall>).then((body) => {
          state.roleSetCalls.push(body);
          if (state.roleSetStatus === 200) {
            state.roles.set(body.role, body.session_id);
            return Response.json(
              {
                session_id: body.session_id,
                role: body.role,
              },
              { status: 200 }
            );
          }
          return new Response("{}", { status: state.roleSetStatus });
        });
      }

      return new Response("Not Found", { status: 404 });
    },
  });

  state.url = `http://127.0.0.1:${server.port}`;
  state.stop = () => server.stop(true);

  return state;
}
