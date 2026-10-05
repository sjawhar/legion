import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { ApiError } from "../../api/client";
import { QueryError } from "../../components/QueryError";
import {
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";

/** The part of an artifact query its page's loading and failure states read. */
interface ArtifactQueryState {
  error: unknown;
  isPending: boolean;
  refetch: () => unknown;
}

export interface ArtifactQueryStatesProps {
  query: ArtifactQueryState;
  /** Shown while the artifact loads, e.g. `Loading document…`. */
  loadingLabel: string;
  /** The heading when the server answers 404, e.g. `Document not found`. */
  notFoundLabel: string;
  /** Where the not-found state's link leads: the list the artifact would have been in. */
  notFoundTo: string;
  notFoundLinkText: string;
  /** Any other failure's message, offered with a Retry. */
  errorMessage: string;
}

/**
 * What an artifact's page shows in place of the artifact while its query is pending or has
 * failed: a loading line, a not-found heading with a way back when the server answers 404, and
 * any other failure with a Retry. The page renders it only in those two states; past them its
 * query holds the artifact.
 */
export function ArtifactQueryStates({
  query,
  loadingLabel,
  notFoundLabel,
  notFoundTo,
  notFoundLinkText,
  errorMessage,
}: ArtifactQueryStatesProps): ReactNode {
  if (query.isPending) {
    return <p className={textMutedOnCanvas}>{loadingLabel}</p>;
  }
  if (query.error instanceof ApiError && query.error.status === 404) {
    return (
      <section>
        <h1 className={`text-xl font-semibold ${textPrimaryOnCanvas}`}>{notFoundLabel}</h1>
        <Link
          className={`mt-4 inline-flex min-h-11 items-center text-sm font-medium underline ${linkText} ${linkHoverText}`}
          to={notFoundTo}
        >
          {notFoundLinkText}
        </Link>
      </section>
    );
  }
  return <QueryError message={errorMessage} onRetry={() => void query.refetch()} />;
}
