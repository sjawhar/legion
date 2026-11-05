import { describe, expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";

/**
 * Every event payload is written once. Before this, each lived twice in `dispatch-api.ts` - a
 * hand-written type in one band and a tolerant zod schema in another, with nothing deriving
 * either from the other - and the two drifted: `IssueEventPayloadSchema` declared nine of the
 * type's twenty-five fields. This test is what keeps them one: a payload type may only be
 * `z.output` of the schema that carries its name, so a field added to the schema is a field of
 * the type, and a field added to the type has nowhere to go but the schema. Where a payload is
 * read two ways, the plain name is the reading a consumer of the bus applies and the exact
 * schema carries the `Written` prefix (`dispatch-api.ts` states the rule), so the type derives
 * from `WrittenXSchema` and `XSchema` is exported beside it.
 */

const SOURCE = import.meta.dir;

/** Every payload one `DispatchEvent` member carries, the two payload mixins, what an
 *  `issue.*` write reports it changed, and the one payload published outside the event log.
 *  `DERIVED_TYPES` below reads that same set out of the sources, and the two must agree: a
 *  member added to the union with a payload nobody listed here fails. */
const PAYLOAD_TYPES = [
  "ArchitectureSourceUpdatedEventPayload",
  "ArchitectureSyncFailedEventPayload",
  "ArchitectureSyncedEventPayload",
  "ArtifactCreatedEventPayload",
  "ArtifactHumanEditedEventPayload",
  "ArtifactReviewEventPayload",
  "ArtifactVersionEventPayload",
  "AskAnsweredEventPayload",
  "AskEditEventPayload",
  "AskEventPayload",
  "AskFollowerEventPayload",
  "BlockInvalidEventPayload",
  "BlockRepairedEventPayload",
  "CheckResultEventPayload",
  "ChildEventPayload",
  "ChildStatusEventPayload",
  "CommentDeliveryEventPayload",
  "CommentEventPayload",
  "DecisionKeptEventPayload",
  "DispatchServerStartedPayload",
  "HandoffRecordedPayload",
  "IssueClaimEventPayload",
  "IssueDeployedEventPayload",
  "IssueEventChanges",
  "IssueEventPayload",
  "IssueReleasedEventPayload",
  "MessageAcceptedEventPayload",
  "MessageDeliveryEventPayload",
  "MessageEventPayload",
  "Project",
  "ProjectDefaultAssigneeUpdatedEventPayload",
  "ReferenceChangesPayload",
  "RepoProjectUpdatedEventPayload",
  "SubscriptionRemovedEventPayload",
  "ThreadLoginsPayload",
  "UserAgentStateUpdatedEventPayload",
  "UserStateUpdatedEventPayload",
  "UserThreadStateUpdatedEventPayload",
] as const;

/** The four payloads no `DispatchEvent` member carries: the two mixins every payload spreads
 *  (`ReferenceChangesPayload`, `ThreadLoginsPayload`), what an `issue.*` write reports it
 *  changed (`IssueEventChanges`, carried inside `IssueEventPayload`), and the `server.started`
 *  payload, which is published on its own subject and never appended to the event log. */
const NON_EVENT_PAYLOAD_TYPES = [
  "DispatchServerStartedPayload",
  "IssueEventChanges",
  "ReferenceChangesPayload",
  "ThreadLoginsPayload",
];

/**
 * The payload field types that stay a closed list's own alias rather than `z.output` of a
 * schema: a schema spells such a field `z.enum(THE_LIST)`, and the type beside `THE_LIST` is
 * what the field produces. Six are expected, and a seventh means a list grew a schema-derived
 * type beside the alias, or a payload took a field whose values nobody closed.
 */
const CONSTANT_LIST_TYPES = [
  "AskTurn",
  "AskUrgency",
  "DeliveryCapability",
  "DispatchScope",
  "IssueComponentsMode",
  "MessageDeliveryMode",
];

/**
 * How many types of the contract are `z.output` of a schema: the payloads above and every shape
 * they are built from, down to the actor. Stated so a type written by hand beside a schema that
 * already describes it is a failure rather than a slow return to two spellings of one shape.
 */
const SCHEMA_DERIVATIONS = 85;

/** Every module of the split, as one text with its whitespace collapsed, so a declaration Biome
 *  wrapped over several lines reads as the one line it is. */
const MODULES = readdirSync(SOURCE).filter(
  (entry) =>
    entry.startsWith("dispatch-api") && entry.endsWith(".ts") && !entry.endsWith(".test.ts")
);

const collapsed = (entry: string): string =>
  readFileSync(path.join(SOURCE, entry), "utf8").replace(/\s+/g, " ");

const SOURCES = MODULES.map(collapsed).join("\n");

const UNION_OPENS = "export type DispatchEvent =";
const UNION_CLOSES = "export type Event = DispatchEvent;";

/** The `DispatchEvent` union itself, so the payload set below is read from the members rather
 *  than from anything that merely mentions a payload. */
function dispatchEventUnion(): string {
  const events = collapsed("dispatch-api-events.ts");
  const opens = events.indexOf(UNION_OPENS);
  const closes = events.indexOf(UNION_CLOSES, opens);
  if (opens < 0 || closes < 0) {
    throw new Error(
      `the DispatchEvent union no longer runs from "${UNION_OPENS}" to "${UNION_CLOSES}" in dispatch-api-events.ts`
    );
  }
  return events.slice(opens, closes);
}

/** The payload set as the sources state it: every `DispatchEvent` member's payload, plus the
 *  four payloads no member carries. */
const DERIVED_TYPES = [
  ...new Set([
    ...[...dispatchEventUnion().matchAll(/readonly payload: ([A-Za-z0-9_]+)/g)].map(
      (match) => match[1] as string
    ),
    ...NON_EVENT_PAYLOAD_TYPES,
  ]),
].sort();

/** Every type that is a closed list's alias, where a payload schema reads that list with
 *  `z.enum`, and every type that is in turn an alias of one of those. */
function constantListAliases(): string[] {
  const lists = new Set(
    [...SOURCES.matchAll(/z\.enum\(([A-Z][A-Z0-9_]*)\)/g)].map((match) => match[1] as string)
  );
  const aliases = new Set<string>();
  for (const match of SOURCES.matchAll(
    /export type ([A-Za-z0-9_]+) = \(typeof ([A-Z][A-Z0-9_]*)\)\[number\];/g
  )) {
    if (lists.has(match[2] as string)) aliases.add(match[1] as string);
  }
  for (const match of SOURCES.matchAll(/export type ([A-Za-z0-9_]+) = ([A-Za-z0-9_]+);/g)) {
    if (aliases.has(match[2] as string)) aliases.add(match[1] as string);
  }
  return [...aliases].sort();
}

describe("every event payload is z.output of its own schema", () => {
  for (const name of PAYLOAD_TYPES) {
    test(name, () => {
      const declarations = SOURCES.match(
        new RegExp(String.raw`export (?:type|interface) ${name}\b`, "g")
      );
      expect(declarations, `${name} is declared once`).toHaveLength(1);
      const derivation = String.raw`export type ${name} = z\.output< ?typeof (Written)?${name}Schema ?>;`;
      const derived = SOURCES.match(new RegExp(derivation));
      expect(derived, `${name} is z.output of ${name}Schema or of Written${name}Schema`).not.toBe(
        null
      );
      // A `Written` prefix says the payload is read two ways, so the plain name is the reading a
      // consumer of the bus applies and must be exported beside it.
      if (derived?.[1] !== undefined) {
        expect(SOURCES, `${name}Schema is the bus reading of Written${name}Schema`).toMatch(
          new RegExp(`export const ${name}Schema[ =]`)
        );
      }
    });
  }

  test("the payload set is the one the DispatchEvent union carries", () => {
    const listed: string[] = [...PAYLOAD_TYPES];
    expect(listed).toEqual(DERIVED_TYPES);
  });

  test(`${CONSTANT_LIST_TYPES.length} payload field types stay a closed list's alias`, () => {
    expect(constantListAliases()).toEqual(CONSTANT_LIST_TYPES);
  });

  test(`the contract derives ${SCHEMA_DERIVATIONS} types from a schema`, () => {
    const derived = SOURCES.match(/export type [A-Za-z0-9_]+ = z\.output</g) ?? [];
    expect(derived).toHaveLength(SCHEMA_DERIVATIONS);
  });
});
