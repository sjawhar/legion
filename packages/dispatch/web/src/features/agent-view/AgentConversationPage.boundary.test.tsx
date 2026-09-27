import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { api } from "../../api/client";
import * as live from "../../api/live";
import type { Agent } from "../../api/types";
import { AgentConversationPage } from "./AgentConversationPage";
import * as conversation from "./conversation";

/**
 * assistant-ui converts every message while the runtime is built, so a message it refuses throws
 * in the render of whichever component calls `useExternalStoreRuntime`. While that was the page
 * itself, the boundary the page rendered sat above nothing: the throw escaped to the route's
 * boundary and the viewer lost the header naming the session and the controls for reaching it,
 * not just the transcript.
 *
 * The throw is injected past the frame validator on purpose. Both guards against a known bad
 * shape are tested elsewhere; what this states is that the conversation view survives a
 * conversion failure the guards did not anticipate, which is the only reason to have a boundary
 * at all.
 */

const SESSION = "01a0e090-4848-7473-acc5-fc96e6a646d3";

const agent: Agent = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/workspaces/planner",
  last_activity: null,
  last_seen: Date.now(),
  machine_id: "planner-host",
  open_asks: 0,
  roles: [],
  session_id: SESSION,
  title: "Planner",
};
// bun runs every test file in one process and a module spy is shared by all of them, so each of
// these has to be put back: an un-restored `toThreadMessages` stub reds every other file that
// renders a conversation.
const spies = [
  spyOn(api, "listAgents"),
  spyOn(live, "readEventStream"),
  spyOn(conversation, "toThreadMessages"),
];

afterEach(() => {
  cleanup();
  for (const spy of spies) spy.mockRestore();
});

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { gcTime: 0, retry: false, staleTime: 0 } },
  });
  return render(
    <MemoryRouter initialEntries={[`/agents/${SESSION}/live`]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route element={<AgentConversationPage />} path="/agents/:sessionId/live" />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("a conversation the runtime cannot convert costs the thread, not the page around it", async () => {
  spyOn(api, "listAgents").mockResolvedValue([agent]);
  // The stream never opens: this test is about rendering, and an open stream would retry.
  spyOn(live, "readEventStream").mockReturnValue(new Promise<void>(() => undefined));
  spyOn(conversation, "toThreadMessages").mockImplementation(() => {
    throw new Error("Unsupported user message part type: reasoning");
  });

  renderPage();

  // The thread region shows its own error state…
  await waitFor(() => {
    expect(screen.getByText(/Something went wrong in this conversation/)).toBeTruthy();
  });
  // …and everything the viewer needs to know what they are looking at, and to act, survives.
  expect(screen.getByTestId("agent-conversation")).toBeTruthy();
  expect(screen.getByRole("link", { name: /Agents/ })).toBeTruthy();
  expect(screen.getByRole("combobox", { name: "Delivery mode" })).toBeTruthy();
  expect(screen.getByText(/Nothing here is stored/)).toBeTruthy();
});
