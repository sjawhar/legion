import { queryOptions } from "@tanstack/react-query";

import { api } from "../../api/client";

/** The live document's text as the server reads it now. A read of it decides whether the editor
 * may connect: the server answers `409 DOC_SCHEMA` for a stored tree outside the Proof schema.
 * It is also the whole of what the document's first draw needs (`DocumentPreview`), so it lives
 * here rather than in `ProofDocument.tsx`: the shell starts it the moment the address is known
 * (`open-document-early.ts`), and the shell must not pull the editor's own module in to do it. */
export const liveTextQueryOptions = (artifactId: string) =>
  queryOptions({
    queryKey: ["artifact", artifactId, "text"],
    queryFn: () => api.getArtifactText(artifactId),
  });
