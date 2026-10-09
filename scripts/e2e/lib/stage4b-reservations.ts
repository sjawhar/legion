// The daemon's default reservations, transcribed from config.DefaultResources()
// (packages/daemon/internal/config/kubernetes.go): each role's cpu and memory when the run
// overrides none, summing to 3 CPU and 15 GiB an issue pod. Spread it to override a role; never
// mutate it.
export const defaults: Record<string, { cpu: string; memory: string }> = {
  architect: { cpu: "250m", memory: "1Gi" },
  planner: { cpu: "250m", memory: "1Gi" },
  implementer: { cpu: "750m", memory: "4Gi" },
  tester: { cpu: "750m", memory: "4Gi" },
  reviewer: { cpu: "750m", memory: "4Gi" },
  merger: { cpu: "250m", memory: "1Gi" },
  controller: { cpu: "1", memory: "4Gi" },
};
