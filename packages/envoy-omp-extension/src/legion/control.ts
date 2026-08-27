import { LEGION_ROLES, type LegionRole } from "@legion/contracts";
import type { ExtensionAgentsApi } from "./pi-types";

export type Redelivery = {
  readonly topic: string;
  readonly payload: string;
  readonly eventId: string;
};

export type LegionControlDirective =
  | {
      readonly type: "revive-worker";
      readonly role: LegionRole;
      readonly agentId: string;
      readonly parentSessionFile: string;
      readonly redeliver: Redelivery;
    }
  | { readonly type: "reclaim-architect"; readonly redeliver: Redelivery }
  | { readonly type: "shutdown" };

export type LegionControlActions = {
  readonly agents?: ExtensionAgentsApi;
  readonly reclaimArchitect: () => Promise<void>;
  readonly requestShutdown: () => void;
  readonly acknowledge: () => void;
  readonly reject: (error: string) => void;
};

export async function handleLegionControlDirective(
  directive: LegionControlDirective,
  actions: LegionControlActions
): Promise<void> {
  try {
    switch (directive.type) {
      case "revive-worker":
        if (!actions.agents) {
          throw new Error("reviving Legion workers requires an OMP host with pi.agents");
        }
        await actions.agents.ensureLive(directive.agentId, {
          parentSessionFile: directive.parentSessionFile,
        });
        break;
      case "reclaim-architect":
        await actions.reclaimArchitect();
        break;
      case "shutdown":
        actions.requestShutdown();
        break;
    }
    actions.acknowledge();
  } catch (error) {
    actions.reject(error instanceof Error ? error.message : String(error));
  }
}

export function parseControlDirective(raw: string): LegionControlDirective {
  const payload: unknown = JSON.parse(raw);
  if (typeof payload !== "object" || payload === null || !("type" in payload)) {
    throw new Error("Legion control directive must be an object with a type");
  }
  const redelivery = (): Redelivery => {
    if (
      !("redeliver" in payload) ||
      typeof payload.redeliver !== "object" ||
      payload.redeliver === null ||
      !("topic" in payload.redeliver) ||
      typeof payload.redeliver.topic !== "string" ||
      !("payload" in payload.redeliver) ||
      typeof payload.redeliver.payload !== "string" ||
      !("eventId" in payload.redeliver) ||
      typeof payload.redeliver.eventId !== "string"
    ) {
      throw new Error("Legion control directive is missing redelivery metadata");
    }
    return {
      topic: payload.redeliver.topic,
      payload: payload.redeliver.payload,
      eventId: payload.redeliver.eventId,
    };
  };
  if (payload.type === "shutdown") return { type: "shutdown" };
  if (payload.type === "reclaim-architect")
    return { type: "reclaim-architect", redeliver: redelivery() };
  if (
    payload.type !== "revive-worker" ||
    !("role" in payload) ||
    typeof payload.role !== "string" ||
    !LEGION_ROLES.includes(payload.role as LegionRole) ||
    !("agentId" in payload) ||
    typeof payload.agentId !== "string" ||
    !("parentSessionFile" in payload) ||
    typeof payload.parentSessionFile !== "string"
  ) {
    throw new Error("Invalid Legion control directive");
  }
  return {
    type: "revive-worker",
    role: payload.role as LegionRole,
    agentId: payload.agentId,
    parentSessionFile: payload.parentSessionFile,
    redeliver: redelivery(),
  };
}
