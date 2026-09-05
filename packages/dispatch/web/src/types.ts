export type Urgency = "low" | "med" | "high" | "blocking";
export type IssueState = "OPEN" | "CLOSED";

/** Why a thread was closed, as GitHub's REST API names it. */
export type CloseReason = "completed" | "not_planned";

/**
 * Coding-agent hosts that ship a `dispatch` tool. Mirrors `DispatchHost` in
 * envoy-client, the only producer of the field; a marker naming anything else
 * is treated as having no host.
 */
export type OriginHost = "omp" | "opencode" | "claude";

export interface Origin {
  host?: OriginHost;
  machine?: string;
  cwd?: string;
  tmux?: string;
  pane?: string;
  sessionId?: string;
  sessionTitle?: string;
}

export interface Thread {
  repo: string;
  number: number;
  title: string;
  body: string;
  state: IssueState;
  urgency: Urgency;
  hasAsk: boolean;
  parentNumber: number;
  updatedAt: string;
  createdAt: string;
  authorLogin: string;
  commentCount: number;
  origin?: Origin;
}

export interface Issue {
  repo: string;
  number: number;
  title: string;
  body: string;
  state: IssueState;
  stateReason: string | null;
  updatedAt: string;
  createdAt: string;
  authorLogin: string;
}

export interface Comment {
  id: number;
  body: string;
  createdAt: string;
  updatedAt: string;
  authorLogin: string;
}

export type StatusFilter = "all" | "open" | "closed";
export type UrgencyFilter = "all" | Urgency;

export interface SidebarFilters {
  status: StatusFilter;
  urgency: UrgencyFilter;
  search: string;
  showAddressed: boolean;
  selectedKey?: string;
  highlightedKeys?: Set<string>;
  // Set when the thread search itself failed; the sidebar shows the error
  // instead of an empty list that reads as "nothing needs you".
  loadError?: string;
  // Map of "<repo>#<n>" → ISO timestamp of thread.updatedAt at mark time.
  // The sidebar treats a thread as addressed when its updatedAt is <=
  // this stored timestamp. Pass-through to the renderer; the filter logic
  // lives in visibleSidebarThreads.
  addressed?: Record<string, string>;
}

export interface SidebarEntry {
  thread: Thread;
  groupNumber: number;
  subThreadCount: number;
  parentInList: boolean;
  addressed: boolean;
}
