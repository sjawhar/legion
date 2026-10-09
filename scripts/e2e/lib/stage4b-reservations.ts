// The daemon's default reservations, transcribed from config.DefaultResources()
// (packages/daemon/internal/config/kubernetes.go): each role's cpu and memory when the run
// overrides none, summing to 3 CPU and 15 GiB an issue pod, and its ephemeral-storage limit
// (`ephemeral_storage`, the role's bound on the node's disk: 20Gi for the two roles that build,
// 10Gi for the rest) over its request (`ephemeral_storage_request`, 1Gi for every role). Spread it
// to override a role; never mutate it.
export interface Reservation {
  cpu: string;
  memory: string;
  ephemeral_storage: string;
  ephemeral_storage_request: string;
}
export const defaults: Record<string, Reservation> = {
  architect: {
    cpu: "250m",
    memory: "1Gi",
    ephemeral_storage: "10Gi",
    ephemeral_storage_request: "1Gi",
  },
  planner: {
    cpu: "250m",
    memory: "1Gi",
    ephemeral_storage: "10Gi",
    ephemeral_storage_request: "1Gi",
  },
  implementer: {
    cpu: "750m",
    memory: "4Gi",
    ephemeral_storage: "20Gi",
    ephemeral_storage_request: "1Gi",
  },
  tester: {
    cpu: "750m",
    memory: "4Gi",
    ephemeral_storage: "20Gi",
    ephemeral_storage_request: "1Gi",
  },
  reviewer: {
    cpu: "750m",
    memory: "4Gi",
    ephemeral_storage: "10Gi",
    ephemeral_storage_request: "1Gi",
  },
  merger: {
    cpu: "250m",
    memory: "1Gi",
    ephemeral_storage: "10Gi",
    ephemeral_storage_request: "1Gi",
  },
  controller: {
    cpu: "1",
    memory: "4Gi",
    ephemeral_storage: "10Gi",
    ephemeral_storage_request: "1Gi",
  },
};
