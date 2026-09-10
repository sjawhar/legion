import type { Artifact } from "../../api/types";

export type DispatchRoute =
  | { key: string; kind: "issue" }
  | { key: string; kind: "spec" }
  | { key: string; kind: "log" }
  | { key: string; kind: "children" }
  | { key: string; kind: "artifacts" }
  | { key: string; kind: "artifact"; slug: string; version?: number }
  | { key: string; kind: "ask"; id: string }
  | { key: string; kind: "comment"; id: string };

export type IssueTab = "spec" | "log" | "children" | "artifacts";

export function isPrimaryDocumentArtifactRoute(
  route: DispatchRoute,
  artifact: Pick<Artifact, "kind" | "primary"> | undefined
): boolean {
  return route.kind === "artifact" && artifact?.primary === true && artifact.kind === "doc";
}

export function issueTabForRoute(
  route: DispatchRoute,
  artifact: Pick<Artifact, "kind" | "primary"> | undefined
): IssueTab {
  if (route.kind === "children") {
    return "children";
  }
  if (route.kind === "log") {
    return "log";
  }
  if (route.kind === "artifacts") {
    return "artifacts";
  }
  if (route.kind === "artifact") {
    return isPrimaryDocumentArtifactRoute(route, artifact) ? "spec" : "artifacts";
  }
  return route.kind === "spec" ? "spec" : "log";
}

const issueKeyPattern = "[A-Z][A-Z0-9-]*";

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

function artifactRoute(
  key: string,
  slug: string,
  versionValue?: string | null
): DispatchRoute | undefined {
  const decodedSlug = decodedSegment(slug);
  const parsedVersion = version(versionValue);
  if (decodedSlug === undefined || parsedVersion === null) {
    return undefined;
  }
  return parsedVersion === undefined
    ? { key, kind: "artifact", slug: decodedSlug }
    : { key, kind: "artifact", slug: decodedSlug, version: parsedVersion };
}

export function parseDispatchReference(value: string): DispatchRoute | undefined {
  const match = value.match(new RegExp(`^dispatch://(${issueKeyPattern})(?:/(.*))?$`));
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
  if (target === "log") {
    return { key, kind: "log" };
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
  const item = target.match(/^(ask|comment)\/([^/?#\s]+)$/);
  const id = item?.[2] === undefined ? undefined : decodedSegment(item[2]);
  if (item === null || id === undefined) {
    return undefined;
  }
  return { id, key, kind: item[1] === "ask" ? "ask" : "comment" };
}

export function parseIssuePath(pathname: string, search = ""): DispatchRoute | undefined {
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
  if (target === "log") {
    return { key, kind: "log" };
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
  const item = target.match(/^(asks|comments)\/([^/?#\s]+)$/);
  const id = item?.[2] === undefined ? undefined : decodedSegment(item[2]);
  if (item === null || id === undefined) {
    return undefined;
  }
  return { id, key, kind: item[1] === "asks" ? "ask" : "comment" };
}

export function buildDispatchReference(route: DispatchRoute): string {
  const issue = `dispatch://${route.key}`;
  if (route.kind === "issue") {
    return issue;
  }
  if (route.kind === "spec") {
    return `${issue}/spec`;
  }
  if (route.kind === "log") {
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

export function buildIssuePath(route: DispatchRoute): string {
  const issue = `/issues/${route.key}`;
  if (route.kind === "issue") {
    return issue;
  }
  if (route.kind === "spec") {
    return `${issue}/spec`;
  }
  if (route.kind === "log") {
    return `${issue}/log`;
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
  return `${issue}/${route.kind === "ask" ? "asks" : "comments"}/${encodeURIComponent(route.id)}`;
}
