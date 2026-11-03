import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { issueQuery } from "../../api/queries";
import type { Artifact, Event } from "../../api/types";
import { linkText } from "../../theme/classes";
import { type AgentPart, AgentParts } from "../refs/AgentName";
import { MarkdownPreview } from "../refs/MarkdownPreview";
import { buildIssuePath, buildReferencePath, documentRoute } from "../refs/routes";

type ArtifactVersionEvent = Extract<Event, { type: "artifact.version" }>;

/** An activity line's parts as this tab draws them: a question the line quotes is formatted and
 *  cut to the one line the row gives it, and with `artifact` the artifact's name is a link to
 *  it. */
function LineParts({
  artifact,
  description,
}: {
  artifact?: (name: string) => ReactNode;
  description: readonly AgentPart[];
}): ReactNode {
  return (
    <AgentParts
      artifact={artifact}
      parts={description}
      quote={(markdown) => <MarkdownPreview className="min-w-0" lines={1} markdown={markdown} />}
    />
  );
}

/** The activity text with the artifact's name turned into a link, so the reader opens the
 *  upload from the Conversation instead of hunting for it on the Artifacts tab. The name is a
 *  part of its own (`{ artifact }`, from `activityDescription`), so the link is that part: the
 *  sentence's text is never searched for the name, which a quoted question in the same line
 *  could otherwise hold. */
function LinkedName({
  description,
  to,
}: {
  description: readonly AgentPart[];
  to: string;
}): ReactNode {
  return (
    <span>
      <LineParts
        artifact={(name) => (
          <Link className={`dispatch-inline-link underline ${linkText}`} to={to}>
            {name}
          </Link>
        )}
        description={description}
      />
    </span>
  );
}

/** `artifact.version` carries the artifact's id and name but not its slug; the issue query
 *  (already loaded by the page) supplies the artifact the version belongs to. */
function SavedVersionLine({
  description,
  event,
  issueKey,
}: {
  description: readonly AgentPart[];
  event: ArtifactVersionEvent;
  issueKey: string;
}): ReactNode {
  const issue = useQuery(issueQuery(issueKey));
  const artifact: Artifact | undefined = issue.data?.artifacts.find(
    ({ id }) => id === event.payload.artifact_id
  );
  if (artifact === undefined) {
    return (
      <span>
        <LineParts description={description} />
      </span>
    );
  }
  return (
    <LinkedName
      description={description}
      to={buildReferencePath(documentRoute(artifact, event.payload.version.number))}
    />
  );
}

export function ActivityLine({
  description,
  event,
  issueKey,
}: {
  description: readonly AgentPart[];
  event: Event;
  issueKey: string;
}): ReactNode {
  switch (event.type) {
    case "artifact.created":
      return (
        <LinkedName
          description={description}
          to={buildReferencePath(documentRoute(event.payload.artifact))}
        />
      );
    case "artifact.version":
      return <SavedVersionLine description={description} event={event} issueKey={issueKey} />;
    case "comment.created":
      return (
        <>
          <span>
            <LineParts description={description} />
          </span>
          <Link
            className={`dispatch-inline-link underline ${linkText}`}
            to={buildIssuePath({ id: event.payload.id, key: issueKey, kind: "comment" })}
          >
            view
          </Link>
        </>
      );
    default:
      return (
        <span>
          <LineParts description={description} />
        </span>
      );
  }
}
