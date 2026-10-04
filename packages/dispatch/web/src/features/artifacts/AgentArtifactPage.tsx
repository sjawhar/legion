import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, useLocation } from "react-router-dom";

import { ApiError } from "../../api/client";
import { QueryError } from "../../components/QueryError";
import {
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { parseAgentArtifactPath } from "../refs/routes";
import { agentArtifactQuery } from "../refs/Unfurl";
import { NotFoundPage } from "../shell/NotFoundPage";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { ArtifactDetails } from "./ArtifactDetails";
import { ArtifactBlobView, ArtifactHeader } from "./ArtifactHeader";

/**
 * An artifact of an agent's conversation - a picture someone pasted into a direct message on the
 * Agents page, or one the agent sent back - at `/agents/<session id>/artifacts/<slug>[?v=N]`, in
 * the shape of an issue artifact's page: the picture (a file's name and size otherwise) at the
 * picked version, then its versions. Clearing the conversation hides the message that carried it,
 * never this page.
 */
export function AgentArtifactPage(): ReactNode {
  const location = useLocation();
  const route = parseAgentArtifactPath(location.pathname, location.search);
  const artifact = useQuery({
    ...agentArtifactQuery(route?.session, route?.slug),
    enabled: route !== undefined,
  });
  useDocumentTitle(
    artifact.data === undefined ? "Artifact · Dispatch" : `${artifact.data.name} · Dispatch`
  );

  if (route === undefined) {
    return <NotFoundPage backLabel="Back to agents" backTo="/agents" />;
  }
  if (artifact.isPending) {
    return <p className={textMutedOnCanvas}>Loading artifact…</p>;
  }
  if (artifact.isError) {
    if (artifact.error instanceof ApiError && artifact.error.status === 404) {
      return (
        <section>
          <h1 className={`text-xl font-semibold ${textPrimaryOnCanvas}`}>Artifact not found</h1>
          <Link
            className={`mt-4 inline-flex min-h-11 items-center text-sm font-medium underline ${linkText} ${linkHoverText}`}
            to="/agents"
          >
            Back to agents
          </Link>
        </section>
      );
    }
    return (
      <QueryError message="Could not load this artifact." onRetry={() => void artifact.refetch()} />
    );
  }
  return (
    <section className="space-y-4">
      <ArtifactHeader
        artifact={artifact.data}
        highlight={false}
        showVersionPicker
        version={route.version}
      >
        <ArtifactBlobView artifact={artifact.data} version={route.version} />
      </ArtifactHeader>
      <ArtifactDetails artifact={artifact.data} />
    </section>
  );
}
