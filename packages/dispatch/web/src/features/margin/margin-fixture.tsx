import type { ReactNode } from "react";
import { useNavigate } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import type { Artifact, Ask, Comment, IssueDetails } from "../../api/types";
import { buildIssuePath } from "../refs/routes";
import { useMargin } from "./margin-context";

export const specArtifact: Artifact = {
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: "CORE-1",
  project: "CORE",
  kind: "doc",
  name: "spec.md",
  primary: true,
  slug: "spec",
  versions: [],
};

export const issue: IssueDetails = {
  artifacts: [specArtifact],
  children: [],
  closed_at: null,
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 1,
  number: 1,
  parent: null,
  assignee: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  primary_artifact_id: "artifact-1",
  project: "CORE",
  referenced_by_count: 0,
  route: null,
  status: "open",
  priority: null,
  rank: "U",
  title: "Review the spec",
  open_asks: [],
  updated_at: "2026-09-09T00:00:00Z",
};

export const comment: Comment = {
  anchor: {
    artifact_id: "artifact-1",
    block_id: null,
    mark_id: "m-1",
    orphaned: false,
    quote: "brown",
    version: 1,
  },
  author: { id: "alice", kind: "user" },
  body: "why?",
  created_at: "2026-09-09T00:00:00Z",
  id: "comment-1",
  ask_id: null,
  turn: null,
  issue_key: "CORE-1",
  reply_to: null,
  resolved: false,
  resolved_by: null,
  resolved_at: null,
  edited_at: null,
  suggestion: null,
  ...commentDeliveryFields(),
};

export const anchoredAsk: Ask = {
  anchor: {
    artifact_id: "artifact-1",
    block_id: null,
    mark_id: "m-2",
    orphaned: false,
    quote: "Review",
    version: 1,
  },
  answer: null,
  author: { id: "session-1", kind: "session" },
  created_at: "2026-09-09T00:00:00Z",
  edited_at: null,
  id: "ask-1",
  issue_key: "CORE-1",
  kind: "question",
  multiple: false,
  opened_event_id: 1,
  options: [{ label: "Ship" }],
  question: "Should this ship?",
  state: "open",
  urgency: "med",
};

export function CommentLink(): ReactNode {
  const navigate = useNavigate();

  return (
    <button
      onClick={() => navigate(buildIssuePath({ id: "comment-1", key: "CORE-1", kind: "comment" }))}
      type="button"
    >
      Open comment
    </button>
  );
}

/** A link to the very route the landing tests start on, so following it is a same-URL push. */
export function SameCommentLink(): ReactNode {
  const navigate = useNavigate();

  return (
    <button
      onClick={() =>
        navigate(`${buildIssuePath({ key: "CORE-1", kind: "spec" })}?comment=comment-1`)
      }
      type="button"
    >
      Open the same comment
    </button>
  );
}

/** Stands in for the open document reporting its layout. A document with no live mark and no
 *  typed ask block reports empty maps, which is still an answer. */
export function ReportEmptyLayoutButton(): ReactNode {
  const { setBlockPlacements, setMarkPlacements } = useMargin();

  return (
    <button
      onClick={() => {
        setMarkPlacements(new Map());
        setBlockPlacements(new Map());
      }}
      type="button"
    >
      Report layout
    </button>
  );
}

export function SelectedItemLabel(): ReactNode {
  const { selectedItemId } = useMargin();

  return <output aria-label="Selected margin item">{selectedItemId ?? "none"}</output>;
}

export function stubMatchMedia(matches: boolean): () => void {
  const original = window.matchMedia;
  window.matchMedia = (() =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches,
      media: "",
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  return () => {
    window.matchMedia = original;
  };
}
