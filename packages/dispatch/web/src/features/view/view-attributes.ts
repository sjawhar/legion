import {
  type DispatchReferenceRoute,
  parseNodeReference,
  referencedArtifact,
} from "../refs/routes";

/** The most data files one `view` block names (dispatch://INFRA-682, R1). */
export const MAX_VIEW_DATA_FILES = 8;

/** The owner of a file a `view` block can name: an issue or a project. A file of an agent's
 *  conversation is neither, and no `view` block reads one. */
export type ViewFileOwner = { readonly issue: string } | { readonly project: string };

/** One file a `view` block names: the reference as the author wrote it, the owner and slug it
 *  resolves to, and the version it pins. A reference without `@vN` pins none and follows the
 *  newest version. */
export interface ViewFile {
  readonly owner: ViewFileOwner;
  readonly reference: string;
  readonly route: DispatchReferenceRoute;
  readonly slug: string;
  readonly version: number | undefined;
}

/** Why a `view` block cannot show, as the one line a reader sees: what the line names (a
 *  reference, or the attribute that holds none) and the cause. */
export interface ViewProblem {
  readonly cause: string;
  readonly reference: string;
}

/** What a `view` block's `page` and `data` attributes say: the page file and its one to eight
 *  data files, or the reason the block cannot show. */
export type ViewAttributes =
  | { readonly data: readonly ViewFile[]; readonly kind: "files"; readonly page: ViewFile }
  | { readonly kind: "problem"; readonly problem: ViewProblem };

/** The file a reference names, or undefined when it names no issue file and no project file: an
 *  agent conversation file, an issue, an ask, a comment, a block of a document, or text that is
 *  no reference at all. The reference is the block author's own, and drawing it is a read, so it
 *  is read as a node's reference (`parseNodeReference`), whatever the length of its slug. */
function viewFile(reference: string): ViewFile | undefined {
  const route = parseNodeReference(reference);
  const named = route === undefined ? undefined : referencedArtifact(route);
  if (route === undefined || named === undefined || "session" in named.owner) {
    return undefined;
  }
  return {
    owner: named.owner,
    reference,
    route,
    slug: named.slug,
    version: named.version,
  };
}

function problem(reference: string, cause: string): ViewAttributes {
  return { kind: "problem", problem: { cause, reference } };
}

/** An attribute's references: its words, with one space between two (the spec's decision
 *  "References as one string"). */
const REFERENCE_SEPARATOR = /\s+/u;

/**
 * The one reading of a `view` block's `page` and `data` attributes, used by the editor's node
 * view and by the body renderer, so a block shows the same in a document and in a comment. It
 * answers the page file and its data files, or the first reason the block cannot show: an empty
 * attribute, a reference that names no issue file and no project file (an agent conversation
 * file, a document's own reference, a block of a document), or more than eight data files.
 * Whether a file that a reference does name is there, is a page, or holds text is answered by
 * the reads the block makes, not here.
 */
export function readViewAttributes(page: unknown, data: unknown): ViewAttributes {
  const pageReferences = String(page ?? "")
    .trim()
    .split(REFERENCE_SEPARATOR)
    .filter((word) => word !== "");
  if (pageReferences.length === 0) {
    return problem("page", "this view names no page file");
  }
  if (pageReferences.length > 1) {
    return problem("page", "this view names more than one page file");
  }
  const pageFile = viewFile(pageReferences[0] ?? "");
  if (pageFile === undefined) {
    return problem(pageReferences[0] ?? "", "this is not an issue file or a project file");
  }
  const dataReferences = String(data ?? "")
    .trim()
    .split(REFERENCE_SEPARATOR)
    .filter((word) => word !== "");
  if (dataReferences.length === 0) {
    return problem("data", "this view names no data file");
  }
  if (dataReferences.length > MAX_VIEW_DATA_FILES) {
    return problem(
      "data",
      `this view names ${dataReferences.length} data files, and a view shows at most ${MAX_VIEW_DATA_FILES}`
    );
  }
  const dataFiles: ViewFile[] = [];
  for (const reference of dataReferences) {
    const file = viewFile(reference);
    if (file === undefined) {
      return problem(reference, "this is not an issue file or a project file");
    }
    dataFiles.push(file);
  }
  return { data: dataFiles, kind: "files", page: pageFile };
}
