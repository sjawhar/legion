import {
  closeIssue,
  getComments,
  getInstallationOwners,
  getIssue,
  isCloseReason,
  openGithubEventSource,
  postComment,
  searchDispatchThreads,
} from "./api";
import { summarizeAnswers } from "./components/ask-form";
import {
  isStatusFilter,
  isUrgencyFilter,
  renderSidebar,
  visibleSidebarThreads,
} from "./components/sidebar";
import { renderThreadDetail, type ThreadDetailInput } from "./components/thread-detail";
import {
  buildAnswerMarkerComment,
  buildUrgencyMarkerComment,
  effectiveUrgency,
  isUrgency,
  parseMetaMarker,
} from "./markers";
import "./styles.css";
import type { QuestionAnswer } from "@opencode-ai/sdk/v2";
import type {
  CloseReason,
  Comment,
  Issue,
  SidebarEntry,
  SidebarFilters,
  Thread,
  Urgency,
} from "./types";

export interface AppApi {
  searchDispatchThreads: typeof searchDispatchThreads;
  getIssue: typeof getIssue;
  getComments: typeof getComments;
  postComment: typeof postComment;
  closeIssue: typeof closeIssue;
}

export interface DashboardControllerOptions {
  owners: string[];
  addressed?: Record<string, string>;
  api?: AppApi;
}

// Selected = (repo, number); we can't just key by number because the same
// issue number can appear in different repos.
interface Selected {
  repo: string;
  number: number;
}

// keyOf produces a composite key for state.issues / state.comments. Using
// strings keeps the existing Map<…> shapes; the format matches the parent
// ref the agent uses ("<owner>/<repo>#<n>").
function keyOf(repo: string, number: number): string {
  return `${repo}#${number}`;
}

interface DashboardState {
  threads: Thread[];
  comments: Map<string, Comment[]>;
  issues: Map<string, Issue>;
  selected?: Selected;
  filters: SidebarFilters;
  // addressed[<repo>#<n>] = ISO timestamp of thread.updatedAt at mark time.
  // Sidebar hides addressed threads until new activity advances updatedAt
  // past this marker (or showAddressed=true on the filter).
  addressed: Record<string, string>;
  sidebarOpen: boolean;
  helpOpen: boolean;
  highlighted: Set<string>;
  replyPending: boolean;
  replyError?: string;
  askPending: boolean;
  askError?: string;
  urgencyPending: boolean;
  urgencyError?: string;
  closePending: boolean;
  closeError?: string;
  addressedPending: boolean;
  addressedError?: string;
  loadError?: string;
}

const defaultApi: AppApi = {
  searchDispatchThreads,
  getIssue,
  getComments,
  postComment,
  closeIssue,
};

export function renderAppShell(): string {
  return `<div class="app-shell">
    <div id="auth-overlay" class="auth-overlay" hidden>
      <div class="auth-card">
        <h1>Dispatch</h1>
        <p>Sign in with your GitHub account to read dispatch threads.</p>
        <a class="sign-in-link" href="/auth/start">Sign in with GitHub</a>
        <p id="auth-hint" class="auth-hint" hidden></p>
      </div>
    </div>
    <div id="owner-error-overlay" class="auth-overlay" hidden>
      <div class="auth-card">
        <h1>Dispatch</h1>
        <p>The Envoy GitHub App is not installed anywhere you can see. Install it on your account or an org you belong to, then reload.</p>
      </div>
    </div>
    <header class="topbar">
      <button type="button" id="toggle-sidebar" title="Toggle sidebar ([ or ])">☰</button>
      <strong>Dispatch</strong>
      <span id="owner-label" class="owner-label"></span>
      <button type="button" id="help-button" title="Keyboard shortcuts (?)">?</button>
    </header>
    <div id="dashboard-root"></div>
    <div id="shortcut-modal" class="shortcut-modal" hidden>
      <div><strong>Shortcuts</strong></div>
      <p><kbd>j</kbd>/<kbd>k</kbd> move, <kbd>Enter</kbd> select, <kbd>[</kbd>/<kbd>]</kbd> toggle sidebar, <kbd>?</kbd> help.</p>
    </div>
  </div>`;
}

export function createDashboardController(options: DashboardControllerOptions) {
  const api = options.api ?? defaultApi;
  const state: DashboardState = {
    threads: [],
    comments: new Map(),
    issues: new Map(),
    filters: { status: "open", urgency: "all", search: "", showAddressed: false },
    addressed: { ...(options.addressed ?? {}) },
    sidebarOpen: true,
    helpOpen: false,
    highlighted: new Set(),
    replyPending: false,
    askPending: false,
    urgencyPending: false,
    closePending: false,
    addressedPending: false,
  };

  function selectedKey(): string | undefined {
    return state.selected ? keyOf(state.selected.repo, state.selected.number) : undefined;
  }

  function isThreadAddressed(repo: string, number: number, updatedAt: string): boolean {
    const marker = state.addressed[`${repo}#${number}`];
    if (!marker) return false;
    return new Date(updatedAt).getTime() <= new Date(marker).getTime();
  }

  function selectedDetail(): ThreadDetailInput | null {
    const selected = state.selected;
    if (!selected) return null;
    const key = keyOf(selected.repo, selected.number);
    const issue = state.issues.get(key);
    if (!issue) return null;
    const thread = state.threads.find(
      (candidate) => candidate.repo === selected.repo && candidate.number === selected.number
    );
    return {
      issue,
      urgency: effectiveUrgency(thread?.urgency ?? "med", state.comments.get(key) ?? []),
      comments: state.comments.get(key) ?? [],
      subThreads: state.threads.filter(
        (candidate) => candidate.repo === issue.repo && candidate.parentNumber === issue.number
      ),
      repo: issue.repo,
      addressed: isThreadAddressed(issue.repo, issue.number, issue.updatedAt),
      writeState: {
        replyPending: state.replyPending,
        replyError: state.replyError,
        askPending: state.askPending,
        askError: state.askError,
        urgencyPending: state.urgencyPending,
        urgencyError: state.urgencyError,
        closePending: state.closePending,
        closeError: state.closeError,
        addressedPending: state.addressedPending,
        addressedError: state.addressedError,
      },
    };
  }

  function requireSelected(): Selected {
    if (!state.selected) throw new Error("No thread selected");
    return state.selected;
  }

  function selectedComments(): Comment[] {
    const key = selectedKey();
    if (!key) throw new Error("No thread selected");
    const comments = state.comments.get(key) ?? [];
    state.comments.set(key, comments);
    return comments;
  }

  function selectedIssue(): Issue {
    const key = selectedKey();
    if (!key) throw new Error("Selected issue is not loaded");
    const issue = state.issues.get(key);
    if (!issue) throw new Error("Selected issue is not loaded");
    return issue;
  }

  function replaceComment(key: string, placeholderId: number, comment: Comment): void {
    state.comments.set(
      key,
      (state.comments.get(key) ?? []).map((candidate) =>
        candidate.id === placeholderId ? comment : candidate
      )
    );
  }

  function removeComment(key: string, placeholderId: number): void {
    state.comments.set(
      key,
      (state.comments.get(key) ?? []).filter((candidate) => candidate.id !== placeholderId)
    );
  }

  function optimisticComment(body: string): Comment {
    const timestamp = new Date().toISOString();
    return {
      id: -Date.now(),
      body,
      createdAt: timestamp,
      updatedAt: timestamp,
      authorLogin: "you",
    };
  }

  function render(): string {
    const filters = sidebarFilters();
    return `<div class="dashboard-root${state.sidebarOpen ? "" : " sidebar-collapsed"}">
      ${state.sidebarOpen ? renderSidebar(state.threads, filters) : ""}
      ${renderThreadDetail(selectedDetail())}
      ${state.helpOpen ? `<div class="shortcut-modal active">j/k move · Enter select · [/ ] sidebar · ? help</div>` : ""}
    </div>`;
  }

  async function loadThreads(): Promise<void> {
    // Owner-scoped search spans every repo the App is installed on for
    // these accounts in one GraphQL call. Threads already carry their repo
    // (derived from the GraphQL node), so the result is repo-aware end to end.
    // A failed search keeps the last good list and shows the error; an empty
    // sidebar must never stand in for "the search broke".
    try {
      state.threads = await api.searchDispatchThreads(options.owners);
      state.loadError = undefined;
    } catch (error) {
      state.loadError = error instanceof Error ? error.message : String(error);
    }
    if (!state.selected) {
      const first = visibleSidebarThreads(state.threads, sidebarFilters())[0];
      if (first) {
        state.selected = { repo: first.thread.repo, number: first.thread.number };
      }
    }
  }

  async function selectThread(repo: string, number: number): Promise<void> {
    state.selected = { repo, number };
    updateUrlForSelection({ repo, number });
    const [issue, comments] = await Promise.all([
      api.getIssue(repo, number),
      api.getComments(repo, number),
    ]);
    const key = keyOf(repo, number);
    state.issues.set(key, issue);
    state.comments.set(key, comments);
  }

  function sidebarFilters(): SidebarFilters {
    return {
      ...state.filters,
      selectedKey: selectedKey(),
      highlightedKeys: state.highlighted,
      addressed: state.addressed,
      loadError: state.loadError,
    };
  }

  function visibleThreads(): SidebarEntry[] {
    return visibleSidebarThreads(state.threads, sidebarFilters());
  }

  function nextSelection(direction: "j" | "k"): Selected | undefined {
    const entries = visibleThreads();
    if (entries.length === 0) return undefined;
    const selKey = selectedKey();
    const current = entries.findIndex(
      (entry) => keyOf(entry.thread.repo, entry.thread.number) === selKey
    );
    const next =
      direction === "j"
        ? current < entries.length - 1
          ? current + 1
          : 0
        : current > 0
          ? current - 1
          : entries.length - 1;
    const nextThread = entries[next]?.thread;
    if (!nextThread) return undefined;
    state.selected = { repo: nextThread.repo, number: nextThread.number };
    return state.selected;
  }

  function toggleSidebar(): boolean {
    state.sidebarOpen = !state.sidebarOpen;
    return state.sidebarOpen;
  }

  function toggleHelp(): boolean {
    state.helpOpen = !state.helpOpen;
    return state.helpOpen;
  }

  function highlightThread(repo: string, number: number): void {
    const key = keyOf(repo, number);
    state.highlighted.add(key);
    setTimeout(() => state.highlighted.delete(key), 1800);
  }

  async function autoMarkAddressed(key: string, timestamp: string): Promise<void> {
    // Best-effort auto-mark after a successful reply/answer. Quiet on
    // failure — the explicit Mark Addressed button surfaces errors.
    const previous = { ...state.addressed };
    state.addressed = { ...state.addressed, [key]: timestamp };
    try {
      await persistAddressed(state.addressed);
    } catch (error) {
      state.addressed = previous;
      console.warn("auto-mark addressed failed", error);
    }
  }

  async function postReply(body: string): Promise<void> {
    const trimmed = body.trim();
    if (!trimmed) return;
    const sel = requireSelected();
    const key = keyOf(sel.repo, sel.number);
    const placeholder = optimisticComment(trimmed);
    selectedComments().push(placeholder);
    state.replyPending = true;
    state.replyError = undefined;
    try {
      const comment = await api.postComment(sel.repo, sel.number, trimmed);
      replaceComment(key, placeholder.id, comment);
      // Replying counts as "I've handled this for now" — auto-mark addressed
      // with the server-confirmed comment timestamp. Anything newer than this
      // resurfaces the thread.
      void autoMarkAddressed(key, comment.createdAt);
    } catch (error) {
      removeComment(key, placeholder.id);
      state.replyError = error instanceof Error ? error.message : String(error);
      throw error;
    } finally {
      state.replyPending = false;
    }
  }

  async function submitAskAnswer(answers: QuestionAnswer[]): Promise<void> {
    const issue = selectedIssue();
    const key = keyOf(issue.repo, issue.number);
    const ask = parseMetaMarker(issue.body)?.ask ?? [];
    const summary = summarizeAnswers(ask, answers);
    const body = buildAnswerMarkerComment(issue.number, answers, summary);
    const placeholder = optimisticComment(body);
    selectedComments().push(placeholder);
    state.askPending = true;
    state.askError = undefined;
    try {
      const comment = await api.postComment(issue.repo, issue.number, body);
      replaceComment(key, placeholder.id, comment);
      // Answering a question — same auto-mark as replying.
      void autoMarkAddressed(key, comment.createdAt);
    } catch (error) {
      removeComment(key, placeholder.id);
      state.askError = error instanceof Error ? error.message : String(error);
      throw error;
    } finally {
      state.askPending = false;
    }
  }

  async function setUrgency(urgency: Urgency): Promise<void> {
    const sel = requireSelected();
    const body = buildUrgencyMarkerComment(urgency);
    const thread = state.threads.find(
      (candidate) => candidate.repo === sel.repo && candidate.number === sel.number
    );
    const previousUrgency = thread?.urgency;
    if (thread) thread.urgency = urgency;
    state.urgencyPending = true;
    state.urgencyError = undefined;
    try {
      const comment = await api.postComment(sel.repo, sel.number, body);
      selectedComments().push(comment);
    } catch (error) {
      if (thread && previousUrgency) thread.urgency = previousUrgency;
      state.urgencyError = error instanceof Error ? error.message : String(error);
      throw error;
    } finally {
      state.urgencyPending = false;
    }
  }

  async function closeSelectedIssue(stateReason: CloseReason): Promise<void> {
    const sel = requireSelected();
    const key = keyOf(sel.repo, sel.number);
    const issue = selectedIssue();
    const previous = { ...issue };
    state.issues.set(key, { ...issue, state: "CLOSED", stateReason });
    state.closePending = true;
    state.closeError = undefined;
    try {
      state.issues.set(key, await api.closeIssue(sel.repo, sel.number, stateReason));
    } catch (error) {
      state.issues.set(key, previous);
      state.closeError = error instanceof Error ? error.message : String(error);
      throw error;
    } finally {
      state.closePending = false;
    }
  }

  async function markAddressed(): Promise<void> {
    const sel = requireSelected();
    const issue = selectedIssue();
    const key = keyOf(sel.repo, sel.number);
    const previous = { ...state.addressed };
    state.addressed = { ...state.addressed, [key]: issue.updatedAt };
    state.addressedPending = true;
    state.addressedError = undefined;
    try {
      await persistAddressed(state.addressed);
    } catch (error) {
      state.addressed = previous;
      state.addressedError = error instanceof Error ? error.message : String(error);
      throw error;
    } finally {
      state.addressedPending = false;
    }
  }

  async function unmarkAddressed(): Promise<void> {
    const sel = requireSelected();
    const key = keyOf(sel.repo, sel.number);
    if (!(key in state.addressed)) return;
    const previous = { ...state.addressed };
    const next = { ...state.addressed };
    delete next[key];
    state.addressed = next;
    state.addressedPending = true;
    state.addressedError = undefined;
    try {
      await persistAddressed(state.addressed);
    } catch (error) {
      state.addressed = previous;
      state.addressedError = error instanceof Error ? error.message : String(error);
      throw error;
    } finally {
      state.addressedPending = false;
    }
  }

  function setShowAddressed(value: boolean): void {
    state.filters.showAddressed = value;
  }

  return {
    state,
    render,
    loadThreads,
    selectThread,
    visibleThreads,
    nextSelection,
    toggleSidebar,
    toggleHelp,
    highlightThread,
    postReply,
    submitAskAnswer,
    setUrgency,
    closeSelectedIssue,
    markAddressed,
    unmarkAddressed,
    setShowAddressed,
  };
}

async function ensureSignedIn(): Promise<boolean> {
  const response = await fetch("/auth/whoami");
  return response.ok;
}

function attachDom(
  controller: ReturnType<typeof createDashboardController>,
  root: HTMLElement
): () => void {
  function paint(): void {
    root.querySelector("#sidebar-root")?.replaceChildren();
    root.querySelector("#detail-root")?.replaceChildren();
    const dashboard = root.querySelector("#dashboard-root");
    if (dashboard) dashboard.innerHTML = controller.render();
  }

  root.addEventListener("click", (event) => {
    const target = event.target as HTMLElement;
    const row = target.closest<HTMLElement>("button[data-thread-number]");
    const repo = row?.dataset.threadRepo;
    const number = row?.dataset.threadNumber;
    if (repo && number) {
      void controller.selectThread(repo, Number(number)).then(paint);
      return;
    }
    if (target.closest<HTMLElement>("#help-button")) {
      controller.toggleHelp();
      paint();
      return;
    }
    if (target.closest<HTMLElement>("#toggle-sidebar")) {
      controller.toggleSidebar();
      paint();
      return;
    }
    const pill = target.closest<HTMLButtonElement>("[data-filter]");
    const filterValue = pill?.dataset.value;
    if (pill?.dataset.filter === "status" && isStatusFilter(filterValue)) {
      controller.state.filters.status = filterValue;
      paint();
    }
    if (pill?.dataset.filter === "urgency" && isUrgencyFilter(filterValue)) {
      controller.state.filters.urgency = filterValue;
      paint();
    }
  });

  root.addEventListener("input", (event) => {
    const target = event.target as HTMLInputElement;
    if (target.id !== "search-input") return;
    controller.state.filters.search = target.value;
    paint();
  });

  root.addEventListener("click", (event) => {
    const target = event.target as HTMLElement;
    const urgencyOption = target.closest<HTMLButtonElement>("button[data-urgency-value]");
    if (!urgencyOption) return;
    const next = urgencyOption.dataset.urgencyValue;
    if (!isUrgency(next)) return;
    // Close the popover so the chip reflects the new state immediately.
    const details = urgencyOption.closest<HTMLDetailsElement>("details.urgency-chip-wrap");
    if (details) details.open = false;
    void controller.setUrgency(next).then(paint, paint);
    paint();
  });

  // Click-outside to close the urgency popover and the resolve-as-not-planned menu.
  document.addEventListener("click", (event) => {
    const target = event.target as HTMLElement;
    if (!target.closest(".urgency-chip-wrap")) {
      for (const open of root.querySelectorAll<HTMLDetailsElement>(
        "details.urgency-chip-wrap[open]"
      )) {
        open.open = false;
      }
    }
    if (!target.closest(".resolve-menu-wrap")) {
      for (const open of root.querySelectorAll<HTMLDetailsElement>(
        "details.resolve-menu-wrap[open]"
      )) {
        open.open = false;
      }
    }
  });

  root.addEventListener("submit", (event) => {
    const form = (event.target as HTMLElement).closest<HTMLFormElement>("form[data-action]");
    if (!form) return;
    event.preventDefault();
    const formData = new FormData(form);
    if (form.dataset.action === "reply") {
      void controller.postReply(String(formData.get("body") ?? "")).then(paint, paint);
      paint();
    }
    if (form.dataset.action === "ask-answer") {
      const count = Number(form.dataset.questionCount ?? "0");
      const answers: QuestionAnswer[] = Array.from({ length: count }, (_, index) => {
        const custom = String(formData.get(`custom-${index}`) ?? "").trim();
        if (formData.has(`custom-enabled-${index}`) && custom) return [custom];
        return formData.getAll(`answer-${index}`).map(String).filter(Boolean);
      });
      void controller.submitAskAnswer(answers).then(paint, paint);
      paint();
    }
  });

  root.addEventListener("click", (event) => {
    const target = event.target as HTMLElement;
    const button = target.closest<HTMLButtonElement>("button[data-action='close']");
    const stateReason = button?.dataset.stateReason;
    if (!isCloseReason(stateReason)) return;
    // Close the resolve-as-not-planned menu when picking from it.
    for (const open of root.querySelectorAll<HTMLDetailsElement>(
      "details.resolve-menu-wrap[open]"
    )) {
      open.open = false;
    }
    void controller.closeSelectedIssue(stateReason).then(paint, paint);
    paint();
  });

  root.addEventListener("click", (event) => {
    const target = event.target as HTMLElement;
    if (target.closest<HTMLButtonElement>("button[data-action='mark-addressed']")) {
      void controller.markAddressed().then(paint, paint);
      paint();
      return;
    }
    if (target.closest<HTMLButtonElement>("button[data-action='unmark-addressed']")) {
      void controller.unmarkAddressed().then(paint, paint);
      paint();
    }
  });

  root.addEventListener("click", (event) => {
    const target = event.target as HTMLElement;
    const copyButton = target.closest<HTMLButtonElement>("button[data-action='copy-origin']");
    const payload = copyButton?.dataset.copyText;
    if (payload) void navigator.clipboard.writeText(payload);
  });

  root.addEventListener("click", (event) => {
    const target = event.target as HTMLElement;
    const toggle = target.closest<HTMLButtonElement>("button[data-toggle]");
    if (!toggle) return;
    if (toggle.dataset.toggle === "show-addressed") {
      controller.setShowAddressed(true);
      paint();
    } else if (toggle.dataset.toggle === "hide-addressed") {
      controller.setShowAddressed(false);
      paint();
    }
  });

  document.addEventListener("keydown", (event) => {
    if (event.target instanceof HTMLInputElement || event.target instanceof HTMLTextAreaElement)
      return;
    if (event.key === "j" || event.key === "k") {
      event.preventDefault();
      const next = controller.nextSelection(event.key);
      if (next) void controller.selectThread(next.repo, next.number).then(paint);
    }
    if (event.key === "Enter" && controller.state.selected) {
      event.preventDefault();
      const sel = controller.state.selected;
      void controller.selectThread(sel.repo, sel.number).then(paint);
    }
    if (event.key === "[" || event.key === "]") {
      event.preventDefault();
      controller.toggleSidebar();
      paint();
    }
    if (event.key === "?") {
      event.preventDefault();
      controller.toggleHelp();
      paint();
    }
  });

  window.addEventListener("hashchange", () => {
    const sel = parseSelectionFromUrl();
    if (!sel) return;
    const cur = controller.state.selected;
    if (cur && cur.repo === sel.repo && cur.number === sel.number) return;
    void controller.selectThread(sel.repo, sel.number).then(paint);
  });

  paint();
  return paint;
}

async function boot(): Promise<void> {
  const app = document.getElementById("app");
  if (!app) throw new Error("Missing #app root");
  app.innerHTML = renderAppShell();

  const signedIn = await ensureSignedIn();
  if (!signedIn) {
    document.getElementById("auth-overlay")?.removeAttribute("hidden");
    return;
  }

  const owners = await getInstallationOwners();
  renderOwnerLabel(owners);
  if (owners.length === 0) {
    document.getElementById("owner-error-overlay")?.removeAttribute("hidden");
    return;
  }
  // GitHub logins are case-insensitive; a hand-typed URL hash or an event's
  // NATS subject may not match the installation's casing.
  const ownerSet = new Set(owners.map((owner) => owner.toLowerCase()));
  const covers = (repo: string): boolean => ownerSet.has(repo.split("/")[0]?.toLowerCase() ?? "");

  const view = await fetchView();
  const controller = createDashboardController({
    owners,
    addressed: view.addressed,
  });
  await controller.loadThreads();
  const fromUrl = parseSelectionFromUrl();
  if (fromUrl && covers(fromUrl.repo)) {
    await controller.selectThread(fromUrl.repo, fromUrl.number);
  } else if (controller.state.selected) {
    await controller.selectThread(controller.state.selected.repo, controller.state.selected.number);
  }
  const paint = attachDom(controller, app);
  openGithubEventSource({
    refetchSidebar: async () => {
      await controller.loadThreads();
      paint();
    },
    refetchComments: async (repo, number) => {
      if (!covers(repo)) return;
      const key = `${repo}#${number}`;
      const fresh = await getComments(repo, number);
      controller.state.comments.set(key, fresh);
      const thread = controller.state.threads.find(
        (candidate) => candidate.repo === repo && candidate.number === number
      );
      if (thread) thread.urgency = effectiveUrgency(thread.urgency, fresh);
      paint();
    },
    refetchIssue: async (repo, number) => {
      if (!covers(repo)) return;
      const key = `${repo}#${number}`;
      const issue = await getIssue(repo, number);
      controller.state.issues.set(key, issue);
      const thread = controller.state.threads.find((t) => t.repo === repo && t.number === number);
      if (thread) thread.state = issue.state;
      paint();
    },
    highlightThread: (repo, number) => controller.highlightThread(repo, number),
  });
}

function renderOwnerLabel(owners: string[]): void {
  const label = document.getElementById("owner-label");
  if (!label) return;
  label.textContent = owners.join(" · ");
  label.title = owners.join("\n");
}

// URL hash format: #<owner>/<repo>/<number>. Plain string, no encoding
// (GitHub-style repo slugs don't contain reserved chars). Used so back/
// forward browser navigation moves between threads without a full reload,
// and so a copied URL deep-links to the right thread.
function parseSelectionFromUrl(): { repo: string; number: number } | null {
  if (typeof window === "undefined") return null;
  const raw = window.location.hash.replace(/^#/, "");
  if (!raw) return null;
  const m = raw.match(/^([^/\s]+\/[^/\s]+)\/(\d+)$/);
  if (!m) return null;
  return { repo: m[1] as string, number: Number(m[2]) };
}

function updateUrlForSelection(sel: { repo: string; number: number }): void {
  if (typeof window === "undefined") return;
  const next = `#${sel.repo}/${sel.number}`;
  if (window.location.hash === next) return;
  window.history.replaceState(null, "", next);
}

interface View {
  addressed: Record<string, string>;
}

async function fetchView(): Promise<View> {
  const response = await fetch("/api/view");
  if (!response.ok) return { addressed: {} };
  const data = (await response.json()) as { addressed?: Record<string, string> };
  return { addressed: data.addressed ?? {} };
}

async function persistAddressed(addressed: Record<string, string>): Promise<void> {
  const response = await fetch("/api/view", {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ addressed }),
  });
  if (!response.ok) {
    const body = await response.text().catch(() => "");
    throw new Error(`PATCH /api/view returned ${response.status}: ${body.slice(0, 200)}`);
  }
}

if (typeof document !== "undefined") {
  void boot();
}
