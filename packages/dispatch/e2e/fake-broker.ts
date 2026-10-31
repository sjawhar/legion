import type {
  CredentialDecisionResponse,
  CredentialPendingResponse,
  CredentialPendingRow,
  CredentialRecord,
} from "../web/src/api/types";
import { harnessPorts } from "./harness-ports";

// The secrets broker's reads Dispatch relays on every Inbox and Settings load: `GET /v1/pending`
// (the credential requests waiting on an approver), `GET /v1/credential-requests/{id}` (one
// record's full detail, for the record page) and `GET /v1/grants` (the live grants of a person's
// sessions and those the person approved), and the two routes Dispatch's machine-login page
// drives: `POST /v1/machine-logins/lookup` (the pending login a typed code selects) and
// `POST /v1/credential-requests/{id}/approve|deny` (that login's decision, carrying the code
// again). The server reaches this through
// DISPATCH_AGENT_SECRETS_URL (run-server.sh) whenever the harness switch is left unset
// (e2e/harness-broker.ts), and it lists nothing until a test seeds a request through
// /__fixture/pending, a record through /__fixture/records or a machine login through
// /__fixture/machine-logins, relaying each row as seeded: a
// secret request's row carries the `approve_url` of its page on the broker that the Inbox links
// to (an absolute URL, which e2e/broker-approval.e2e.ts points at a record on the real broker,
// e2e/run-broker.sh), a machine login's row none. It verifies no caller: the token the server
// presents, e2e/fake-broker-account-token's, is never read. Every other broker route answers 404.

/** A pending credential request as the broker lists it, beside the approver it waits on: the row
 *  is the SPA's own type, so a change to the contract fails the typecheck here too. */
export type FakePendingRequest = CredentialPendingRow & { readonly approver: string };

/** A machine login the operator's typed code selects, as `POST /v1/machine-logins/lookup`
 *  resolves it: the code the machine printed, and the record that lookup and every later read
 *  answer. Deciding the record writes its decision back into `record`, so a second lookup of the
 *  same code reports what the broker recorded. */
export interface FakeMachineLogin {
  readonly code: string;
  record: CredentialRecord;
}

/** Every piece of fixture state this listener holds; `PUT /__fixture/reset` replaces it whole. */
interface FakeBrokerState {
  machineLogins: FakeMachineLogin[];
  pending: FakePendingRequest[];
  records: Record<string, CredentialRecord>;
}

function emptyState(): FakeBrokerState {
  return { machineLogins: [], pending: [], records: {} };
}

let state = emptyState();

/** A confirmation code as the broker prints it, from the code as a person typed it: upper-cased,
 *  spaces dropped and the hyphen in place (internal/broker/oauth/codes.go's NormalizeUserCode). */
function normalizeCode(code: string): string {
  const bare = code.replaceAll(/[ -]/g, "").toUpperCase();
  return bare.length === 8 ? `${bare.slice(0, 4)}-${bare.slice(4)}` : bare;
}

function brokerError(status: number, code: string, error: string): Response {
  return Response.json({ code, error }, { status });
}

/** Decides a machine login as internal/broker/api/handlers_ui_records.go does: the code is typed
 *  again (the only thing that selects a machine record), the approver Dispatch named from its own
 *  session must be one the record admits, and a record already decided is terminal. An approval
 *  mints the credential the login then holds, named after the record so a test can read it. */
async function decide(recordID: string, approve: boolean, request: Request): Promise<Response> {
  const login = state.machineLogins.find((entry) => entry.record.record_id === recordID);
  if (login === undefined) {
    return brokerError(404, "NOT_FOUND", "no such credential request");
  }
  const body = (await request.json()) as { approver?: string; code?: string };
  if ((body.approver ?? "") === "") {
    return brokerError(400, "APPROVER_REQUIRED", "approver is required");
  }
  if ((body.code ?? "") === "") {
    return brokerError(400, "CODE_REQUIRED", "code is required to decide a machine login");
  }
  if (normalizeCode(body.code ?? "") !== normalizeCode(login.code)) {
    return brokerError(403, "CODE_MISMATCH", "that is not this machine login's code");
  }
  const { record } = login;
  if (record.approver !== "anyone" && record.approver !== body.approver) {
    return brokerError(403, "NOT_APPROVER", `${body.approver} may not decide this request`);
  }
  if (record.decided !== null) {
    return brokerError(409, "RECORD_TERMINAL", "this machine login was already decided");
  }
  const credentialID = approve ? `cred-${record.record_id}` : null;
  login.record = {
    ...record,
    decided: {
      at: new Date().toISOString(),
      credential_id: credentialID,
      event: approve ? "approved" : "denied",
    },
    state: approve ? "approved" : "denied",
  };
  const answer: CredentialDecisionResponse = approve
    ? { credential_id: credentialID, state: "approved" }
    : { state: "denied" };
  return Response.json(answer);
}

// FAKE_BROKER_PORT, or with it unset port 0, so the OS chooses; the line at the end reports the
// port bound, which e2e/harness-start.ts reads.
const server = Bun.serve({
  hostname: "127.0.0.1",
  port: harnessPorts().fakeBroker.port,
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
    if (request.method === "GET" && url.pathname.startsWith("/v1/credential-requests/")) {
      const recordID = decodeURIComponent(url.pathname.slice("/v1/credential-requests/".length));
      const record = state.records[recordID];
      if (record === undefined) {
        return brokerError(404, "NOT_FOUND", "no such credential request");
      }
      return Response.json(record);
    }
    if (request.method === "GET" && url.pathname === "/v1/grants") {
      return Response.json({ grants: [] });
    }
    // Only the typed code selects a machine login (ruling 13), so this answers that login's
    // record whole, decided or not, as internal/broker/api/handlers_ui_machinelogin.go does.
    if (request.method === "POST" && url.pathname === "/v1/machine-logins/lookup") {
      const { code } = (await request.json()) as { code?: string };
      if ((code ?? "") === "") {
        return brokerError(400, "CODE_REQUIRED", "code is required");
      }
      const login = state.machineLogins.find(
        (entry) => normalizeCode(entry.code) === normalizeCode(code ?? "")
      );
      if (login === undefined) {
        return brokerError(404, "NO_SUCH_CODE", "no pending machine login has this code");
      }
      return Response.json(login.record);
    }
    if (request.method === "POST" && url.pathname.startsWith("/v1/credential-requests/")) {
      const rest = url.pathname.slice("/v1/credential-requests/".length);
      const slash = rest.lastIndexOf("/");
      const action = rest.slice(slash + 1);
      if (action === "approve" || action === "deny") {
        return decide(decodeURIComponent(rest.slice(0, slash)), action === "approve", request);
      }
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/pending") {
      const pending = (await request.json()) as FakePendingRequest[];
      state = { ...state, pending };
      return Response.json({ ok: true });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/records") {
      const records = (await request.json()) as CredentialRecord[];
      state = { ...state, records: Object.fromEntries(records.map((r) => [r.record_id, r])) };
      return Response.json({ ok: true });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/machine-logins") {
      const machineLogins = (await request.json()) as FakeMachineLogin[];
      state = { ...state, machineLogins };
      return Response.json({ ok: true });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/reset") {
      state = emptyState();
      return Response.json({ ok: true });
    }
    if (url.pathname === "/healthz") return Response.json({ ok: true });
    return brokerError(404, "NOT_FOUND", "not found");
  },
});

console.log(`fake broker listener on 127.0.0.1:${server.port}`);
