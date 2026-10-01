import { itemFromSearch } from "@legion/contracts";
import { matchPath } from "react-router-dom";

import type { Artifact } from "../../api/types";

export type IssueRoute =
  | { key: string; kind: "issue" }
  | { key: string; kind: "spec" }
  | { key: string; kind: "conversation" }
  | { key: string; kind: "children" }
  | { key: string; kind: "artifacts" }
  | { key: string; kind: "artifact"; slug: string; version?: number }
  | { key: string; kind: "ask"; id: string }
  | { key: string; kind: "comment"; id: string }
  | { key: string; kind: "message"; id: string };

/** `project` is the bare `/projects/:key`, which opens on `architecture` when the project has
 *  an architecture source and on `issues` otherwise (`ProjectPage`'s open-on rule); the Issues
 *  tab and its `?label=`/`?status=`/`?q=` filter URLs live at `/issues`. */
export type ProjectRoute =
  | { kind: "project"; project: string }
  | { kind: "architecture"; project: string }
  | { kind: "issues"; project: string }
  | { kind: "documents"; project: string }
  | {
      kind: "document";
      project: string;
      slug: string;
      version?: number;
      item?: { kind: "ask" | "comment"; id: string };
    };

export type DispatchRoute = IssueRoute | ProjectRoute;
export type ProjectDocumentRoute = Extract<ProjectRoute, { kind: "document" }>;
export type DispatchReferenceRoute = IssueRoute | ProjectDocumentRoute;
/** The Inbox at `/`, optionally narrowed to one agent's asks (`?agent=<session id>`); with
 *  `section=needs-you` only the asks waiting on the viewer are shown. `view` picks the
 *  partition: `mine` (the viewer's issues plus an Unassigned band) or `everyone`; absent, the
 *  viewer's remembered choice applies. */
export type InboxView = "mine" | "everyone";
export interface InboxRoute {
  agent?: string;
  section?: "needs-you";
  view?: InboxView;
}
export type IssueTab = "spec" | "conversation" | "children" | "artifacts";

const projectKeyPattern = "[A-Z][A-Z0-9]{1,9}";
const issueKeyPattern = `${projectKeyPattern}-[1-9]\\d*`;
const issueKeyOnlyPattern = new RegExp(`^${issueKeyPattern}$`);
const projectKeyOnlyPattern = new RegExp(`^${projectKeyPattern}$`);
const issuePathPattern = new RegExp(`^/issues/(${issueKeyPattern})(?:/(.*))?$`);
const projectPathPattern = new RegExp(`^/projects/(${projectKeyPattern})(?:/(.*))?$`);
/** The slug Dispatch gives a document, the one a reference can name. */
const artifactSlugPattern = "[a-z0-9]+(?:-[a-z0-9]+)*";
const artifactSlugOnlyPattern = new RegExp(`^${artifactSlugPattern}$`);
const issueArtifactReferencePattern = new RegExp(
  `^artifact/(${artifactSlugPattern})(?:@v([1-9]\\d*))?$`
);
const projectArtifactReferencePattern = new RegExp(
  `^artifact/(${artifactSlugPattern})(?:@v([1-9]\\d*))?(?:/(ask|comment)/([^/?#\\s]+))?$`
);

/** Agents reference the conversation as `dispatch://KEY/log`; the browser path is `/conversation`. */
const legacyLogReferencePattern = /^log$/;

export function isProjectRoute(route: DispatchRoute): route is ProjectRoute {
  return (
    route.kind === "project" ||
    route.kind === "architecture" ||
    route.kind === "issues" ||
    route.kind === "documents" ||
    route.kind === "document"
  );
}

export function isPrimaryDocumentArtifactRoute(
  route: IssueRoute,
  artifact: Pick<Artifact, "kind" | "primary"> | undefined
): boolean {
  return route.kind === "artifact" && artifact?.primary === true && artifact.kind === "doc";
}

export function issueTabForRoute(
  route: IssueRoute,
  artifact: Pick<Artifact, "kind" | "primary"> | undefined
): IssueTab {
  if (route.kind === "children") {
    return "children";
  }
  if (route.kind === "conversation") {
    return "conversation";
  }
  if (route.kind === "artifacts") {
    return "artifacts";
  }
  if (route.kind === "artifact") {
    return isPrimaryDocumentArtifactRoute(route, artifact) ? "spec" : "artifacts";
  }
  return route.kind === "issue" || route.kind === "spec" ? "spec" : "conversation";
}

/** A path segment decoded; `undefined` for one that does not decode, decodes empty, or decodes to
 * a control character, which no id or slug holds and the server's `text.Extract` refuses. */
function decodedSegment(value: string): string | undefined {
  try {
    const decoded = decodeURIComponent(value);
    return decoded === "" || /\p{Cc}/u.test(decoded) ? undefined : decoded;
  } catch {
    return undefined;
  }
}

function version(value: string | null | undefined): number | undefined | null {
  if (value === null || value === undefined || value === "") {
    return undefined;
  }
  return /^[1-9]\d*$/.test(value) ? Number(value) : null;
}

function artifactRoute(
  key: string,
  slug: string,
  versionValue?: string | null
): IssueRoute | undefined {
  const decodedSlug = decodedSegment(slug);
  const parsedVersion = version(versionValue);
  if (decodedSlug === undefined || parsedVersion === null) {
    return undefined;
  }
  return parsedVersion === undefined
    ? { key, kind: "artifact", slug: decodedSlug }
    : { key, kind: "artifact", slug: decodedSlug, version: parsedVersion };
}

function projectDocumentRoute(
  project: string,
  slug: string,
  versionValue: string | null | undefined,
  item?: { kind: "ask" | "comment"; id: string }
): ProjectDocumentRoute | undefined {
  const decodedSlug = decodedSegment(slug);
  const parsedVersion = version(versionValue);
  if (decodedSlug === undefined || parsedVersion === null) {
    return undefined;
  }
  return {
    ...(item === undefined ? {} : { item }),
    kind: "document",
    project,
    slug: decodedSlug,
    ...(parsedVersion === undefined ? {} : { version: parsedVersion }),
  };
}

function parseIssueReference(key: string, target: string | undefined): IssueRoute | undefined {
  if (target === undefined) {
    return { key, kind: "issue" };
  }
  if (target === "spec") {
    return { key, kind: "spec" };
  }
  if (legacyLogReferencePattern.test(target) || target === "conversation") {
    return { key, kind: "conversation" };
  }
  if (target === "children") {
    return { key, kind: "children" };
  }
  if (target === "artifacts") {
    return { key, kind: "artifacts" };
  }
  const artifact = target.match(issueArtifactReferencePattern);
  if (artifact !== null) {
    return artifactRoute(key, artifact[1] ?? "", artifact[2]);
  }
  const item = target.match(/^(ask|comment|message)\/([^/?#\s]+)$/);
  const id = item?.[2] === undefined ? undefined : decodedSegment(item[2]);
  if (item === null || id === undefined) {
    return undefined;
  }
  return {
    id,
    key,
    kind: item[1] === "ask" ? "ask" : item[1] === "comment" ? "comment" : "message",
  };
}

/** A `dispatch://` reference's route. One holding a control character names nothing, as the
 * server's `text.Extract` reads it. */
export function parseDispatchReference(value: string): DispatchReferenceRoute | undefined {
  if (/\p{Cc}/u.test(value)) {
    return undefined;
  }
  const match = value.match(/^dispatch:\/\/([^/]+)(?:\/(.*))?$/);
  if (match === null) {
    return undefined;
  }
  const key = match[1];
  const target = match[2];
  if (key === undefined) {
    return undefined;
  }
  if (issueKeyOnlyPattern.test(key)) {
    return parseIssueReference(key, target);
  }
  if (!projectKeyOnlyPattern.test(key) || target === undefined) {
    return undefined;
  }
  const document = target.match(projectArtifactReferencePattern);
  const itemID = document?.[4] === undefined ? undefined : decodedSegment(document[4]);
  if (document === null || (document[3] !== undefined && itemID === undefined)) {
    return undefined;
  }
  return projectDocumentRoute(
    key,
    document[1] ?? "",
    document[2],
    document[3] === undefined || itemID === undefined
      ? undefined
      : { id: itemID, kind: document[3] === "ask" ? "ask" : "comment" }
  );
}

export function parseIssuePath(pathname: string, search = ""): IssueRoute | undefined {
  const match = pathname.match(issuePathPattern);
  if (match === null) {
    return undefined;
  }
  const key = match[1];
  const target = match[2];
  if (key === undefined) {
    return undefined;
  }
  if (target === undefined) {
    return { key, kind: "issue" };
  }
  if (target === "spec") {
    return { key, kind: "spec" };
  }
  if (target === "conversation") {
    return { key, kind: "conversation" };
  }
  if (target === "children") {
    return { key, kind: "children" };
  }
  if (target === "artifacts") {
    return { key, kind: "artifacts" };
  }
  const artifact = target.match(/^artifacts?\/([^/?#\s]+)$/);
  if (artifact !== null) {
    return artifactRoute(key, artifact[1] ?? "", new URLSearchParams(search).get("v"));
  }
  const item = target.match(/^(asks|comments|messages)\/([^/?#\s]+)$/);
  const id = item?.[2] === undefined ? undefined : decodedSegment(item[2]);
  if (item === null || id === undefined) {
    return undefined;
  }
  return {
    id,
    key,
    kind: item[1] === "asks" ? "ask" : item[1] === "comments" ? "comment" : "message",
  };
}

export function parseProjectPath(pathname: string, search = ""): ProjectRoute | undefined {
  const match = pathname.match(projectPathPattern);
  if (match === null) {
    return undefined;
  }
  const project = match[1];
  const target = match[2];
  if (project === undefined) {
    return undefined;
  }
  if (target === undefined) {
    return { kind: "project", project };
  }
  if (target === "architecture") {
    return { kind: "architecture", project };
  }
  if (target === "issues") {
    return { kind: "issues", project };
  }
  if (target === "documents") {
    return { kind: "documents", project };
  }
  const document = target.match(/^documents\/([^/?#\s]+)$/);
  if (document === null) {
    return undefined;
  }
  const item = itemFromSearch(search);
  if (item === null) {
    return undefined;
  }
  return projectDocumentRoute(
    project,
    document[1] ?? "",
    new URLSearchParams(search).get("version"),
    item
  );
}

/** The project a route is about: a project path's own key, or an issue path's key without its
 *  number (an issue key is `<PROJECT>-<n>`, `issueKeyPattern` above); `undefined` elsewhere. */
export function routeProjectOf(pathname: string): string | undefined {
  const issueKey = parseIssuePath(pathname)?.key;
  return parseProjectPath(pathname)?.project ?? issueKey?.slice(0, issueKey.lastIndexOf("-"));
}

/** The reference a `dispatch://` URL or a same-origin dashboard path names; undefined for an
 * external link or a dashboard path that is not an issue/document reference. A reference holding a
 * control character, or naming a slug Dispatch never gives a document, names nothing, as the
 * server's `text.Extract` reads it. */
export function referenceRouteFromHref(
  href: string,
  appOrigin: string = window.location.origin
): DispatchReferenceRoute | undefined {
  if (href.startsWith("dispatch://")) {
    return parseDispatchReference(href);
  }
  if (/\p{Cc}/u.test(href)) {
    return undefined;
  }
  let url: URL;
  try {
    url = new URL(href, appOrigin);
  } catch {
    return undefined;
  }
  if (url.origin !== appOrigin) {
    return undefined;
  }
  const route =
    parseIssuePath(url.pathname, url.search) ?? parseProjectPath(url.pathname, url.search);
  if (route === undefined || (isProjectRoute(route) && route.kind !== "document")) {
    return undefined;
  }
  if ("slug" in route && !artifactSlugOnlyPattern.test(route.slug)) {
    return undefined;
  }
  // An issue document path carrying `?comment=`/`?ask=` names that item, the same rule
  // `parseProjectPath` already applies to a project document. Without it a search hit's hover
  // card previewed the document instead of the comment the reader is about to open.
  if (!isProjectRoute(route) && (route.kind === "spec" || route.kind === "artifact")) {
    const item = itemFromSearch(url.search);
    if (item === null) {
      return undefined;
    }
    if (item !== undefined) {
      return { id: item.id, key: route.key, kind: item.kind };
    }
  }
  return route;
}

export interface ReferenceSpan {
  start: number;
  value: string;
}

/**
 * The reference-shaped spans in text: `dispatch://` references and `http(s)://` URLs, each ending
 * at whitespace (Unicode space separators included), an angle or square bracket, a quote, a
 * backtick or a pipe, then trimmed by `trimReference`. The one bracket a span may hold is a
 * bracketed IPv6 host right after `http(s)://`, so a server at an IPv6 literal keeps its dashboard
 * URLs as references. The whitespace is spelled out rather than written `\s`, which in JavaScript
 * also matches a vertical tab and U+FEFF, where Go's `\s` matches neither. It is the rule the
 * server's `text.ExtractAt` indexes mentions by, and `DISPATCH_TEXT_REFERENCES` in
 * `@legion/contracts` is the table both are tested against.
 */
const referencePattern =
  /(?:dispatch:\/\/|https?:\/\/(?:\[[0-9A-Fa-f:.]+\])?)[^\t\n\f\r \p{Z}<>"'`[\]|]+/gu;

/**
 * Every reference span in text that starts with `prefix`, trimmed; a span starting otherwise is
 * omitted from the result. The spans are the same whatever the prefix, so a `dispatch://` inside a
 * URL is never a span of its own, as the server reads it.
 */
export function referenceSpans(text: string, prefix = ""): ReferenceSpan[] {
  const spans: ReferenceSpan[] = [];
  for (const match of text.matchAll(referencePattern)) {
    if (match[0].startsWith(prefix)) {
      spans.push({ start: match.index, value: trimReference(match[0]) });
    }
  }
  return spans;
}

/**
 * Drops what trails a reference in prose: sentence punctuation, the `*`, `_` and `~` that close
 * emphasis and strikethrough around it (GFM's autolinks drop the same characters, keeping them
 * inside a link), and a closing parenthesis that opens nowhere in the reference. It counts the
 * parentheses once and walks back from the end, so a run of closers costs one pass.
 */
function trimReference(value: string): string {
  let opened = 0;
  let closed = 0;
  for (let index = 0; index < value.length; index += 1) {
    const char = value.charAt(index);
    if (char === "(") opened += 1;
    else if (char === ")") closed += 1;
  }
  let end = value.length;
  for (; end > 0; end -= 1) {
    const char = value.charAt(end - 1);
    if (char === ")") {
      if (opened >= closed) break;
      closed -= 1;
    } else if (!".,;:!?*_~".includes(char)) {
      break;
    }
  }
  return value.slice(0, end);
}

/** A reference a text cites: the `dispatch://` form and the dashboard path it opens. */
export interface ComposerReference {
  href?: string;
  reference: string;
}

/**
 * The references body cites, in order of first appearance: every `dispatch://` reference and
 * dashboard URL of `appOrigin` that names an issue, a document or an item. The composer's
 * reference pills and the unfurl cards read text through it.
 */
export function composerReferences(
  body: string,
  appOrigin = window.location.origin
): ComposerReference[] {
  const references: ComposerReference[] = [];
  for (const { value } of referenceSpans(body)) {
    const route = referenceRouteFromHref(value, appOrigin);
    if (route === undefined) {
      continue;
    }
    const reference = buildDispatchReference(route);
    if (!references.some((item) => item.reference === reference)) {
      references.push({ href: buildReferencePath(route), reference });
    }
  }
  return references;
}

export function buildDispatchReference(route: DispatchReferenceRoute): string {
  if ("project" in route) {
    return `dispatch://${route.project}/artifact/${encodeURIComponent(route.slug)}${
      route.version === undefined ? "" : `@v${route.version}`
    }${route.item === undefined ? "" : `/${route.item.kind}/${encodeURIComponent(route.item.id)}`}`;
  }
  const issue = `dispatch://${route.key}`;
  if (route.kind === "issue") {
    return issue;
  }
  if (route.kind === "spec") {
    return `${issue}/spec`;
  }
  if (route.kind === "conversation") {
    return `${issue}/log`;
  }
  if (route.kind === "children") {
    return `${issue}/children`;
  }
  if (route.kind === "artifacts") {
    return `${issue}/artifacts`;
  }
  if (route.kind === "artifact") {
    return `${issue}/artifact/${encodeURIComponent(route.slug)}${
      route.version === undefined ? "" : `@v${route.version}`
    }`;
  }
  return `${issue}/${route.kind}/${encodeURIComponent(route.id)}`;
}

/** The route a document is referenced and linked by: an issue's primary document is its `spec`,
 *  any other issue artifact is `artifact/<slug>`, and a project document sits under its project.
 *  `version` pins a historical version (`@vN`), a form the bare `spec` route cannot carry, so a
 *  versioned primary document is referenced as `artifact/<slug>@vN`. */
export function documentRoute(
  artifact: Pick<Artifact, "issue_key" | "kind" | "primary" | "project" | "slug">,
  version?: number
): DispatchReferenceRoute {
  if (artifact.issue_key === null) {
    return version === undefined
      ? { kind: "document", project: artifact.project, slug: artifact.slug }
      : { kind: "document", project: artifact.project, slug: artifact.slug, version };
  }
  if (version === undefined && artifact.primary && artifact.kind === "doc") {
    return { key: artifact.issue_key, kind: "spec" };
  }
  return version === undefined
    ? { key: artifact.issue_key, kind: "artifact", slug: artifact.slug }
    : { key: artifact.issue_key, kind: "artifact", slug: artifact.slug, version };
}

/** The route an ask or comment is referenced by: under its issue when it has one, else under the
 *  project document that owns it. Only Inbox rows carry `document`, so a caller showing a
 *  project document's items names that document; without either there is no reference. */
export function itemRoute(
  kind: "ask" | "comment",
  item: { id: string; issue_key: string | null },
  document: { project: string; slug: string } | undefined
): DispatchReferenceRoute | undefined {
  if (item.issue_key !== null) {
    return { id: item.id, key: item.issue_key, kind };
  }
  if (document === undefined) {
    return undefined;
  }
  return {
    item: { id: item.id, kind },
    kind: "document",
    project: document.project,
    slug: document.slug,
  };
}

/** The path that opens `artifact` with `item` in view: the document's own route naming the item,
 *  the shape every in-app thread link already uses. A typed ask block is named by its block
 *  fragment, which is how a decision block is focused, rather than by the ask's id. */
export function documentItemPath(
  artifact: Pick<Artifact, "issue_key" | "kind" | "primary" | "project" | "slug">,
  item: { blockID?: string; id: string; kind: "ask" | "comment" }
): string {
  const path = buildReferencePath(documentRoute(artifact));
  if (item.blockID !== undefined) {
    return `${path}#b-${encodeURIComponent(item.blockID)}`;
  }
  return `${path}?${item.kind}=${encodeURIComponent(item.id)}`;
}

/** What a reference resolves to for display: the record whose title, excerpt, or author the
 * reader is shown. An issue's spec/log/children/artifacts tabs all resolve to the issue itself;
 * an ask or comment nested under a project document resolves to that item, not the document. */
export type ReferenceTargetKind = "issue" | "document" | "ask" | "comment" | "message";

export function referenceTargetKind(route: DispatchReferenceRoute): ReferenceTargetKind {
  if (route.kind === "document") {
    return route.item?.kind ?? "document";
  }
  switch (route.kind) {
    case "ask":
    case "comment":
    case "message":
      return route.kind;
    case "artifact":
      return "document";
    default:
      return "issue";
  }
}

/** The SPA path a reference navigates to, whichever side of the issue/project split it is on. */
export function buildReferencePath(route: DispatchReferenceRoute): string {
  return isProjectRoute(route) ? buildProjectPath(route) : buildIssuePath(route);
}

export function buildIssuePath(route: IssueRoute): string {
  const issue = `/issues/${route.key}`;
  if (route.kind === "issue") {
    return issue;
  }
  if (route.kind === "spec") {
    return `${issue}/spec`;
  }
  if (route.kind === "conversation") {
    return `${issue}/conversation`;
  }
  if (route.kind === "children") {
    return `${issue}/children`;
  }
  if (route.kind === "artifacts") {
    return `${issue}/artifacts`;
  }
  if (route.kind === "artifact") {
    const artifact = `${issue}/artifacts/${encodeURIComponent(route.slug)}`;
    return route.version === undefined ? artifact : `${artifact}?v=${route.version}`;
  }
  const segment =
    route.kind === "ask" ? "asks" : route.kind === "comment" ? "comments" : "messages";
  return `${issue}/${segment}/${encodeURIComponent(route.id)}`;
}

export function buildProjectPath(route: ProjectRoute): string {
  const project = `/projects/${route.project}`;
  if (route.kind === "project") {
    return project;
  }
  if (route.kind === "architecture" || route.kind === "issues" || route.kind === "documents") {
    return `${project}/${route.kind}`;
  }
  const document = `${project}/documents/${encodeURIComponent(route.slug)}`;
  const query = new URLSearchParams();
  if (route.version !== undefined) {
    query.set("version", String(route.version));
  }
  if (route.item !== undefined) {
    query.set(route.item.kind, route.item.id);
  }
  const search = query.toString();
  return search === "" ? document : `${document}?${search}`;
}

export function parseInboxSearch(search: string): InboxRoute {
  const params = new URLSearchParams(search);
  const agent = params.get("agent");
  const route: InboxRoute = {};
  if (agent !== null && agent !== "") {
    route.agent = agent;
  }
  if (params.get("section") === "needs-you") {
    route.section = "needs-you";
  }
  const view = params.get("view");
  if (view === "mine" || view === "everyone") {
    route.view = view;
  }
  return route;
}

export function buildInboxPath(route: InboxRoute = {}): string {
  const query = new URLSearchParams();
  if (route.agent !== undefined) {
    query.set("agent", route.agent);
  }
  if (route.section !== undefined) {
    query.set("section", route.section);
  }
  if (route.view !== undefined) {
    query.set("view", route.view);
  }
  const search = query.toString();
  return search === "" ? "/" : `/?${search}`;
}

/**
 * Whether this route has a margin at all: an issue, or a standalone project document. Nothing
 * else does, so the shell reserves no gutter for one and `features/margin/Margin.tsx` renders
 * nothing there. One helper because two answers that disagree leave an 80 px gutter beside a
 * column that is not there. It answers from the route rather than from the resolved margin
 * owner, whose document form only appears once the artifact query lands.
 */
export function routeHasMargin(pathname: string, search = ""): boolean {
  return (
    parseIssuePath(pathname, search) !== undefined ||
    parseProjectPath(pathname, search)?.kind === "document"
  );
}

/** The live agent view's route pattern, shared by the router and `routeFillsViewport`. */
export const AGENT_LIVE_PATH = "/agents/:sessionId/live";

/**
 * Whether this route's page owns its scroller and so gets exactly the viewport from the shell:
 * the live agent view, whose thread scrolls while its header and composer stay put. Every other
 * page scrolls the document, which `ViewportAnchor` and the sticky composers depend on.
 */
export function routeFillsViewport(pathname: string): boolean {
  return matchPath(AGENT_LIVE_PATH, pathname) !== null;
}
