import { type UseQueryResult, useQuery } from "@tanstack/react-query";

import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import type { ProjectDocumentRoute } from "../refs/routes";

/**
 * The project document a route names, by `project/slug`. Disabled without a document route; the
 * `["artifact-ref", …]` key is the one the event stream invalidates when a document changes, so
 * every caller shares one cache entry.
 */
export function useProjectArtifact(
  route: ProjectDocumentRoute | undefined
): UseQueryResult<Artifact> {
  return useQuery({
    enabled: route !== undefined,
    queryKey: ["artifact-ref", `${route?.project}/${route?.slug}`],
    queryFn: () => {
      if (route === undefined) {
        throw new Error("Project document query requires a document route.");
      }
      return api.getProjectArtifact(route.project, route.slug);
    },
  });
}
