import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useMemo, useState } from "react";

import { api } from "../../api/client";
import { queryKeys } from "../../api/query-keys";
import type { Agent, MessageRead } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill } from "../../components/Pill";
import {
  borderDefault,
  card,
  connectionDotConnecting,
  dangerText,
  inputClasses,
  liveDotBg,
  offlineDotBg,
  primaryButtonBg,
  primaryButtonEnabledHoverBg,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { ConversationComposer } from "../conversation/ConversationComposer";
import { TargetedMessageCard } from "../conversation/TargetedMessageCard";
import { useAgents } from "../conversation/useAgents";
import { waitingOnYou } from "../inbox/BlockedOnYou";
import { MarkdownBody } from "../refs/MarkdownBody";
import { Timestamp } from "../refs/Timestamp";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { userPreferenceStorageKey } from "../shell/userPreference";

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

function PinIcon({ pinned }: { pinned: boolean }): ReactNode {
  return (
    <svg
      aria-hidden="true"
      className="h-4 w-4"
      fill={pinned ? "currentColor" : "none"}
      viewBox="0 0 16 16"
    >
      <path
        d="M5 2.5h6v3l1.5 2v1H9v4.75L8 14.5l-1-1.25V8.5H3.5v-1L5 5.5z"
        stroke="currentColor"
        strokeLinejoin="round"
        strokeWidth="1.25"
      />
    </svg>
  );
}

function agentLabel(agent: Agent): string {
  return agent.title.trim() === "" ? agent.session_id : agent.title;
}

export function orderAgents(agents: readonly Agent[], pinned: readonly string[]): Agent[] {
  const pinOrder = new Map(pinned.map((sessionID, index) => [sessionID, index]));
  return [...agents].sort((left, right) => {
    const leftPin = pinOrder.get(left.session_id);
    const rightPin = pinOrder.get(right.session_id);
    if (leftPin !== undefined || rightPin !== undefined) {
      if (leftPin === undefined) return 1;
      if (rightPin === undefined) return -1;
      return leftPin - rightPin;
    }
    if (left.last_activity === null || right.last_activity === null) {
      if (left.last_activity === null && right.last_activity !== null) return 1;
      if (left.last_activity !== null && right.last_activity === null) return -1;
    } else if (left.last_activity !== right.last_activity) {
      return right.last_activity.localeCompare(left.last_activity);
    }
    return left.title.localeCompare(right.title) || left.session_id.localeCompare(right.session_id);
  });
}

function AgentTargetedMessage({ agent, read }: { agent: Agent; read: MessageRead }): ReactNode {
  const queryClient = useQueryClient();
  const retry = useMutation({
    mutationFn: (delivery: "btw" | "steer") => api.createMessageDelivery(read.message.id, delivery),
    onSuccess: () =>
      void queryClient.invalidateQueries({ queryKey: queryKeys.agentMessages(agent.session_id) }),
  });
  const reply = read.replies.at(-1);
  return (
    <TargetedMessageCard
      answer={
        reply === undefined
          ? undefined
          : {
              author: reply.author.kind === "user" ? reply.author.id : agentLabel(agent),
              body: (
                <div className={textPrimaryOnCanvas}>
                  {read.replies.map((item) => (
                    <MarkdownBody key={item.id} markdown={item.body} variant="inline" />
                  ))}
                </div>
              ),
            }
      }
      body={
        <>
          <p className={`flex items-baseline gap-2 text-sm ${textSecondaryOnCanvas}`}>
            <span className="font-semibold">
              {read.message.author.kind === "user" ? read.message.author.id : agentLabel(agent)}
            </span>
            <Timestamp at={read.message.created_at} />
          </p>
          <div className={textPrimaryOnCanvas}>
            <MarkdownBody markdown={read.message.body} variant="inline" />
          </div>
        </>
      }
      canBtw={agent.capabilities.includes("btw")}
      deliveries={read.message.deliveries.map((attempt) => ({
        attempt: attempt.attempt,
        createdAt: attempt.created_at,
        delivery: attempt.delivery,
        error: attempt.error,
        state: attempt.state,
        targetName: agentLabel(agent),
      }))}
      header={null}
      isClosed={false}
      onRetry={retry.mutate}
      retrying={retry.isPending}
      targetName={agentLabel(agent)}
      turnID={`message:${read.message.id}`}
    />
  );
}

function AgentMessageList({ agent }: { agent: Agent }): ReactNode {
  const messages = useQuery({
    queryFn: () => api.listAgentMessages(agent.session_id),
    queryKey: queryKeys.agentMessages(agent.session_id),
  });
  const label = agentLabel(agent);
  if (messages.isPending) return null;
  if (messages.isError) {
    return <p className={`mt-3 text-sm ${dangerText}`}>Could not load this conversation.</p>;
  }
  if (messages.data === undefined || messages.data.length === 0) return null;
  return (
    <ol
      aria-label={`Conversation with ${label}`}
      className={`mt-3 space-y-2 border-t pt-3 ${borderDefault}`}
    >
      {messages.data.map((read) => (
        <AgentTargetedMessage agent={agent} key={read.message.id} read={read} />
      ))}
    </ol>
  );
}

function AgentMessageComposer({ agent }: { agent: Agent }): ReactNode {
  const queryClient = useQueryClient();
  const [issueKey, setIssueKey] = useState("");
  const [issuePickerOpen, setIssuePickerOpen] = useState(false);
  const issues = useQuery({
    enabled: issuePickerOpen,
    queryFn: () => api.listIssues({ open: true }),
    queryKey: queryKeys.agentIssuePicker(),
  });
  const label = agentLabel(agent);

  return (
    <div className={`mt-3 border-t pt-3 ${borderDefault}`}>
      <button
        aria-expanded={issuePickerOpen}
        aria-label="Choose issue"
        className={`min-h-11 rounded-lg border px-3 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
        onClick={() => setIssuePickerOpen((open) => !open)}
        type="button"
      >
        Issue: {issueKey === "" ? "No issue" : issueKey}
      </button>
      {issuePickerOpen ? (
        <div className="mt-2">
          {issues.isPending ? (
            <p className={`text-sm ${textMutedOnCanvas}`}>Loading issues…</p>
          ) : null}
          {issues.isError ? (
            <p className={`text-sm ${dangerText}`}>Could not load issues.</p>
          ) : null}
          {issues.data === undefined ? null : (
            <label className={`block text-sm font-medium ${textSecondaryOnCanvas}`}>
              Issue (optional)
              <select
                aria-label="Issue"
                className={`mt-1 block min-h-11 w-full rounded-lg px-3 py-2 text-sm font-normal ${inputClasses(true)}`}
                onChange={(event) => {
                  setIssueKey(event.target.value);
                  setIssuePickerOpen(false);
                }}
                value={issueKey}
              >
                <option value="">No issue</option>
                {issues.data.map((issue) => (
                  <option key={issue.key} value={issue.key}>
                    {issue.key} · {issue.title}
                  </option>
                ))}
              </select>
            </label>
          )}
        </div>
      ) : null}
      <ConversationComposer
        agents={[agent]}
        embedded
        onSent={() =>
          void queryClient.invalidateQueries({
            queryKey: queryKeys.agentMessages(agent.session_id),
          })
        }
        onSend={({ body, delivery, target }) =>
          issueKey === ""
            ? api.createAgentMessage(agent.session_id, { body, delivery })
            : api.createMessage(issueKey, { body, delivery, target })
        }
        recipientSlot={
          <span className={`text-sm font-medium ${textSecondaryOnCanvas}`}>To: {label}</span>
        }
        route={`session:${agent.session_id}`}
      />
    </div>
  );
}

function AgentRow({
  agent,
  needsYou,
  onPin,
  pinned,
}: {
  agent: Agent;
  needsYou: number;
  onPin: () => void;
  pinned: boolean;
}): ReactNode {
  const label = agentLabel(agent);
  const machineAndDir = `${agent.machine_id} · ${agent.dir}`;

  return (
    <article className={`rounded-xl border p-4 ${card} ${borderDefault}`}>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex min-w-0 items-start gap-2">
          <FreshnessDot agent={agent} />
          <div className="min-w-0">
            <h2 className={`truncate text-base font-semibold ${textPrimaryOnCanvas}`} title={label}>
              {label}
            </h2>
            <p className={`truncate text-sm ${textMutedOnCanvas}`} title={machineAndDir}>
              {machineAndDir}
            </p>
          </div>
        </div>
        <div className="flex flex-wrap items-center justify-end gap-2">
          {needsYou === 0 ? null : <LabelPill selected>Needs you {needsYou}</LabelPill>}
          <LabelPill>Open asks {agent.open_asks}</LabelPill>
          <button
            aria-label={pinned ? `Unpin ${label}` : `Pin ${label}`}
            aria-pressed={pinned}
            className={`flex min-h-11 min-w-11 shrink-0 items-center justify-center rounded-lg ${
              pinned
                ? `${primaryButtonBg} ${primaryButtonEnabledHoverBg}`
                : `${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`
            }`}
            onClick={onPin}
            title={pinned ? "Unpin agent" : "Pin agent"}
            type="button"
          >
            <PinIcon pinned={pinned} />
          </button>
        </div>
      </div>
      <div className="mt-3 flex flex-wrap items-center gap-2 text-xs">
        {agent.roles.map((role) => (
          <LabelPill key={role}>{role}</LabelPill>
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
      <AgentMessageList agent={agent} />
      <AgentMessageComposer agent={agent} />
      <p className={`mt-2 text-xs ${textMutedOnCanvas}`}>
        Seen <Timestamp at={new Date(agent.last_seen).toISOString()} />
      </p>
    </article>
  );
}

export function AgentsPage(): ReactNode {
  useDocumentTitle("Agents · Dispatch");
  const { agents, error, isError, isPending } = useAgents(true, true);
  const inbox = useQuery({ queryFn: () => api.getInbox(), queryKey: ["inbox"] });
  const whoAmI = useQuery({ queryFn: () => api.whoAmI(), queryKey: ["whoami"] });
  const preferenceKey =
    whoAmI.data?.kind === "user"
      ? userPreferenceStorageKey(whoAmI.data.login, "agents.pinned")
      : undefined;
  const [pinned, setPinned] = useState<readonly string[]>([]);
  useEffect(() => {
    if (preferenceKey === undefined) {
      setPinned([]);
      return;
    }
    const raw = window.localStorage.getItem(preferenceKey);
    try {
      const parsed: unknown = raw === null ? [] : JSON.parse(raw);
      setPinned(
        Array.isArray(parsed)
          ? parsed.filter((value): value is string => typeof value === "string")
          : []
      );
    } catch {
      setPinned([]);
    }
  }, [preferenceKey]);
  const needsYouBySession = useMemo(() => {
    const counts = new Map<string, number>();
    for (const ask of waitingOnYou(inbox.data ?? [])) {
      if (ask.author.kind !== "session") continue;
      counts.set(ask.author.id, (counts.get(ask.author.id) ?? 0) + 1);
    }
    return counts;
  }, [inbox.data]);
  const orderedAgents = useMemo(() => orderAgents(agents, pinned), [agents, pinned]);
  const togglePin = (sessionID: string) => {
    setPinned((current) => {
      const next = current.includes(sessionID)
        ? current.filter((candidate) => candidate !== sessionID)
        : [...current, sessionID];
      if (preferenceKey !== undefined) {
        window.localStorage.setItem(preferenceKey, JSON.stringify(next));
      }
      return next;
    });
  };

  if (isPending) return <LoadingSkeleton label="Loading agents" />;
  if (isError) return <p className={dangerText}>Could not load agents: {error}</p>;

  return (
    <section aria-label="Agents">
      <header className={`mb-5 border-b pb-4 ${borderDefault}`}>
        <h1 className={`text-[22px] font-semibold tracking-tight ${textPrimaryOnCanvas}`}>
          Agents
        </h1>
        <p className={`mt-1 text-sm ${textMutedOnCanvas}`}>
          Live Envoy sessions and their Dispatch activity.
        </p>
      </header>
      {orderedAgents.length === 0 ? (
        <EmptyState label="Agents empty state" message="No agents are connected." />
      ) : (
        <div className="space-y-3">
          {orderedAgents.map((agent) => (
            <AgentRow
              agent={agent}
              key={agent.session_id}
              needsYou={needsYouBySession.get(agent.session_id) ?? 0}
              onPin={() => togglePin(agent.session_id)}
              pinned={pinned.includes(agent.session_id)}
            />
          ))}
        </div>
      )}
    </section>
  );
}
