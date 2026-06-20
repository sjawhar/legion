export type Urgency = "low" | "med" | "high" | "blocking";
export type IssueState = "OPEN" | "CLOSED";

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

export interface SidebarFilters {
  status: "all" | "open" | "closed";
  urgency: "all" | Urgency;
  search: string;
  showAddressed: boolean;
  selectedKey?: string;
  highlightedKeys?: Set<string>;
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
