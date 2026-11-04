import { describe, expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";

/**
 * Every event payload is written once. Before this, each lived twice in `dispatch-api.ts` - a
 * hand-written type in one band and a tolerant zod schema in another, with nothing deriving
 * either from the other - and the two drifted: `IssueEventPayloadSchema` declared nine of the
 * type's twenty-five fields. This test is what keeps them one: a payload type may only be
 * `z.output` of the schema that carries its name, so a field added to the schema is a field of
 * the type, and a field added to the type has nowhere to go but the schema.
 */

const SOURCE = import.meta.dir;

/** Every payload one `DispatchEvent` member carries, the two payload mixins, what an
 *  `issue.*` write reports it changed, and the one payload published outside the event log. */
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

/**
 * How many types of the contract are `z.output` of a schema: the payloads above and every shape
 * they are built from, down to the actor. Stated so a type written by hand beside a schema that
 * already describes it is a failure rather than a slow return to two spellings of one shape.
 */
const DERIVED_TYPES = 85;

/** Every module of the split, as one text with its whitespace collapsed, so a declaration Biome
 *  wrapped over several lines reads as the one line it is. */
const SOURCES = readdirSync(SOURCE)
  .filter(
    (entry) =>
      entry.startsWith("dispatch-api") && entry.endsWith(".ts") && !entry.endsWith(".test.ts")
  )
  .map((entry) => readFileSync(path.join(SOURCE, entry), "utf8").replace(/\s+/g, " "))
  .join("\n");

describe("every event payload is z.output of its own schema", () => {
  for (const name of PAYLOAD_TYPES) {
    test(name, () => {
      const declarations = SOURCES.match(
        new RegExp(String.raw`export (?:type|interface) ${name}\b`, "g")
      );
      expect(declarations, `${name} is declared once`).toHaveLength(1);
      const derivation = String.raw`export type ${name} = z\.output< ?typeof ${name}Schema ?>;`;
      expect(SOURCES, `${name} is z.output of ${name}Schema`).toMatch(new RegExp(derivation));
    });
  }

  test(`the contract derives ${DERIVED_TYPES} types from a schema`, () => {
    const derived = SOURCES.match(/export type [A-Za-z0-9_]+ = z\.output</g) ?? [];
    expect(derived).toHaveLength(DERIVED_TYPES);
  });
});
