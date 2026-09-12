export const queryKeys = {
  artifact: (id: string | undefined) => ["artifact", id] as const,
  ask: (id: string | undefined) => ["ask", id] as const,
  askThread: (id: string | undefined) => ["ask-thread", id] as const,
  comment: (id: string | undefined) => ["comment", id] as const,
  projectArtifact: (project: string | undefined, slug: string | undefined) =>
    ["project", project, "artifact", slug] as const,
  projectArtifactPrefix: (project: string) => ["project", project, "artifact"] as const,
};
