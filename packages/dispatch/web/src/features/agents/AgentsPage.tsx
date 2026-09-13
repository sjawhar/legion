import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";

import { api } from "../../api/client";
import type { Agent } from "../../api/types";
import {
  borderDefault,
  card,
  connectionDotConnecting,
  dangerText,
  inputClasses,
  liveDotBg,
  offlineDotBg,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  surfaceMutedBg,
  surfaceMutedStrongBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { ConversationComposer } from "../conversation/ConversationComposer";
import { useAgents } from "../conversation/useAgents";
import { waitingOnYou } from "../inbox/BlockedOnYou";
import { Timestamp } from "../refs/Timestamp";
import { useDocumentTitle } from "../shell/useDocumentTitle";

type Delivery = "btw" | "steer";

function freshness(agent: Agent): { dot: string; label: string } {
  const age = Date.now() - agent.last_seen;
  if (age < 2 * 60_000) return { dot: liveDotBg, label: "Seen less than 2 minutes ago" };
  if (age < 10 * 60_000)
    return { dot: connectionDotConnecting, label: "Seen less than 10 minutes ago" };
  return { dot: offlineDotBg, label: "Seen 10 minutes ago or longer" };
}

function FreshnessDot({ agent }: { agent: Agent }): ReactNode {
  const status = freshness(agent);
  return (
    <span className="inline-flex shrink-0 items-center" role="status" title={status.label}>
      <span aria-hidden className={`h-2.5 w-2.5 rounded-full ${status.dot}`} />
      <span className="sr-only">{status.label}</span>
    </span>
  );
}

function AgentMessageComposer({
  agent,
  delivery,
  onClose,
}: {
  agent: Agent;
  delivery: Delivery;
  onClose: () => void;
}): ReactNode {
  const [issueKey, setIssueKey] = useState("");
  const issues = useQuery({
    queryFn: () => api.listIssues(),
    queryKey: ["issues", "agent-composer"],
  });
  const label = agent.title.trim() === "" ? agent.session_id : agent.title;

  return (
    <div className={`mt-3 border-t pt-3 ${borderDefault}`}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className={`text-sm font-semibold ${textPrimaryOnCanvas}`}>
          Send {delivery === "btw" ? "BTW" : "Steer"} to {label}
        </h3>
        <button
          aria-label={`Close ${delivery === "btw" ? "BTW" : "Steer"} composer for ${label}`}
          className={`min-h-11 rounded-lg px-3 text-sm font-medium ${textSecondaryOnCanvas}`}
          onClick={onClose}
          type="button"
        >
          Close
        </button>
      </div>
      {issues.isPending ? (
        <p className={`mt-2 text-sm ${textMutedOnCanvas}`}>Loading issues…</p>
      ) : null}
      {issues.isError ? (
        <p className={`mt-2 text-sm ${dangerText}`}>Could not load issues.</p>
      ) : null}
      {issues.data !== undefined ? (
        <label className={`mt-2 block text-sm font-medium ${textSecondaryOnCanvas}`}>
          Issue
          <select
            aria-label="Issue"
            className={`mt-1 block min-h-11 w-full rounded-lg px-3 py-2 text-sm font-normal ${inputClasses(true)}`}
            onChange={(event) => setIssueKey(event.target.value)}
            required
            value={issueKey}
          >
            <option value="">Choose an issue</option>
            {issues.data.map((issue) => (
              <option key={issue.key} value={issue.key}>
                {issue.key} · {issue.title}
              </option>
            ))}
          </select>
        </label>
      ) : null}
      {issueKey === "" ? null : (
        <div className="mt-2">
          <ConversationComposer
            agents={[agent]}
            defaultDelivery={delivery}
            issueKey={issueKey}
            onSent={onClose}
            recipientSlot={
              <span className={`text-sm font-medium ${textSecondaryOnCanvas}`}>To: {label}</span>
            }
            route={`session:${agent.session_id}`}
          />
        </div>
      )}
    </div>
  );
}

function AgentRow({ agent, needsYou }: { agent: Agent; needsYou: number }): ReactNode {
  const [delivery, setDelivery] = useState<Delivery | null>(null);
  const label = agent.title.trim() === "" ? agent.session_id : agent.title;
  const machineAndDir = `${agent.machine_id} · ${agent.dir}`;
  const supportsBTW = agent.capabilities.includes("btw");

  return (
    <article className={`rounded-xl border p-4 ${card} ${borderDefault}`}>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex min-w-0 items-start gap-2">
          <FreshnessDot agent={agent} />
          <div className="min-w-0">
            <h2 className={`truncate font-semibold ${textPrimaryOnCanvas}`} title={label}>
              {label}
            </h2>
            <p className={`truncate text-sm ${textMutedOnCanvas}`} title={machineAndDir}>
              {machineAndDir}
            </p>
          </div>
        </div>
        <div className="flex flex-wrap items-center justify-end gap-2">
          {needsYou === 0 ? null : (
            <span
              className={`rounded-full px-2 py-1 text-xs font-semibold ${surfaceMutedStrongBg} ${textSecondaryOnCanvas}`}
            >
              Needs you {needsYou}
            </span>
          )}
          <span
            className={`rounded-full px-2 py-1 text-xs font-medium ${surfaceMutedBg} ${textSecondaryOnCanvas}`}
          >
            Open asks {agent.open_asks}
          </span>
        </div>
      </div>
      <div className="mt-3 flex flex-wrap items-center gap-2 text-xs">
        {agent.roles.map((role) => (
          <span
            className={`rounded-full px-2 py-1 font-medium ${surfaceMutedStrongBg} ${textSecondaryOnCanvas}`}
            key={role}
          >
            {role}
          </span>
        ))}
        {agent.capabilities.map((capability) => (
          <span className={textMutedOnCanvas} key={capability}>
            {capability}
          </span>
        ))}
        <span className={`ml-auto ${textMutedOnCanvas}`}>
          {agent.last_activity === null ? (
            "No dispatch activity"
          ) : (
            <Timestamp at={agent.last_activity} />
          )}
        </span>
      </div>
      <div className="mt-3 flex flex-wrap items-center gap-2">
        <button
          aria-label={`BTW ${label}`}
          className={`min-h-11 rounded-lg px-3 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
          disabled={!supportsBTW}
          onClick={() => setDelivery("btw")}
          title={supportsBTW ? undefined : `${label} does not advertise BTW`}
          type="button"
        >
          BTW
        </button>
        <button
          aria-label={`Steer ${label}`}
          className={`min-h-11 rounded-lg border px-3 text-sm font-semibold ${borderDefault} ${textSecondaryOnCanvas}`}
          onClick={() => setDelivery("steer")}
          type="button"
        >
          Steer
        </button>
        <span className={`text-xs ${textMutedOnCanvas}`}>
          Seen <Timestamp at={new Date(agent.last_seen).toISOString()} />
        </span>
      </div>
      {delivery === null ? null : (
        <AgentMessageComposer agent={agent} delivery={delivery} onClose={() => setDelivery(null)} />
      )}
    </article>
  );
}

export function AgentsPage(): ReactNode {
  useDocumentTitle("Agents · Dispatch");
  const { agents, error, isError, isPending } = useAgents(true, true);
  const inbox = useQuery({ queryFn: () => api.getInbox(), queryKey: ["inbox"] });
  const needsYouBySession = useMemo(() => {
    const counts = new Map<string, number>();
    for (const ask of waitingOnYou(inbox.data ?? [])) {
      if (ask.author.kind !== "session") continue;
      counts.set(ask.author.id, (counts.get(ask.author.id) ?? 0) + 1);
    }
    return counts;
  }, [inbox.data]);
  const orderedAgents = useMemo(
    () =>
      [...agents].sort(
        (left, right) => right.last_seen - left.last_seen || left.title.localeCompare(right.title)
      ),
    [agents]
  );

  if (isPending) return <p className={textMutedOnCanvas}>Loading agents…</p>;
  if (isError) return <p className={dangerText}>Could not load agents: {error}</p>;

  return (
    <section aria-label="Agents">
      <header className={`mb-5 border-b pb-4 ${borderDefault}`}>
        <h1 className="text-2xl font-semibold">Agents</h1>
        <p className={`mt-1 text-sm ${textMutedOnCanvas}`}>
          Live Envoy sessions and their Dispatch activity.
        </p>
      </header>
      {orderedAgents.length === 0 ? (
        <p className={textMutedOnCanvas}>No agents are connected to Envoy right now.</p>
      ) : (
        <div className="space-y-3">
          {orderedAgents.map((agent) => (
            <AgentRow
              agent={agent}
              key={agent.session_id}
              needsYou={needsYouBySession.get(agent.session_id) ?? 0}
            />
          ))}
        </div>
      )}
    </section>
  );
}
