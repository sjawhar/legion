import { describe, expect, test } from "bun:test";
import { fileURLToPath } from "node:url";

const library = fileURLToPath(new URL(".", import.meta.url));
const program = fileURLToPath(new URL("./design-gate-verdict.jq", import.meta.url));

interface Verdict {
  request: string | null;
  summarized: boolean;
  blocks: number;
  early: string[];
}

interface Version {
  number: number;
  markdown: string;
}

interface ApprovalAskFixture {
  id?: string;
  kind?: string;
  state?: string;
  created_at?: string;
  approval?: {
    artifact_id: string;
    name: string;
    version: number;
    requested_version?: number;
  };
}

function approvalEvents(asks: unknown[]): unknown[] {
  return asks.flatMap((value, index) => {
    const ask = value as ApprovalAskFixture;
    if (ask.kind !== "approval" || ask.approval === undefined) return [];
    const payload = {
      ...ask,
      id: ask.id ?? `approval-${index}`,
      approval: {
        ...ask.approval,
        requested_version: ask.approval.requested_version ?? ask.approval.version,
      },
    };
    const events: unknown[] = [{ type: "ask.opened", created_at: ask.created_at, payload }];
    if (ask.state === "answered") {
      events.push({ type: "ask.answered", created_at: ask.created_at, payload });
    }
    return events;
  });
}

/**
 * Runs the stage 4b design-gate verdict as the driver does: the issue's asks, approval lifecycle
 * events, then each spec version that an approval ask was handed back at, slurped.
 */
function verdict(
  asks: unknown[],
  versions: Version[],
  approved = 5,
  events = approvalEvents(asks)
): Verdict {
  const run = Bun.spawnSync(
    [
      "jq",
      "-c",
      "-s",
      "-L",
      library,
      "--arg",
      "artifact",
      "spec-1",
      "--argjson",
      "version",
      String(approved),
      "-f",
      program,
    ],
    {
      stdin: new TextEncoder().encode(
        [asks, events, ...versions].map((value) => JSON.stringify(value)).join("\n")
      ),
    }
  );
  if (run.exitCode !== 0) throw new Error(`jq exited ${run.exitCode}: ${run.stderr.toString()}`);
  return JSON.parse(run.stdout.toString());
}

const block = {
  kind: "question",
  block_id: "block-1",
  block_artifact: { id: "spec-1" },
  question: "Where does the smoke file go?",
  created_at: "2026-09-30T10:00:00.5Z",
  answer: { at: "2026-09-30T10:05:00.123456789Z" },
};
const approval = (version: number, createdAt: string, state: string) => ({
  kind: "approval",
  block_id: null,
  approval: { artifact_id: "spec-1", name: "spec.md", version },
  question: `Approve spec.md (version ${version})? Proposes the file under docs/smoke/.`,
  created_at: createdAt,
  state,
});
// A spec version as Dispatch stores it: the block, when the version holds it, carries its id and
// its state in its attributes.
const specAt = (number: number, state: "open" | "answered" | null): Version => {
  const attributes =
    state === "open"
      ? 'state="open"'
      : 'state="answered" answered_by="alice" answered_at="2026-09-30T10:05:00.123456789Z" selected="[&#x22;Docs&#x22;]"';
  const blockText = `\n:::ask{#block-1 urgency="med" multiple="false" ${attributes}}\nWhere does the smoke file go?\n\n- Root: under smoke/.\n- Docs: under docs/smoke/.\n:::\n`;
  return {
    number,
    markdown: `## The smoke file\n\nWhere it goes is the human's choice.\n${state === null ? "" : blockText}`,
  };
};

describe("design-gate-verdict.jq", () => {
  test("a summarized request at a version whose blocks are answered passes, and another document's asks are ignored", () => {
    const elsewhere = {
      ...approval(2, "2026-09-30T10:03:00Z", "open"),
      approval: { artifact_id: "other", name: "notes.md", version: 2 },
    };
    expect(
      verdict(
        [block, elsewhere, approval(5, "2026-09-30T10:06:00.7Z", "answered")],
        [specAt(5, "answered")]
      )
    ).toEqual({
      request: "Approve spec.md (version 5)? Proposes the file under docs/smoke/.",
      summarized: true,
      blocks: 1,
      early: [],
    });
  });

  test("counts each version an approval row was handed back at from its event history", () => {
    const current = {
      ...approval(2, "2026-09-30T10:06:00Z", "answered"),
      id: "approval-1",
      approval: { artifact_id: "spec-1", name: "spec.md", version: 2, requested_version: 2 },
      question: "Approve spec.md (version 2)? Adds the rollback budget.",
    };
    const previous = {
      question: "Approve spec.md (version 1)? Proposes the file under docs/smoke/.",
      options: [],
      multiple: false,
      urgency: "high",
    };
    const events = [
      {
        type: "ask.opened",
        created_at: "2026-09-30T10:00:00Z",
        payload: {
          ...current,
          approval: {
            artifact_id: "spec-1",
            name: "spec.md",
            version: 1,
            requested_version: 1,
          },
          question: "Approve spec.md (version 1)? Proposes the file under docs/smoke/.",
        },
      },
      {
        type: "ask.edited",
        created_at: "2026-09-30T10:01:00Z",
        payload: {
          ...current,
          approval: {
            artifact_id: "spec-1",
            name: "spec.md",
            version: 2,
            requested_version: 1,
          },
          previous,
          question: "Approve spec.md (version 2)? Proposes the file under docs/smoke/.",
        },
      },
      // The hand-back with a new summary: its rewording, then the hand-back itself.
      {
        type: "ask.edited",
        created_at: "2026-09-30T10:06:00Z",
        payload: {
          ...current,
          approval: { ...current.approval, requested_version: 1 },
          previous: {
            ...previous,
            question: "Approve spec.md (version 2)? Proposes the file under docs/smoke/.",
          },
        },
      },
      {
        type: "ask.handed_back",
        created_at: "2026-09-30T10:06:00Z",
        payload: current,
      },
      {
        type: "ask.answered",
        created_at: "2026-09-30T10:07:00Z",
        payload: current,
      },
    ];
    expect(
      verdict([block, current], [specAt(1, "open"), specAt(2, "answered")], 2, events)
    ).toEqual({
      request: "Approve spec.md (version 2)? Adds the rollback budget.",
      summarized: true,
      blocks: 1,
      early: ["version 1: Where does the smoke file go?"],
    });
  });

  // The version-3 request was retracted when the block's answer wrote version 4, and the approved
  // request at version 5 came after the answer; the early one still counts.
  test("an early request is caught though it was retracted and a later one was approved", () => {
    const asks = [
      block,
      approval(3, "2026-09-30T10:02:00Z", "resolved"),
      approval(5, "2026-09-30T10:06:00.7Z", "answered"),
    ];
    expect(verdict(asks, [specAt(3, "open"), specAt(5, "answered")]).early).toEqual([
      "version 3: Where does the smoke file go?",
    ]);
  });

  // Dispatch indexes a block as an ask when it settles the document, 2 s after the edit that wrote
  // it by default: an architect that sends the edit and the approval request in one turn requests
  // before the block's ask exists. No ordering of times decides it; the version does.
  test("a request is early when its version holds a block open, whenever the block's ask was indexed or answered", () => {
    const indexedLater = { ...block, created_at: "2026-09-30T10:00:02.009Z" };
    expect(
      verdict(
        [indexedLater, approval(2, "2026-09-30T10:00:00.018Z", "resolved")],
        [specAt(2, "open")],
        2
      ).early
    ).toEqual(["version 2: Where does the smoke file go?"]);
    const sameSecondWrite = { ...block, created_at: "2026-09-30T10:00:00.2Z", answer: null };
    expect(
      verdict([sameSecondWrite, approval(5, "2026-09-30T10:00:00.8Z", "open")], [specAt(5, "open")])
        .early
    ).toEqual(["version 5: Where does the smoke file go?"]);
    const sameSecondAnswer = {
      ...block,
      created_at: "2026-09-30T09:59:00Z",
      answer: { at: "2026-09-30T10:00:00.9Z" },
    };
    expect(
      verdict(
        [sameSecondAnswer, approval(5, "2026-09-30T10:00:00.1Z", "answered")],
        [specAt(5, "open")]
      ).early
    ).toEqual(["version 5: Where does the smoke file go?"]);
  });

  // A request made before a choice became an ask block is early once a human answers that choice.
  test("a request made before the choice was asked as a block is early", () => {
    const asks = [
      block,
      approval(1, "2026-09-30T09:58:00Z", "resolved"),
      approval(3, "2026-09-30T10:06:00Z", "answered"),
    ];
    expect(verdict(asks, [specAt(1, null), specAt(3, "answered")], 3)).toMatchObject({
      blocks: 1,
      early: ["version 1: Where does the smoke file go?"],
    });
  });

  // The request reached the server before the edit adding the block committed, so it names the
  // version before the block, and the block's ask was indexed after both.
  test("a request sent in parallel with the edit that wrote the block is early", () => {
    const parallel = {
      ...block,
      created_at: "2026-09-30T10:00:02.1Z",
      answer: { at: "2026-09-30T10:03:00.4Z" },
    };
    const asks = [
      parallel,
      approval(1, "2026-09-30T10:00:00.1Z", "resolved"),
      approval(3, "2026-09-30T10:04:00Z", "answered"),
    ];
    expect(verdict(asks, [specAt(1, null), specAt(3, "answered")], 3).early).toEqual([
      "version 1: Where does the smoke file go?",
    ]);
  });

  // The flow legion-architect prescribes: the human answers the version-3 request with Request
  // changes, the revision raises a second block, the human answers it, and the architect requests
  // again. The version-3 request was answered, so only its version judges it.
  test("a request the human answered with Request changes is not early for a block the revision raised", () => {
    const raised = {
      ...block,
      block_id: "block-2",
      question: "Which date format?",
      created_at: "2026-09-30T10:10:00Z",
      answer: { at: "2026-09-30T10:15:00Z" },
    };
    const asks = [
      block,
      approval(3, "2026-09-30T10:06:00Z", "answered"),
      raised,
      approval(6, "2026-09-30T10:16:00Z", "answered"),
    ];
    expect(verdict(asks, [specAt(3, "answered"), specAt(6, "answered")], 6)).toMatchObject({
      blocks: 2,
      early: [],
    });
  });

  // Dispatch writes a list item that opens with code on its marker line, so a fence reader takes
  // that item's closing fence for an opener and would hide every block after it.
  test("a block after a list item that opens with code is still read", () => {
    const listed = specAt(5, "open");
    listed.markdown = `## Setup\n\n- \`\`\`sh\n  make smoke\n  \`\`\`\n${listed.markdown}`;
    expect(
      verdict([block, approval(5, "2026-09-30T10:06:00Z", "answered")], [listed]).early
    ).toEqual(["version 5: Where does the smoke file go?"]);
  });

  test("a block quoted in a fenced code block is not an open block", () => {
    const quoted = specAt(5, "answered");
    quoted.markdown +=
      '\nAn open block is written:\n\n```md\n:::ask{#example urgency="med" multiple="false" state="open"}\nShip it?\n:::\n```\n';
    expect(
      verdict([block, approval(5, "2026-09-30T10:06:00Z", "answered")], [quoted]).early
    ).toEqual([]);
  });

  test("only the spec's blocks a human answered count: another document's, a retracted one and one naming no document do not", () => {
    const elsewhere = { ...block, block_id: "block-2", block_artifact: { id: "other" } };
    const retracted = {
      ...block,
      block_id: "block-3",
      answer: null,
      resolution: { kind: "retracted", at: "2026-09-30T10:01:00Z" },
    };
    const { block_artifact: _, ...unattached } = { ...block, block_id: "block-4" };
    expect(
      verdict(
        [elsewhere, retracted, unattached, approval(5, "2026-09-30T10:02:00Z", "answered")],
        [specAt(5, null)]
      )
    ).toMatchObject({ blocks: 0, early: [] });
  });

  test("a request without a summary, or none at the approved version, is reported", () => {
    const bare = {
      ...approval(5, "2026-09-30T10:06:00Z", "answered"),
      question: "Approve spec.md (version 5)?",
    };
    expect(verdict([block, bare], [specAt(5, "answered")])).toMatchObject({
      request: "Approve spec.md (version 5)?",
      summarized: false,
    });
    expect(
      verdict([block, approval(4, "2026-09-30T10:06:00Z", "answered")], [specAt(4, "answered")])
    ).toMatchObject({
      request: null,
      summarized: false,
    });
  });

  test("a requested version the driver did not read fails the verdict rather than passing it", () => {
    expect(() => verdict([block, approval(5, "2026-09-30T10:06:00Z", "answered")], [])).toThrow(
      "version 5 of the spec was not read"
    );
  });
});
