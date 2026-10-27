import { expect, jest, spyOn, test } from "bun:test";
import { environmentManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Actor, Agent } from "../../api/types";
import { liveDotBg, offlineDotBorder, unknownDotBorder } from "../../theme/classes";
import { useAgents } from "../conversation/useAgents";
import { AgentName, AskResolutionLine, useAgentText } from "./AgentName";
import { AGENT_REGISTRY_POLL_MS, AgentRegistryPoller } from "./agent-registry";

/** One live session, as `GET /api/v1/agents` lists it. */
function listed(sessionId: string, title: string, lastSeen = Date.now() - 3 * 60_000): Agent {
  return {
    session_id: sessionId,
    title,
    dir: "/w",
    host: "host",
    machine_id: "host",
    roles: [],
    capabilities: [],
    last_seen: lastSeen,
    open_asks: 0,
    last_activity: null,
  };
}

function newClient(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

function renderWith(node: ReactNode, client = newClient()) {
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>{node}</QueryClientProvider>
    </MemoryRouter>
  );
  return { client, view };
}

function agentNode(container: HTMLElement, sessionId: string): HTMLElement {
  const node = container.querySelector<HTMLElement>(`[data-agent-name="${sessionId}"]`);
  if (node === null) {
    throw new Error(`no AgentName for ${sessionId}`);
  }
  return node;
}

const LISTED_ID = "abcdef1234567890";

test("a session is named by its live title, else its stored title, else its short id", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([listed(LISTED_ID, "Planner")]);
  const { view } = renderWith(
    <>
      <AgentName
        actor={{ id: LISTED_ID, kind: "session", origin: { session_title: "old title" } }}
      />
      <AgentName
        actor={{ id: "fedcba9876543210", kind: "session", origin: { session_title: "Stamped" } }}
      />
      <AgentName actor={{ id: "0123456789abcdef", kind: "session" }} />
      <AgentName sessionId="9999999999999999" storedTitle="Stored" />
      <AgentName sessionId="8888888888888888" storedTitle={undefined} />
    </>
  );
  try {
    await waitFor(() =>
      expect(agentNode(view.container, LISTED_ID).getAttribute("data-agent-state")).toBe("running")
    );
    expect(agentNode(view.container, LISTED_ID).textContent).toBe("Planner");
    expect(agentNode(view.container, "fedcba9876543210").textContent).toBe("Stamped");
    expect(agentNode(view.container, "0123456789abcdef").textContent).toBe("session:01234567…");
    expect(agentNode(view.container, "9999999999999999").textContent).toBe("Stored");
    expect(agentNode(view.container, "8888888888888888").textContent).toBe("session:88888888…");
    // Many names on a page, one read of the registry.
    expect(agents).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a bare session id is named by its live title over the stored one", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([listed(LISTED_ID, "Planner")]);
  const { view } = renderWith(<AgentName sessionId={LISTED_ID} storedTitle="Stored" />);
  try {
    expect(await screen.findByRole("link", { name: "Planner" })).toBeDefined();
    expect(screen.queryByText("Stored")).toBeNull();
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

/** What a screen reader reads of `node`, in order: its text, an element with an `aria-label` as
 *  that name, and nothing marked `aria-hidden`. */
function spokenText(node: Node): string {
  if (node.nodeType === node.TEXT_NODE) return node.textContent ?? "";
  if (node.nodeType !== node.ELEMENT_NODE) return "";
  const element = node as Element;
  if (element.getAttribute("aria-hidden") === "true") return "";
  const label = element.getAttribute("aria-label");
  if (label !== null) return ` ${label} `;
  return Array.from(element.childNodes, spokenText).join("").replace(/\s+/g, " ").trim();
}

test("the owner and service attribution follow the name, outside its link, read once", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([listed(LISTED_ID, "Planner")]);
  const { view } = renderWith(
    <>
      <AgentName actor={{ id: LISTED_ID, kind: "session", owner: "alice" }} />
      <AgentName
        actor={{
          id: "worker-session",
          kind: "session",
          origin: { session_title: "Implementer" },
          service: "system:serviceaccount:legion:legion-worker",
        }}
      />
    </>
  );
  try {
    // The link is named by the visible name; the attribution after it is text, so a screen
    // reader hears the byline as it reads, never the attribution twice.
    const owned = await screen.findByRole("link", { name: "Planner" });
    expect(owned.getAttribute("aria-label")).toBeNull();
    await waitFor(() =>
      expect(spokenText(agentNode(view.container, LISTED_ID))).toBe("Running Planner (for alice)")
    );
    expect(agentNode(view.container, LISTED_ID).textContent).toBe("Planner (for alice)");
    const service = screen.getByRole("link", { name: "Implementer" });
    expect(service.getAttribute("aria-label")).toBeNull();
    expect(spokenText(agentNode(view.container, "worker-session"))).toBe(
      "Not running Implementer (as legion/legion-worker)"
    );
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a name in the short form carries the full session id as its tooltip", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([listed(LISTED_ID, "Planner")]);
  const { view } = renderWith(
    <>
      <AgentName actor={{ id: LISTED_ID, kind: "session" }} />
      <AgentName actor={{ id: "0123456789abcdef", kind: "session" }} />
      <AgentName sessionId="01234567ffffffff" storedTitle={undefined} />
      <AgentName kind="static" sessionId="fedcba9876543210" storedTitle="Stored" />
    </>
  );
  try {
    await screen.findByRole("link", { name: "Planner" });
    expect(agentNode(view.container, LISTED_ID).getAttribute("title")).toBeNull();
    expect(agentNode(view.container, "fedcba9876543210").getAttribute("title")).toBeNull();
    // Two sessions minted in the same minute read alike; their tooltips tell them apart.
    for (const id of ["0123456789abcdef", "01234567ffffffff"]) {
      expect(agentNode(view.container, id).textContent).toBe("session:01234567…");
      expect(agentNode(view.container, id).getAttribute("title")).toBe(id);
    }
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a user or system actor is its id as plain text, with no link, state, or registry read", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const { client, view } = renderWith(
    <p>
      <AgentName actor={{ id: "alice", kind: "user" }} />
      {" / "}
      <AgentName actor={{ id: "architecture-importer", kind: "system" }} />
    </p>
  );
  try {
    await act(async () => {});
    expect(view.container.textContent).toBe("alice / architecture-importer");
    expect(view.container.querySelector("[data-agent-name]")).toBeNull();
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.queryByRole("img")).toBeNull();
    expect(agents).not.toHaveBeenCalled();
    expect(client.getQueryCache().find({ queryKey: ["agents"] })).toBeUndefined();
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("the control variant keeps the host control around a user or system actor's id", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const control = (name: ReactNode) => (
    <button aria-expanded={false} type="button">
      {name}
    </button>
  );
  const { view } = renderWith(
    <>
      <AgentName actor={{ id: "alice", kind: "user" }} control={control} kind="control" />
      <AgentName
        actor={{ id: "architecture-importer", kind: "system" }}
        className="host-class"
        control={control}
        kind="control"
      />
    </>
  );
  try {
    await act(async () => {});
    expect(screen.getByRole("button", { name: "alice" })).toBeDefined();
    const system = screen.getByRole("button", { name: "architecture-importer" });
    expect(system.parentElement?.className).toBe("host-class");
    expect(screen.queryByRole("link")).toBeNull();
    expect(view.container.querySelector("[data-agent-name]")).toBeNull();
    expect(agents).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

function mark(container: HTMLElement, sessionId: string): HTMLElement {
  return within(agentNode(container, sessionId)).getByRole("img");
}

test("before any registry read lands, the state is unknown, never not running", async () => {
  const agents = spyOn(api, "listAgents").mockReturnValue(new Promise<Agent[]>(() => {}));
  const { view } = renderWith(
    <AgentName actor={{ id: LISTED_ID, kind: "session", origin: { session_title: "Stamped" } }} />
  );
  try {
    await act(async () => {});
    expect(agents).toHaveBeenCalledTimes(1);
    const node = agentNode(view.container, LISTED_ID);
    expect(node.getAttribute("data-agent-state")).toBe("unknown");
    expect(
      within(node).getByRole("img", { name: "Running state unknown" }).getAttribute("title")
    ).toBe("Running state unknown");
    expect(node.textContent).toBe("Stamped");
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a registry read that fails before any success leaves the state unknown", async () => {
  const agents = spyOn(api, "listAgents").mockRejectedValue(new Error("down"));
  const { client, view } = renderWith(
    <AgentName actor={{ id: LISTED_ID, kind: "session", origin: { session_title: "Stamped" } }} />
  );
  try {
    await waitFor(() => expect(client.getQueryState(["agents"])?.status).toBe("error"));
    expect(agentNode(view.container, LISTED_ID).getAttribute("data-agent-state")).toBe("unknown");
    expect(mark(view.container, LISTED_ID).getAttribute("aria-label")).toBe(
      "Running state unknown"
    );
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a listed session is running, an unlisted one not running, and a failed refresh unknown", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([listed(LISTED_ID, "Planner")]);
  const { client, view } = renderWith(
    <>
      <AgentName actor={{ id: LISTED_ID, kind: "session" }} />
      <AgentName actor={{ id: "gone-session-0000", kind: "session" }} />
    </>
  );
  try {
    await waitFor(() =>
      expect(agentNode(view.container, LISTED_ID).getAttribute("data-agent-state")).toBe("running")
    );
    const running = mark(view.container, LISTED_ID);
    expect(running.getAttribute("aria-label")).toBe("Running");
    expect(running.getAttribute("title")).toBe("Running · seen 3 minutes ago");
    expect(running.className).toContain(liveDotBg);
    expect(running.className).not.toContain("border");

    const stopped = mark(view.container, "gone-session-0000");
    expect(agentNode(view.container, "gone-session-0000").getAttribute("data-agent-state")).toBe(
      "not-running"
    );
    expect(stopped.getAttribute("aria-label")).toBe("Not running");
    expect(stopped.className).toContain(offlineDotBorder);
    expect(stopped.className).toContain("border-solid");
    expect(stopped.className).not.toContain("bg-");

    // The next read fails: what the registry said before no longer holds, so neither session
    // reads as running or not running, and the cached title still names the agent.
    agents.mockRejectedValue(new Error("down"));
    await act(async () => {
      await client.refetchQueries({ queryKey: ["agents"] });
    });
    for (const id of [LISTED_ID, "gone-session-0000"]) {
      expect(agentNode(view.container, id).getAttribute("data-agent-state")).toBe("unknown");
      const unknown = mark(view.container, id);
      expect(unknown.getAttribute("aria-label")).toBe("Running state unknown");
      expect(unknown.className).toContain(unknownDotBorder);
      expect(unknown.className).toContain("border-dashed");
    }
    expect(agentNode(view.container, LISTED_ID).textContent).toBe("Planner");
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("the name links to the session's live conversation, its id encoded", async () => {
  const id = "team/a b#1?";
  const agents = spyOn(api, "listAgents").mockResolvedValue([listed(id, "Odd one")]);
  const { view } = renderWith(
    <>
      <AgentName actor={{ id, kind: "session" }} />
      <p>
        Claimed by <AgentName actor={{ id, kind: "session" }} inline />
      </p>
    </>
  );
  try {
    const [standalone, inline] = await screen.findAllByRole("link", { name: "Odd one" });
    expect(standalone?.getAttribute("href")).toBe("/agents/team%2Fa%20b%231%3F/live");
    expect(standalone?.classList.contains("dispatch-inline-link")).toBe(false);
    expect(inline?.getAttribute("href")).toBe("/agents/team%2Fa%20b%231%3F/live");
    expect(inline?.classList.contains("dispatch-inline-link")).toBe(true);
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("the control variant keeps the host control and puts the live link beside it", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([listed(LISTED_ID, "Planner")]);
  const { view } = renderWith(
    <AgentName
      actor={{ id: LISTED_ID, kind: "session", owner: "alice" }}
      control={(name) => (
        <button aria-expanded={false} type="button">
          {name}
        </button>
      )}
      kind="control"
    />
  );
  try {
    const live = await screen.findByRole("link", {
      name: "Open live conversation with Planner (for alice)",
    });
    const host = screen.getByRole("button", { name: "Planner" });
    expect(host.contains(live)).toBe(false);
    expect(live.parentElement).toBe(host.parentElement);
    expect(live.getAttribute("href")).toBe(`/agents/${LISTED_ID}/live`);
    expect(live.classList.contains("dispatch-inline-link")).toBe(false);
    expect(live.textContent?.trim()).not.toBe("");
    expect(screen.getAllByRole("link")).toHaveLength(1);
    expect(agentNode(view.container, LISTED_ID).textContent).toContain("Planner (for alice)");
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("the static variant shows the name and state with no link", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([listed(LISTED_ID, "Planner")]);
  const { view } = renderWith(
    <AgentName actor={{ id: LISTED_ID, kind: "session" }} kind="static" />
  );
  try {
    await waitFor(() =>
      expect(agentNode(view.container, LISTED_ID).getAttribute("data-agent-state")).toBe("running")
    );
    expect(agentNode(view.container, LISTED_ID).textContent).toBe("Planner");
    expect(screen.getByRole("img", { name: "Running" })).toBeDefined();
    expect(screen.queryByRole("link")).toBeNull();
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("a resolution names its actor through the component", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([listed(LISTED_ID, "Planner")]);
  const { view } = renderWith(
    <p>
      <AskResolutionLine
        resolution={{
          actor: { id: LISTED_ID, kind: "session" },
          at: "2026-10-01T00:00:00Z",
          kind: "retracted",
          reason: "done",
        }}
      />
    </p>
  );
  try {
    const link = await screen.findByRole("link", { name: "Planner" });
    expect(link.classList.contains("dispatch-inline-link")).toBe(true);
    expect(view.container.textContent).toBe("Retracted by Planner");
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

test("the text API names agents by the cached registry titles and fetches nothing", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const client = newClient();
  client.setQueryData(["agents"], [listed(LISTED_ID, "Planner Bot")]);
  const owned: Actor = { id: LISTED_ID, kind: "session", owner: "alice" };
  function Texts(): ReactNode {
    const text = useAgentText();
    return (
      <ul>
        <li>{text.label(owned)}</li>
        <li>{text.initials(owned)}</li>
        <li>{text.sessionText("0123456789abcdef", undefined)}</li>
        <li>{text.sessionText(LISTED_ID, "Stored")}</li>
      </ul>
    );
  }
  const { view } = renderWith(<Texts />, client);
  try {
    await act(async () => {});
    expect(screen.getAllByRole("listitem").map((item) => item.textContent)).toEqual([
      "Planner Bot (for alice)",
      "PB",
      "session:01234567…",
      "Planner Bot",
    ]);
    expect(agents).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    agents.mockRestore();
  }
});

function PolledAgents(): ReactNode {
  useAgents(true, true);
  return null;
}

// However many names mount, and whenever they mount (including long after the last read, past
// the query's 10 s staleTime), the shell's one poller is the only thing that refreshes the
// registry: one request per 15 s per tab.
test("names mounted at staggered times plus a polled useAgents caller fetch at most once per 15 s", async () => {
  // The test preload imports TanStack before it registers happy-dom, so TanStack takes the run
  // for a server and schedules no refetch interval at all; this test is about that interval.
  const wasServer = environmentManager.isServer();
  environmentManager.setIsServer(() => false);
  jest.useFakeTimers();
  const start = Date.now();
  jest.setSystemTime(start);
  const fetchedAt: number[] = [];
  const agents = spyOn(api, "listAgents").mockImplementation(async () => {
    fetchedAt.push(Date.now() - start);
    return [listed(LISTED_ID, "Planner")];
  });
  const client = newClient();
  // Mount offsets (ms) of five names and the useAgents caller.
  const mounts: Array<{ at: number; node: ReactNode }> = [
    { at: 0, node: <AgentName actor={{ id: LISTED_ID, kind: "session" }} key="a" /> },
    { at: 7_000, node: <AgentName actor={{ id: "s-2", kind: "session" }} key="b" /> },
    { at: 12_000, node: <AgentName actor={{ id: "s-3", kind: "session" }} key="c" /> },
    { at: 26_000, node: <AgentName actor={{ id: "s-4", kind: "session" }} key="d" /> },
    { at: 38_000, node: <AgentName sessionId="s-5" storedTitle={undefined} key="e" /> },
    { at: 41_000, node: <PolledAgents key="f" /> },
  ];
  const tree = (count: number) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <AgentRegistryPoller />
        {mounts.slice(0, count).map((mount) => mount.node)}
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree(0));
  try {
    let elapsed = 0;
    for (const [index, mount] of mounts.entries()) {
      await act(async () => {
        jest.advanceTimersByTime(mount.at - elapsed);
      });
      elapsed = mount.at;
      view.rerender(tree(index + 1));
      await act(async () => {});
    }
    await act(async () => {
      jest.advanceTimersByTime(45_000 - elapsed);
    });
    await act(async () => {});

    // The poller kept refreshing across the window (a first load, then its polls), and no two
    // requests were closer than one poll apart, however many readers mounted in between.
    expect(fetchedAt.length).toBeGreaterThanOrEqual(3);
    for (let index = 1; index < fetchedAt.length; index += 1) {
      expect((fetchedAt[index] ?? 0) - (fetchedAt[index - 1] ?? 0)).toBeGreaterThanOrEqual(
        AGENT_REGISTRY_POLL_MS
      );
    }

    // With nothing showing an agent, the poller stops.
    view.rerender(tree(0));
    const before = fetchedAt.length;
    await act(async () => {
      jest.advanceTimersByTime(60_000);
    });
    expect(fetchedAt.length).toBe(before);
  } finally {
    view.unmount();
    jest.useRealTimers();
    environmentManager.setIsServer(() => wasServer);
    agents.mockRestore();
  }
});
