import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact, Event } from "../../api/types";
import { linkText } from "../../theme/classes";
import { buildIssuePath, buildReferencePath, documentRoute } from "../refs/routes";

type ArtifactVersionEvent = Extract<Event, { type: "artifact.version" }>;

/** The activity text with the artifact's name turned into a link, so the reader opens the
 *  upload from the Conversation instead of hunting for it on the Artifacts tab. The text itself
 *  comes from `activityDescription`, which names the artifact once. */
function LinkedName({
  description,
  name,
  to,
}: {
  description: string;
  name: string;
  to: string;
}): ReactNode {
  const at = description.indexOf(name);
  if (at === -1) {
    return <span>{description}</span>;
  }
  return (
    <span>
      {description.slice(0, at)}
      <Link className={`underline ${linkText}`} to={to}>
        {name}
      </Link>
      {description.slice(at + name.length)}
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
  description: string;
  event: ArtifactVersionEvent;
  issueKey: string;
}): ReactNode {
  const issue = useQuery({ queryKey: ["issue", issueKey], queryFn: () => api.getIssue(issueKey) });
  const artifact: Artifact | undefined = issue.data?.artifacts.find(
    ({ id }) => id === event.payload.artifact_id
  );
  if (artifact === undefined) {
    return <span>{description}</span>;
  }
  return (
    <LinkedName
      description={description}
      name={event.payload.name}
      to={buildReferencePath(documentRoute(artifact, event.payload.version.number))}
    />
  );
}

export function ActivityLine({
  description,
  event,
  issueKey,
}: {
  description: string;
  event: Event;
  issueKey: string;
}): ReactNode {
  switch (event.type) {
    case "artifact.created":
      return (
        <LinkedName
          description={description}
          name={event.payload.artifact.name}
          to={buildReferencePath(documentRoute(event.payload.artifact))}
        />
      );
    case "artifact.version":
      return <SavedVersionLine description={description} event={event} issueKey={issueKey} />;
    case "comment.created":
      return (
        <>
          <span>{description}</span>
          <Link
            className={`underline ${linkText}`}
            to={buildIssuePath({ id: event.payload.id, key: issueKey, kind: "comment" })}
          >
            view
          </Link>
        </>
      );
    default:
      return <span>{description}</span>;
  }
}
