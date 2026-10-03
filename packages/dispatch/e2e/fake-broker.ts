import type { CredentialPendingResponse, CredentialPendingRow } from "../web/src/api/types";
import { harnessPorts } from "./harness-ports";

// The secrets broker's two UI-bearer reads Dispatch relays on every Inbox and Settings load:
// `GET /v1/pending` (the credential requests waiting on an approver) and `GET /v1/grants` (the
// live grants an approver issued). The server reaches this through DISPATCH_AGENT_SECRETS_URL
// (run-server.sh) whenever the harness switch is left unset (e2e/harness-broker.ts), and it lists
// nothing until a test seeds a request through /__fixture/pending. Every other broker route
// answers 404.

/** A pending credential request as the broker lists it, beside the approver it waits on: the row
 *  is the SPA's own type, so a change to the contract fails the typecheck here too. */
export type FakePendingRequest = CredentialPendingRow & { readonly approver: string };

/** Every piece of fixture state this listener holds; `PUT /__fixture/reset` replaces it whole. */
interface FakeBrokerState {
  pending: FakePendingRequest[];
}

function emptyState(): FakeBrokerState {
  return { pending: [] };
}

let state = emptyState();

Bun.serve({
  hostname: "127.0.0.1",
  port: harnessPorts.fakeBroker.port,
  async fetch(request) {
    const url = new URL(request.url);
    // The broker's own row shape names no approver: it lists one approver's requests.
    if (request.method === "GET" && url.pathname === "/v1/pending") {
      const approver = url.searchParams.get("approver");
      const body: CredentialPendingResponse = {
        pending: state.pending
          .filter((row) => row.approver === approver)
          .map(({ approver: _, ...row }) => row),
      };
      return Response.json(body);
    }
    if (request.method === "GET" && url.pathname === "/v1/grants") {
      return Response.json({ grants: [] });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/pending") {
      const pending = (await request.json()) as FakePendingRequest[];
      state = { ...state, pending };
      return Response.json({ ok: true });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/reset") {
      state = emptyState();
      return Response.json({ ok: true });
    }
    if (url.pathname === "/healthz") return Response.json({ ok: true });
    return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
  },
});

console.log(`fake broker listener on 127.0.0.1:${harnessPorts.fakeBroker.port}`);
