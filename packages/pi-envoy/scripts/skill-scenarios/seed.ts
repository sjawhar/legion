// Seeds rig.sh's scratch Dispatch through its API. DISPATCH_E2E_PORT names the server, which
// e2e/api.ts addresses.
//
//   seed.ts project                 project LWEVAL and LWEVAL-1, the issue the tester's world implements
//   seed.ts ask-on-message <file>   one run's own issue and the owner's message posting its plan;
//                                   writes {issue, message} to <file>
import { writeFileSync } from "node:fs";
import {
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
} from "../../../dispatch/e2e/api";

const [command, out] = Bun.argv.slice(2);
if (command === "project") {
  await createProject({ key: "LWEVAL", name: "skill scenarios" });
  const issue = await createIssue(
    {
      project: "LWEVAL",
      title: "greet(): a greeting helper for the widgets CLI",
      spec: [
        "## Goal",
        "",
        "`greet(name)` returns `Hello, <name>!`, and `bun greet.ts <name>` prints it.",
        "",
        "## Acceptance",
        "",
        "1. `bun greet.ts Ada` prints `Hello, Ada!` and exits 0.",
        "2. `bun greet.ts` with no name exits 2 and prints `usage: greet.ts <name>` to stderr.",
        "3. Surrounding whitespace in the name is trimmed.",
      ].join("\n"),
    },
    { as: "agent" }
  );
  console.log(`seeded ${issue.key}`);
} else if (command === "ask-on-message" && out !== undefined) {
  // The owner session's world: an issue whose backfill was proposed and never run, the proposal
  // as an attached table, and the message that posts the plan for the rest of the work.
  const owner = { as: "agent", actor: { kind: "session", id: "dispatch-owner" } } as const;
  const issue = await createIssue(
    {
      project: "LWEVAL",
      title: "Make the Architecture page tell the truth: every issue attached to its components",
      spec: [
        "## Summary",
        "",
        "The Architecture page colours each component by the issues attached to it, but most issues",
        "in the two proving-ground trees (the Go refactor and the roadmap) are attached to nothing,",
        "so the page shows one gray box. Every issue in both trees should carry the components it",
        "changes, or a stated reason it is not architectural.",
      ].join("\n"),
    },
    owner
  );
  await createIssueArtifact(
    issue.key,
    {
      name: "attachment-dry-run.md",
      content: [
        "| issue | proposed components | why |",
        "| --- | --- | --- |",
        "| Go refactor: state store | daemon-go | changes the coordinator's store |",
        "| Go refactor: pod runtime | daemon-go, worker-image | changes the sandbox runtime |",
        "| Roadmap: hiring pipeline | none (not architectural) | hiring process work |",
        "| Roadmap: search ranking | dispatch-server | changes search |",
      ].join("\n"),
    },
    owner
  );
  const message = await createMessage(
    issue.key,
    {
      body: [
        "Taken by the Dispatch owner session, from tonight's owner audit. What shipped: the",
        "unattached-issue guidance and the coordinator's architecture model. What never ran: the",
        "backfill itself. The dry-run table (attachment-dry-run.md, attached here) only proposed",
        "attachments for the Go refactor's and the roadmap's trees. Next: apply that table through",
        "dispatch_issue_update, after re-checking each row against the live component model. Then",
        "read both trees back and confirm every child carries components or a not-architectural",
        "reason.",
      ].join(" "),
    },
    owner
  );
  writeFileSync(out, `${JSON.stringify({ issue: issue.key, message: message.id })}\n`);
  console.log(`seeded ${issue.key} with message ${message.id}`);
} else {
  console.error("usage: seed.ts project | ask-on-message <file>");
  process.exit(2);
}
