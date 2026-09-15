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

export type ProjectRoute =
  | { kind: "project"; project: string }
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

/** Agents reference the conversation as `dispatch://KEY/log`; the browser path is `/conversation`. */
const legacyLogReferencePattern = /^log$/;

export function isProjectRoute(route: DispatchRoute): route is ProjectRoute {
  return route.kind === "project" || route.kind === "documents" || route.kind === "document";
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

function decodedSegment(value: string): string | undefined {
  try {
    const decoded = decodeURIComponent(value);
    return decoded === "" ? undefined : decoded;
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

function itemFromQuery(search: string): { kind: "ask" | "comment"; id: string } | undefined | null {
  const params = new URLSearchParams(search);
  const ask = params.get("ask");
  const comment = params.get("comment");
  if (ask === null && comment === null) {
    return undefined;
  }
  if (ask !== null && comment !== null) {
    return null;
  }
  const id = decodedSegment(ask ?? comment ?? "");
  if (id === undefined) {
    return null;
  }
  return ask === null ? { id, kind: "comment" } : { id, kind: "ask" };
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
  const artifact = target.match(/^artifact\/([^@/?#\s]+)(?:@v([1-9]\d*))?$/);
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

export function parseDispatchReference(value: string): DispatchReferenceRoute | undefined {
  const match = value.match(/^dispatch:\/\/([^/]+)(?:\/(.*))?$/);
  if (match === null) {
    return undefined;
  }
  const key = match[1];
  const target = match[2];
  if (key === undefined) {
    return undefined;
  }
  if (new RegExp(`^${issueKeyPattern}$`).test(key)) {
    return parseIssueReference(key, target);
  }
  if (!new RegExp(`^${projectKeyPattern}$`).test(key) || target === undefined) {
    return undefined;
  }
  const document = target.match(
    /^artifact\/([^@/?#\s]+)(?:@v([1-9]\d*))?(?:\/(ask|comment)\/([^/?#\s]+))?$/
  );
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
  const match = pathname.match(new RegExp(`^/issues/(${issueKeyPattern})(?:/(.*))?$`));
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
  const match = pathname.match(new RegExp(`^/projects/(${projectKeyPattern})(?:/(.*))?$`));
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
  if (target === "documents") {
    return { kind: "documents", project };
  }
  const document = target.match(/^documents\/([^/?#\s]+)$/);
  if (document === null) {
    return undefined;
  }
  const item = itemFromQuery(search);
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
  if (route.kind === "documents") {
    return `${project}/documents`;
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
