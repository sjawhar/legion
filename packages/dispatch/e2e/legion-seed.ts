// The Legion view's e2e fixture, written straight into Postgres: the board reads only what the
// sampler recorded of a project's daemon (`legion_settings`, `legion_reads`,
// `legion_observations`), so these are the rows a minute of reading would have left, without a
// daemon to read. One module, so the view's spec and anything that comes after it read one board.
//
// A sampler pass that fires while a spec runs cannot disturb what these spec assert: a pass writes
// a `legion_reads` row, which only the header's freshness line reads, and an observation only
// where a value moved — and the fake daemon here answers no state at all, so every pass of it
// fails and leaves every observation where it is.
//
// acme/widgets-shaped placeholders throughout (AGENTS.md: "This repository is public").
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";

import { sql } from "./psql";

const SEED_ACTOR = `'{"kind":"user","id":"alice"}'`;

/** The project the fixture's board belongs to. */
export const LEGION_PROJECT = "ACME";

/** One observation row: the field of one issue (or of the project, under the empty key) as a read
 *  found it, and how long ago that read was. */
interface Observation {
  readonly issue: string;
  readonly field: string;
  readonly value: string;
  /** A Postgres interval: how long before now the value was first seen. */
  readonly ago: string;
}

/** The daemon's admission block: a cap of four with two slots held, two roots waiting in the
 *  daemon's own order — which is neither Dispatch's key order nor its priority order — and one
 *  root a blocker link holds. */
const ADMISSION = {
  cap: 4,
  active: ["ACME-1", "ACME-2"],
  waiting: ["ACME-4", "ACME-3"],
  trees: ["ACME-1", "ACME-2"],
  blocked: [{ issue: "ACME-5", blockedBy: ["ACME-1"] }],
};

/** The settings the daemon reports, and where each got its value: one from its configuration file
 *  and one someone changed through the settings route. */
const SETTINGS = { admission_cap: 4, review_round_cap: 3 };
const SETTING_SOURCES = {
  admission_cap: { source: "file", at: "2026-10-20T00:00:00Z" },
  review_round_cap: { source: "route", by: "bob", at: "2026-11-01T09:00:00Z" },
};

const OBSERVATIONS: readonly Observation[] = [
  { issue: "", field: "admission", value: JSON.stringify(ADMISSION), ago: "3 hours" },
  { issue: "", field: "settings", value: JSON.stringify(SETTINGS), ago: "3 hours" },
  {
    issue: "",
    field: "setting_sources",
    value: JSON.stringify(SETTING_SOURCES),
    ago: "3 hours",
  },
  // ACME-1 holds slot 0 and is being implemented, with its implementer in a turn and its pull
  // request's checks failing.
  { issue: "ACME-1", field: "phase", value: '"implementing"', ago: "2 hours" },
  { issue: "ACME-1", field: "slot", value: "0", ago: "3 hours" },
  { issue: "ACME-1", field: "claim.implementer", value: '"working"', ago: "20 minutes" },
  { issue: "ACME-1", field: "pull_request.checks_verdict", value: '"failing"', ago: "15 minutes" },
  // ACME-2 holds slot 1 and waits on a person.
  { issue: "ACME-2", field: "phase", value: '"awaiting_person"', ago: "45 minutes" },
  { issue: "ACME-2", field: "slot", value: "1", ago: "3 hours" },
  {
    issue: "ACME-2",
    field: "waiting_on",
    value: JSON.stringify({ kind: "ask", since: "2026-11-03T04:19:00Z", answeredAt: null }),
    ago: "45 minutes",
  },
  // The two roots in the waiting line, and the one a blocker holds: each is in the queue, so
  // none of them has a phase of its own yet.
  { issue: "ACME-3", field: "phase", value: '"admitted"', ago: "90 minutes" },
  { issue: "ACME-4", field: "phase", value: '"admitted"', ago: "80 minutes" },
  { issue: "ACME-5", field: "phase", value: '"admitted"', ago: "70 minutes" },
];

/** The project, its five issues, its daemon at `stateUrl`, one good read, and every observation
 *  that read found. The issues are Dispatch's own, which is where each card's title and priority
 *  come from. */
export async function seedLegionFixture(stateUrl: string): Promise<void> {
  await sql(
    `INSERT INTO projects (key, name) VALUES ('${LEGION_PROJECT}', 'Acme') ON CONFLICT DO NOTHING`,
    `INSERT INTO people (login) VALUES ('alice') ON CONFLICT DO NOTHING`,
    `INSERT INTO issues (key, project_key, number, title, status, priority, created_by, rank) VALUES
       ('ACME-1', '${LEGION_PROJECT}', 1, 'Ship the widgets', 'in_progress', 0, ${SEED_ACTOR}, 'U'),
       ('ACME-2', '${LEGION_PROJECT}', 2, 'Ask about the gadgets', 'in_progress', 1, ${SEED_ACTOR}, 'V'),
       ('ACME-3', '${LEGION_PROJECT}', 3, 'Queued behind the widgets', 'todo', 2, ${SEED_ACTOR}, 'W'),
       ('ACME-4', '${LEGION_PROJECT}', 4, 'Queued first by the daemon', 'todo', 3, ${SEED_ACTOR}, 'X'),
       ('ACME-5', '${LEGION_PROJECT}', 5, 'Held by a blocker', 'todo', 3, ${SEED_ACTOR}, 'Y')`,
    `INSERT INTO legion_settings (project, state_url, created_by, updated_at)
       VALUES ('${LEGION_PROJECT}', '${stateUrl}', ${SEED_ACTOR}, now())`,
    `INSERT INTO legion_reads (project, read_at, ok, error, bytes)
       VALUES ('${LEGION_PROJECT}', now() - interval '20 seconds', true, null, 4096)`,
    `INSERT INTO legion_observations (project, issue_key, field, value, observed_at) VALUES ${OBSERVATIONS.map(
      (observation) =>
        `('${LEGION_PROJECT}', '${observation.issue}', '${observation.field}', '${observation.value.replaceAll("'", "''")}'::jsonb, now() - interval '${observation.ago}')`
    ).join(", ")}`
  );
}

/** A Legion daemon as far as Dispatch's relay is concerned: it answers
 *  `PUT /legion/v1/operator/settings` with `answer`, under `status`, and records the body and the
 *  bearer it saw. Every other route is a 404, so a sampler pass that reads its state while a spec
 *  runs records a failed read and changes no observation.
 *
 *  It listens on a port the OS chooses, on the loopback the Dispatch server reaches it by. */
export interface FakeLegionDaemon {
  readonly url: string;
  /** The bodies the relay sent, in order, each as the daemon received it. */
  readonly changes: { by?: string; changes?: Record<string, unknown> }[];
  /** The `Authorization` header of the last call, or undefined when it carried none. */
  authorization(): string | undefined;
  close(): Promise<void>;
}

export async function startFakeLegionDaemon(
  answer: Record<string, unknown>,
  status = 200
): Promise<FakeLegionDaemon> {
  const changes: { by?: string; changes?: Record<string, unknown> }[] = [];
  let authorization: string | undefined;
  const server: Server = createServer((incoming, response) => {
    if (incoming.method !== "PUT" || incoming.url !== "/legion/v1/operator/settings") {
      response.writeHead(404).end("{}");
      return;
    }
    authorization = incoming.headers.authorization;
    const chunks: Buffer[] = [];
    incoming.on("data", (chunk: Buffer) => chunks.push(chunk));
    incoming.on("end", () => {
      changes.push(JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}"));
      response.writeHead(status, { "content-type": "application/json" });
      response.end(JSON.stringify(answer));
    });
  });
  const listening = Promise.withResolvers<void>();
  server.listen(0, "127.0.0.1", listening.resolve);
  await listening.promise;
  const { port } = server.address() as AddressInfo;
  return {
    authorization: () => authorization,
    changes,
    close: () => {
      const closed = Promise.withResolvers<void>();
      server.close((error) => (error === undefined ? closed.resolve() : closed.reject(error)));
      return closed.promise;
    },
    url: `http://127.0.0.1:${port}`,
  };
}
