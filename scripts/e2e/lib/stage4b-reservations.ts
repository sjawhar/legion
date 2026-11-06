// The daemon's default reservations, transcribed from config.DefaultResources()
// (packages/daemon/internal/config/kubernetes.go): each pod's cpu and memory when the run overrides
// none (an issue pod 3 CPU and 19 GiB, every container of it carrying that one reservation; the
// controller's pod 1 CPU and 4 GiB), and its ephemeral-storage limit (`ephemeral_storage`, the pod's
// bound on the node's disk) over its request (`ephemeral_storage_request`). Spread it to override a
// pod; never mutate it.
export interface Reservation {
  cpu: string;
  memory: string;
  ephemeral_storage: string;
  ephemeral_storage_request: string;
}
export const defaults: Record<"issue" | "controller", Reservation> = {
  issue: {
    cpu: "3",
    memory: "19Gi",
    ephemeral_storage: "80Gi",
    ephemeral_storage_request: "6Gi",
  },
  controller: {
    cpu: "1",
    memory: "4Gi",
    ephemeral_storage: "10Gi",
    ephemeral_storage_request: "1Gi",
  },
};
