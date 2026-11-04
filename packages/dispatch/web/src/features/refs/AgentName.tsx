import { actorAttribution, actorName, sessionLabel, shortSessionId } from "@legion/contracts";
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
import { askResolutionBadge, askResolutionReason } from "./actor";
import { type AgentRegistry, type AgentState, useAgentRegistry } from "./agent-registry";
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

/** An agent to name: an actor, or a bare session id with the title stamped where it was
 *  recorded. */
type AgentRef = { actor: Actor } | { sessionId: string; storedTitle: string | undefined };

type SessionRef = { actor: SessionActor } | { sessionId: string; storedTitle: string | undefined };

interface ResolvedAgent {
  /** The session the agent is; `undefined` for a user or system actor. */
  readonly sessionId: string | undefined;
  /** A session's registry title, else the title stamped on the write, else `session:<8>…`; a
   *  user's or system's id. */
  readonly name: string;
  /** What follows the name: ` (for <owner>)`, ` (as <namespace>/<name>)`, or `""`. */
  readonly attribution: string;
}

/** The one naming rule: every name this module draws or spells comes from here. */
function resolveAgent(
  ref: SessionRef,
  registry: AgentRegistry
): ResolvedAgent & { readonly sessionId: string };
function resolveAgent(ref: AgentRef, registry: AgentRegistry): ResolvedAgent;
function resolveAgent(ref: AgentRef, registry: AgentRegistry): ResolvedAgent {
  if ("actor" in ref) {
    const { actor } = ref;
    return {
      attribution: actorAttribution(actor),
      name: actorName(actor, registry.titles),
      sessionId: actor.kind === "session" ? actor.id : undefined,
    };
  }
  return {
    attribution: "",
    name: sessionLabel(ref.sessionId, registry.title(ref.sessionId) ?? ref.storedTitle),
    sessionId: ref.sessionId,
  };
}

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

const AGENT_SPAN_CLASS = "min-w-0 [overflow-wrap:anywhere]";

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
 * text (inside the host's control, for `kind="control"`) and reads no registry. A session
 * renders in one `<span data-agent-name data-agent-state>`: its state mark, its name, and its
 * attribution.
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
    const named = props.kind === "control" ? props.control(actor.id) : actor.id;
    return props.className === undefined ? named : <span className={props.className}>{named}</span>;
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
  subject: SessionRef;
  variant: AgentVariant;
}): ReactNode {
  const registry = useAgentRegistry(true);
  const { attribution, name, sessionId } = resolveAgent(subject, registry);
  const state = registry.state(sessionId);
  const to = generatePath(AGENT_LIVE_PATH, { sessionId: encodeURIComponent(sessionId) });

  let named: ReactNode;
  let liveLink: ReactNode = null;
  if (variant.kind === "control") {
    named = variant.control(name);
    const openLabel = `Open live conversation with ${name}${attribution}`;
    liveLink = (
      <Link aria-label={openLabel} className={`ml-1 px-1 ${LINK_CLASS}`} title={openLabel} to={to}>
        <span aria-hidden="true">↗</span>
      </Link>
    );
  } else if (variant.kind === "static") {
    named = name;
  } else {
    // No `aria-label`: the link is named by its visible name, and the attribution after it is
    // read once, as text.
    named = (
      <Link className={inline ? `${LINK_CLASS} dispatch-inline-link` : LINK_CLASS} to={to}>
        {name}
      </Link>
    );
  }

  // The name never widens its host: a flex item may shrink below its text (`min-w-0`), and a
  // long title with no space breaks mid-word; a host's `truncate` (nowrap) still ellipsizes.
  // The short form `session:<8>…` does not tell sessions apart (ids minted in the same minute
  // share it), so an agent named by it carries its full id as its tooltip.
  return (
    <span
      className={className === undefined ? AGENT_SPAN_CLASS : `${AGENT_SPAN_CLASS} ${className}`}
      data-agent-name={sessionId}
      data-agent-state={state}
      title={name === shortSessionId(sessionId) ? sessionId : undefined}
    >
      <AgentStateMark lastSeen={registry.lastSeen(sessionId)} state={state} />
      {named}
      {attribution}
      {liveLink}
    </span>
  );
}

/** A Conversation avatar's initials: the first letter of the first two words of an agent's
 *  name (never its attribution). */
function agentInitials(name: string): string {
  return name
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
   *  by, an artifact part its name, and a quoted passage the source its author wrote, which is
   *  all a place that holds only a string can say of it. */
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
  return useMemo(() => {
    const label = (ref: AgentRef) => {
      const { attribution, name } = resolveAgent(ref, registry);
      return `${name}${attribution}`;
    };
    return {
      initials: (actor: Actor) => agentInitials(resolveAgent({ actor }, registry).name),
      label: (actor: Actor) => label({ actor }),
      part: (part: AgentPart) => {
        if (typeof part === "string") return part;
        if (isQuoted(part)) return part.markdown;
        return "artifact" in part ? part.artifact : label(part);
      },
      sessionText: (sessionId: string, storedTitle: string | undefined) =>
        resolveAgent({ sessionId, storedTitle }, registry).name,
    };
  }, [registry]);
}

/** The text before and after a resolution's actor. A withdrawal is the close of the ask's issue,
 *  so it names the closer: `Withdrawn when <agent> closed the issue`; else `<Verb> by <agent>`. */
function resolutionFrame(resolution: AskResolution): readonly [before: string, after: string] {
  return resolution.kind === "withdrawn"
    ? ["Withdrawn when ", " closed the issue"]
    : [`${askResolutionBadge(resolution)} by `, ""];
}

/** `Resolved by <agent>` / `Retracted by <agent>` / `Withdrawn when <agent> closed the issue`:
 *  the actor sentence of a resolution, for callers that render its reason (`askResolutionReason`)
 *  with their own markup. `agent="static"` names the agent without its link, for a host that is
 *  itself one control (a listbox option). */
export function AskResolutionLine({
  agent,
  resolution,
}: {
  agent?: "link" | "static";
  resolution: AskResolution;
}): ReactNode {
  const [before, after] = resolutionFrame(resolution);
  return (
    <>
      {before}
      <AgentName actor={resolution.actor} inline kind={agent} />
      {after}
    </>
  );
}

/**
 * A sentence that names agents, kept as parts until it is rendered: text, an agent that
 * `AgentParts` draws through `AgentName`, an artifact the sentence is about, or a passage
 * somebody wrote that it quotes. A builder that describes an event returns these, so the agent
 * keeps its identity (its current name, state and live link) instead of being flattened into a
 * label, the artifact keeps the link its host can give it instead of being found again inside
 * the text, and the quoted passage keeps its Markdown instead of being shown as its source. A
 * role address is text; only a recorded session is an agent part.
 */
export type AgentPart = string | AgentRef | ArtifactPart | QuotedPart;

/** The artifact a sentence is about, by name: `AgentParts` draws it as its name, or through
 *  `artifact`, which a host that knows the artifact's route (`ActivityLine`) passes to link it. */
type ArtifactPart = { artifact: string };

/** A passage somebody wrote that a sentence quotes - an ask's question, the question a reply
 *  answers - as its author wrote it: `AgentParts` hands it to `quote`, which draws it formatted
 *  (`MarkdownPreview` at one line), so `**held**` reads as bold `held` rather than as its
 *  source. The drawing is the host's because this module sits under `MarkdownPreview`'s own
 *  imports and cannot import it back. */
type QuotedPart = { markdown: string };

function isQuoted(part: AgentPart): part is QuotedPart {
  return typeof part !== "string" && "markdown" in part;
}

/**
 * Draws `AgentPart`s in a line of text: each text part as is, each agent part as an inline
 * `AgentName`, each artifact part as its name (or through `artifact`, which a host that can
 * link it passes), and each quoted passage through `quote` (or, without one, as the source its
 * author wrote).
 *
 * A sentence that quotes a passage is drawn as a row of its parts: the quote is drawn in a box
 * of its own, which cannot sit inside a run of text, so the sentence's own words sit beside it
 * and keep their spaces (`whitespace-pre-wrap`, which a flex item's text would otherwise
 * collapse at its edges). A sentence without one is the run of text it always was.
 */
export function AgentParts({
  artifact,
  parts,
  quote,
}: {
  artifact?: (name: string) => ReactNode;
  parts: readonly AgentPart[];
  quote?: (markdown: string) => ReactNode;
}): ReactNode {
  const quotes = parts.some(isQuoted);
  const drawPart = (part: AgentPart): ReactNode => {
    if (typeof part === "string") {
      return quotes ? <span className="whitespace-pre-wrap">{part}</span> : part;
    }
    if (isQuoted(part)) {
      return quote === undefined ? part.markdown : quote(part.markdown);
    }
    if ("artifact" in part) {
      return artifact === undefined ? part.artifact : artifact(part.artifact);
    }
    return <AgentName {...part} inline />;
  };
  const drawn = parts.map((part, index) => (
    // biome-ignore lint/suspicious/noArrayIndexKey: the parts are one fixed sentence, in order.
    <Fragment key={index}>{drawPart(part)}</Fragment>
  ));
  return quotes ? <span className="inline-flex max-w-full items-baseline">{drawn}</span> : drawn;
}

/** `Resolved by <agent> - <reason>` / `Retracted by <agent> - <reason>` / `Withdrawn when <agent>
 *  closed the issue` (a withdrawal's reason only repeats it) as `AgentPart`s, for the description
 *  builders (the activity and event descriptions). */
export function askResolutionParts(resolution: AskResolution): AgentPart[] {
  const [before, after] = resolutionFrame(resolution);
  const reason = askResolutionReason(resolution);
  return [
    before,
    { actor: resolution.actor },
    reason === undefined ? after : `${after} - ${reason}`,
  ];
}
