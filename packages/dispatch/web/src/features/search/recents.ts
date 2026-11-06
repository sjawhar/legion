/**
 * The person's recent items: the issues and project documents they opened in this browser, newest
 * first, which the bar lists while its box is empty.
 *
 * They are kept in the browser and nowhere else. Dispatch has no server store for them, and the
 * bar must paint the moment the key is pressed, so a keystroke that opens it makes no request:
 * the list is read from `localStorage` on the render that opens the bar. The key is the one every
 * browser-only preference uses (`userPreferenceStorageKey`), so one person's recent items are
 * private to their own signed-in identity and a sign-in as someone else lists theirs.
 *
 * What counts as recent is what the person opened: a visit to an issue page or to a project
 * document records that item, whether they reached it from the bar, from a link or from the
 * address bar, and an item opened again moves back to the head of the list. `RECENT_LIMIT` of
 * them are kept, the rest fall off the end.
 */
import { useQuery } from "@tanstack/react-query";
import { useEffect } from "react";
import { useLocation } from "react-router-dom";

import { issueQuery, whoAmIQuery } from "../../api/queries";
import { useProjectArtifact } from "../document/useProjectArtifact";
import { buildIssuePath, buildProjectPath, parseIssuePath } from "../refs/routes";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { documentRouteOf } from "./bar-scope";

/** How many recent items the browser keeps, which is also how many the bar lists: enough to hold
 *  a morning's work, short enough that the list a person opens is still one glance. */
export const RECENT_LIMIT = 8;

export interface RecentItem {
  /** What the row reads: an issue's key and title, a document's name. */
  label: string;
  /** Where the row goes, and what one item is: a second visit to it moves it, never doubles it. */
  path: string;
}

function storageKey(login: string): string {
  return userPreferenceStorageKey(login, "search.recents");
}

/** What a stored row holds, read defensively: another version of Dispatch, or a person's own
 *  editing of their storage, must not leave the bar unable to open. */
function storedItems(value: string | null): RecentItem[] {
  if (value === null) {
    return [];
  }
  try {
    const parsed: unknown = JSON.parse(value);
    if (!Array.isArray(parsed)) {
      return [];
    }
    return parsed.filter(
      (item): item is RecentItem =>
        typeof item === "object" &&
        item !== null &&
        typeof (item as RecentItem).label === "string" &&
        typeof (item as RecentItem).path === "string"
    );
  } catch {
    return [];
  }
}

/** The person's recent items, newest first. Nobody signed in has none. */
export function readRecents(login: string | undefined): RecentItem[] {
  return login === undefined
    ? []
    : storedItems(window.localStorage.getItem(storageKey(login))).slice(0, RECENT_LIMIT);
}

/** Puts `item` at the head of the person's list, dropping an older visit to the same page and
 *  anything past `RECENT_LIMIT`. A browser refusing the write (private mode, a full quota) costs
 *  the reader their recent items and nothing else. */
export function recordRecent(login: string | undefined, item: RecentItem): void {
  if (login === undefined) {
    return;
  }
  const kept = [item, ...readRecents(login).filter((older) => older.path !== item.path)].slice(
    0,
    RECENT_LIMIT
  );
  try {
    window.localStorage.setItem(storageKey(login), JSON.stringify(kept));
  } catch {
    // A browser that will not store them lists none.
  }
}

/**
 * Records the issue or project document the page is on, for the bar to list back. Mounted once in
 * the shell, so every way into an item - a link, the bar itself, a reload, the address bar -
 * records it.
 *
 * Both reads here are the page's own: the issue page and the document page ask for the same
 * query keys, so this adds no request, and the title or name is written into the row as soon as
 * either answers.
 */
export function useRecordRecents(): void {
  const login = useQuery(whoAmIQuery()).data?.login;
  const location = useLocation();
  const issueRoute = parseIssuePath(location.pathname, location.search);
  const issueKey = issueRoute?.key;
  const documentRoute = documentRouteOf(location.pathname, location.search);
  const issue = useQuery({ ...issueQuery(issueKey), enabled: issueKey !== undefined });
  const artifact = useProjectArtifact(documentRoute);
  const issueTitle = issue.data?.title;
  const documentName = artifact.data?.name;
  const documentProject = documentRoute?.project;
  const documentSlug = documentRoute?.slug;

  useEffect(() => {
    if (issueKey !== undefined) {
      recordRecent(login, {
        label: issueTitle === undefined ? issueKey : `${issueKey} ${issueTitle}`,
        path: buildIssuePath({ key: issueKey, kind: "issue" }),
      });
      return;
    }
    if (documentProject !== undefined && documentSlug !== undefined) {
      recordRecent(login, {
        label: documentName ?? documentSlug,
        path: buildProjectPath({
          kind: "document",
          project: documentProject,
          slug: documentSlug,
        }),
      });
    }
  }, [documentName, documentProject, documentSlug, issueKey, issueTitle, login]);
}
