import { dispatchDocumentSubject, dispatchIssueSubject } from "@legion/contracts";

/**
 * Where an owner's events go and how a session names it: the label agents read in tool
 * results and the wildcard `envoy_subscribe` takes. No write subscribes a session to it;
 * whole-owner subscription is the agent's explicit choice (D3), so every write says so.
 */
export interface OwnerTopic {
  readonly label: string;
  readonly topic: string;
}

/** How tool results name a project document: `<project>/<slug>`. */
export function documentLabel(project: string, slug: string): string {
  return `${project}/${slug}`;
}

export function issueTopic(key: string): OwnerTopic {
  return { label: key, topic: dispatchIssueSubject(key, ">") };
}

export function documentTopicOf(project: string, slug: string): OwnerTopic {
  return {
    label: documentLabel(project, slug),
    topic: dispatchDocumentSubject(project, slug, ">"),
  };
}

/** The `dispatch://` address of an issue. */
export function dispatchIssueRef(key: string): string {
  return `dispatch://${key}`;
}

/** The `dispatch://` address of a document, `owner` being the issue key or project key it lives under. */
export function dispatchDocumentRef(owner: string, slug: string): string {
  return `dispatch://${owner}/artifact/${slug}`;
}

/** The `dispatch://` address of an ask, comment, or message under an owner's address. */
export function dispatchChildRef(
  ownerRef: string,
  kind: "ask" | "comment" | "message",
  id: string
): string {
  return `${ownerRef}/${kind}/${id}`;
}
