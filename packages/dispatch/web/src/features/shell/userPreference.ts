export type UserPreference =
  | "agents.pinned"
  | "project.board-edges"
  | "project.issue-filters"
  | "project.issue-view"
  | "shell.margin"
  | "shell.margin-width"
  | "shell.sidebar";

/** Browser-only preferences are private to the signed-in Dispatch identity. */
export function userPreferenceStorageKey(login: string, preference: UserPreference): string {
  return `dispatch.${preference}:${login}`;
}
