import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useCallback, useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import { QueryError } from "../../components/QueryError";
import {
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { ArtifactDocument } from "../artifacts/ArtifactDocument";
import { ArtifactBlobView, ArtifactHeader } from "../artifacts/ArtifactHeader";
import { ReferencedBy } from "../artifacts/ArtifactsTab";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { SubscribedAgents } from "../issue/SubscribedAgents";
import { buildProjectPath, parseProjectPath } from "../refs/routes";
import { firstHighlightTerm } from "../search/search-model";
import { useDocumentTitle } from "../shell/useDocumentTitle";

export function DocumentPage(): ReactNode {
  const location = useLocation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const route = parseProjectPath(location.pathname, location.search);
  const documentRoute = route?.kind === "document" ? route : undefined;
  const [showDiff, setShowDiff] = useState(false);
  const [toolbar, setToolbar] = useState<DocumentToolbar | undefined>(undefined);
  const handleToolbarChange = useCallback((next: DocumentToolbar | undefined) => {
    setToolbar(next);
    if (next === undefined) {
      setShowDiff(false);
    }
  }, []);
  const artifact = useQuery({
    enabled: documentRoute !== undefined,
    queryKey: ["artifact-ref", `${documentRoute?.project}/${documentRoute?.slug}`],
    queryFn: () => {
      if (documentRoute === undefined) {
        throw new Error("Project document query requires a document route.");
      }
      return api.getProjectArtifact(documentRoute.project, documentRoute.slug);
    },
  });
  const subscribers = useQuery({
    enabled: artifact.data !== undefined,
    queryKey: ["subscribers", artifact.data?.id],
    queryFn: () => {
      if (artifact.data === undefined) {
        throw new Error("Document subscribers query requires an artifact.");
      }
      return api.getArtifactSubscribers(artifact.data.id);
    },
  });
  const unsubscribe = useMutation({
    mutationFn: (sessionId: string) => {
      if (artifact.data === undefined) {
        throw new Error("Document unsubscribe requires an artifact.");
      }
      return api.unsubscribeArtifactSession(artifact.data.id, sessionId);
    },
    onSuccess: () => {
      if (artifact.data !== undefined) {
        void queryClient.invalidateQueries({ queryKey: ["subscribers", artifact.data.id] });
      }
    },
  });
  const query = new URLSearchParams(location.search);
  const highlightTerm = firstHighlightTerm(query.get("q") ?? "");
  const project = documentRoute?.project;
  useDocumentTitle(
    artifact.data === undefined || project === undefined
      ? "Document · Dispatch"
      : `${artifact.data.name} · ${project} · Dispatch`
  );

  if (documentRoute === undefined) {
    return null;
  }
  if (artifact.isPending) {
    return <p className={textMutedOnCanvas}>Loading document…</p>;
  }
  if (artifact.isError) {
    if (artifact.error instanceof ApiError && artifact.error.status === 404) {
      return (
        <section>
          <h1 className={`text-xl font-semibold ${textPrimaryOnCanvas}`}>Document not found</h1>
          <Link
            className={`mt-4 inline-flex min-h-11 items-center text-sm font-medium underline ${linkText} ${linkHoverText}`}
            to={buildProjectPath({ kind: "documents", project: documentRoute.project })}
          >
            Back to {documentRoute.project} documents
          </Link>
        </section>
      );
    }
    return (
      <QueryError message="Could not load this document." onRetry={() => void artifact.refetch()} />
    );
  }
  if (artifact.data === undefined) {
    return <p className={dangerText}>Could not load this document.</p>;
  }

  const owner = {
    artifactId: artifact.data.id,
    kind: "document" as const,
    project: documentRoute.project,
    slug: documentRoute.slug,
  };
  const version = documentRoute.version;
  const item = documentRoute.item;
  const selectVersion = (nextVersion: number | null) => {
    setShowDiff(false);
    navigate(
      buildProjectPath({
        kind: "document",
        project: documentRoute.project,
        slug: documentRoute.slug,
        ...(nextVersion === null ? {} : { version: nextVersion }),
      })
    );
  };

  return (
    <section className="space-y-6">
      <ArtifactHeader
        artifact={artifact.data}
        highlight={false}
        isClosed={false}
        onShowDiffChange={setShowDiff}
        showDiff={showDiff}
        showVersionPicker
        toolbar={artifact.data.kind === "doc" ? toolbar : undefined}
        version={version}
      >
        <SubscribedAgents
          onUnsubscribe={(sessionId) => unsubscribe.mutate(sessionId)}
          ownerLabel={`${artifact.data.project}/${artifact.data.slug}`}
          subscribers={subscribers.isError ? [] : (subscribers.data ?? [])}
        />
        {subscribers.isError ? (
          <QueryError
            message="Subscribed agents unavailable — Envoy listener unreachable."
            onRetry={() => subscribers.refetch()}
            retrying={subscribers.isFetching}
          />
        ) : null}
        {unsubscribe.isError ? (
          <QueryError
            message="Could not unsubscribe this agent."
            onRetry={() => unsubscribe.mutate(unsubscribe.variables as string)}
            retrying={unsubscribe.isPending}
          />
        ) : null}
        {artifact.data.kind === "doc" ? (
          <ArtifactDocument
            artifact={artifact.data}
            askId={item?.kind === "ask" ? item.id : undefined}
            commentId={item?.kind === "comment" ? item.id : undefined}
            highlightTerm={highlightTerm}
            isClosed={false}
            onToolbarChange={handleToolbarChange}
            onVersionChange={selectVersion}
            owner={owner}
            showDiff={showDiff}
            version={version}
          />
        ) : (
          <ArtifactBlobView artifact={artifact.data} version={version} />
        )}
      </ArtifactHeader>
      <ReferencedBy references={artifact.data.referenced_by} />
    </section>
  );
}
