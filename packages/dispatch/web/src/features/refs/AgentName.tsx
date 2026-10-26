import { actorLabel, actorName, sessionLabel } from "@legion/contracts";
import { Fragment, type ReactNode, useMemo } from "react";
import { generatePath, Link } from "react-router-dom";

import type { Actor, AskResolution } from "../../api/types";
import {
  focusVisibleRing,
  linkHoverText,
  linkText,
  liveDotBg,
  offlineDotBorder,
  unknownDotBorder,
} from "../../theme/classes";
import { type AgentState, useAgentRegistry } from "./agent-registry";
import { AGENT_LIVE_PATH } from "./routes";
import { relativeTimeLabel } from "./Timestamp";

/**
 * How Dispatch shows an agent (a session), in one place: named by its current registry title
 * when the registry lists it, else by today's rule (the title stamped on the write, else
 * `session:<8>…`), with its running state from the registry and a link to its live
 * conversation. This module owns the SPA's use of the contracts naming functions; the reference
 * short form in `routes.ts` is the one other reader, and it does not show an agent. The root
 * `biome.json` bans importing them anywhere else in `web/src` (and `AGENT_LIVE_PATH` outside
 * `app.tsx`, which mounts the route), but not through a namespace import
 * (`import * as contracts from "@legion/contracts"`): the review audit in
 * `packages/dispatch/AGENTS.md` ("How an agent is shown") catches that member use.
 */

type SessionActor = Extract<Actor, { kind: "session" }>;

type AgentSubject =
  | { actor: Actor; sessionId?: never; storedTitle?: never }
  | { actor?: never; sessionId: string; storedTitle: string | undefined };

type AgentVariant =
  /** `link` (default): the name links to the live conversation. `static`: no link, for the
   *  places a link cannot sit (a tooltip, a listbox option, the live page's own header). */
  | { kind?: "link" | "static"; control?: never }
  /** `control`: the host keeps its own control around the name, and the live link renders
   *  beside it, outside the host control. */
  | { kind: "control"; control: (name: ReactNode) => ReactNode };

type AgentNameProps = AgentSubject &
  AgentVariant & {
    className?: string;
    /** Inside a sentence: the link keeps its line box (`.dispatch-inline-link`, `styles.css`). */
    inline?: boolean;
  };

const LINK_CLASS = `rounded outline-none focus-visible:ring-2 ${focusVisibleRing} ${linkText} ${linkHoverText}`;

const MARK_CLASS = "mr-1 inline-block h-2 w-2 shrink-0 rounded-full align-middle";

/** Filled, hollow and dashed: the states read apart without colour. */
const STATE_MARK_CLASS: Record<AgentState, string> = {
  "not-running": `${MARK_CLASS} border-2 border-solid ${offlineDotBorder}`,
  running: `${MARK_CLASS} ${liveDotBg}`,
  unknown: `${MARK_CLASS} border border-dashed ${unknownDotBorder}`,
};

const STATE_LABEL: Record<AgentState, string> = {
  "not-running": "Not running",
  running: "Running",
  unknown: "Running state unknown",
};

function AgentStateMark({
  lastSeen,
  state,
}: {
  lastSeen: number | undefined;
  state: AgentState;
}): ReactNode {
  const seen =
    state === "running" && lastSeen !== undefined
      ? relativeTimeLabel(new Date(lastSeen), Date.now())
      : undefined;
  return (
    <span
      aria-label={STATE_LABEL[state]}
      className={STATE_MARK_CLASS[state]}
      role="img"
      title={seen === undefined ? STATE_LABEL[state] : `${STATE_LABEL[state]} · seen ${seen}`}
    />
  );
}

/**
 * An agent, as every place in the app shows one. A `user` or `system` actor is its id as plain
 * text and reads no registry. A session renders in one
 * `<span data-agent-name data-agent-state>`: its state mark, its name, and its attribution.
 */
export function AgentName(props: AgentNameProps): ReactNode {
  if (props.sessionId !== undefined) {
    return (
      <SessionAgentName
        className={props.className}
        inline={props.inline}
        subject={{ sessionId: props.sessionId, storedTitle: props.storedTitle }}
        variant={props}
      />
    );
  }
  const { actor } = props;
  if (actor.kind !== "session") {
    return props.className === undefined ? (
      actor.id
    ) : (
      <span className={props.className}>{actor.id}</span>
    );
  }
  return (
    <SessionAgentName
      className={props.className}
      inline={props.inline}
      subject={{ actor }}
      variant={props}
    />
  );
}

function SessionAgentName({
  className,
  inline,
  subject,
  variant,
}: {
  className: string | undefined;
  inline: boolean | undefined;
  subject: { actor: SessionActor } | { sessionId: string; storedTitle: string | undefined };
  variant: AgentVariant;
}): ReactNode {
  const registry = useAgentRegistry(true);
  let sessionId: string;
  let label: string;
  let name: string;
  if ("actor" in subject) {
    sessionId = subject.actor.id;
    label = actorLabel(subject.actor, registry.titles);
    name = actorName(subject.actor, registry.titles);
  } else {
    sessionId = subject.sessionId;
    name = sessionLabel(sessionId, registry.title(sessionId) ?? subject.storedTitle);
    label = name;
  }
  // `actorLabel` is `actorName` followed by the attribution, so what follows the name is it.
  const suffix = label.slice(name.length);
  const state = registry.state(sessionId);
  const to = generatePath(AGENT_LIVE_PATH, { sessionId: encodeURIComponent(sessionId) });

  let named: ReactNode;
  let liveLink: ReactNode = null;
  if (variant.kind === "control") {
    named = variant.control(name);
    const openLabel = `Open live conversation with ${label}`;
    liveLink = (
      <Link aria-label={openLabel} className={`ml-1 px-1 ${LINK_CLASS}`} title={openLabel} to={to}>
        <span aria-hidden="true">↗</span>
      </Link>
    );
  } else if (variant.kind === "static") {
    named = name;
  } else {
    named = (
      <Link
        aria-label={suffix === "" ? undefined : label}
        className={inline ? `${LINK_CLASS} dispatch-inline-link` : LINK_CLASS}
        to={to}
      >
        {name}
      </Link>
    );
  }

  return (
    <span className={className} data-agent-name={sessionId} data-agent-state={state}>
      <AgentStateMark lastSeen={registry.lastSeen(sessionId)} state={state} />
      {named}
      {suffix}
      {liveLink}
    </span>
  );
}

/** A Conversation avatar's initials: the first letter of the first two words of the agent's
 *  name (never its attribution). */
function agentInitials(actor: Actor, titles: ReadonlyMap<string, string>): string {
  return actorName(actor, titles)
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((part) => part[0]?.toUpperCase())
    .join("");
}

export interface AgentText {
  /** The full label: name plus ` (for <owner>)` / ` (as ns/name)`. */
  label(actor: Actor): string;
  /** An `AgentPart` as text: a text part as is, an agent part by the rule `AgentParts` draws it
   *  by. */
  part(part: AgentPart): string;
  /** A bare session id's name: its registry title, else `storedTitle`, else its short id. */
  sessionText(sessionId: string, storedTitle: string | undefined): string;
  /** The Conversation avatar's initials (`agentInitials`). */
  initials(actor: Actor): string;
}

/**
 * An agent's name as plain text, by the same rule `AgentName` renders, for the places that can
 * hold only a string: an `aria-label` or `title`, `document.title`, a confirmation dialog's
 * sentence, the composer's `@Title` mention text and its notices, a clipboard value, the Inbox
 * empty state. Never a visible name in a byline, chip, heading or sentence: that is
 * `AgentName`. Reads the registry titles the cache already holds and fetches nothing itself.
 */
export function useAgentText(): AgentText {
  const registry = useAgentRegistry(false);
  return useMemo(
    () => ({
      initials: (actor: Actor) => agentInitials(actor, registry.titles),
      label: (actor: Actor) => actorLabel(actor, registry.titles),
      part: (part: AgentPart) =>
        typeof part === "string"
          ? part
          : "actor" in part
            ? actorLabel(part.actor, registry.titles)
            : sessionLabel(part.sessionId, registry.title(part.sessionId) ?? part.storedTitle),
      sessionText: (sessionId: string, storedTitle: string | undefined) =>
        sessionLabel(sessionId, registry.title(sessionId) ?? storedTitle),
    }),
    [registry]
  );
}

const RESOLUTION_VERB: Record<AskResolution["kind"], string> = {
  resolved: "Resolved",
  retracted: "Retracted",
};

/** `Resolved by <agent>` / `Retracted by <agent>`: the verb-and-actor prefix of a resolution,
 *  for callers that render its reason with their own markup. */
export function AskResolutionLine({ resolution }: { resolution: AskResolution }): ReactNode {
  return (
    <>
      {`${RESOLUTION_VERB[resolution.kind]} by `}
      <AgentName actor={resolution.actor} inline />
    </>
  );
}

/**
 * A sentence that names agents, kept as parts until it is rendered: text, or an agent that
 * `AgentParts` draws through `AgentName`. A builder that describes an event returns these, so
 * the agent keeps its identity (its current name, state and live link) instead of being
 * flattened into a label. A role address is text; only a recorded session is an agent part.
 */
export type AgentPart =
  | string
  | { actor: Actor }
  | { sessionId: string; storedTitle: string | undefined };

/** Draws `AgentPart`s in a line of text: each text part as is (or through `text`, which the
 *  host uses to mark up its own text), each agent part as an inline `AgentName`. */
export function AgentParts({
  parts,
  text,
}: {
  parts: readonly AgentPart[];
  text?: (part: string, index: number) => ReactNode;
}): ReactNode {
  return parts.map((part, index) => (
    // biome-ignore lint/suspicious/noArrayIndexKey: the parts are one fixed sentence, in order.
    <Fragment key={index}>
      {typeof part === "string" ? (
        text === undefined ? (
          part
        ) : (
          text(part, index)
        )
      ) : "actor" in part ? (
        <AgentName actor={part.actor} inline />
      ) : (
        <AgentName inline sessionId={part.sessionId} storedTitle={part.storedTitle} />
      )}
    </Fragment>
  ));
}

/** `Resolved by <agent> - <reason>` / `Retracted by <agent> - <reason>` as `AgentPart`s, for
 *  the description builders (the activity and event descriptions). */
export function askResolutionParts(resolution: AskResolution): AgentPart[] {
  return [
    `${RESOLUTION_VERB[resolution.kind]} by `,
    { actor: resolution.actor },
    ` - ${resolution.reason}`,
  ];
}
