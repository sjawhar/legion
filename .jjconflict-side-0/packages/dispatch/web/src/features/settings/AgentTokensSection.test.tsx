import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { ApiError, api } from "../../api/client";
import type { AgentToken } from "../../api/types";
import { AgentTokensSection } from "./AgentTokensSection";

const token: AgentToken = {
  created_at: "2026-09-12T00:00:00Z",
  id: "11111111-1111-1111-1111-111111111111",
  last_used_at: null,
  name: "architect",
  prefix: "AbCdEf12",
  revoked_at: null,
};

test("Settings exposes a new personal token once, copies it, and revokes an existing token", async () => {
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([token]);
  const createAgentToken = spyOn(api, "createAgentToken").mockResolvedValue({
    ...token,
    id: "22222222-2222-2222-2222-222222222222",
    name: "reviewer",
    token: "dsp_abcdefghijklmnopqrstuvwxyz0123456789-ABCDEFG",
  });
  const revokeAgentToken = spyOn(api, "revokeAgentToken").mockResolvedValue();
  const originalConfirm = window.confirm;
  const originalClipboard = navigator.clipboard;
  const writeText = spyOn({ writeText: async () => undefined }, "writeText");
  window.confirm = () => true;
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <AgentTokensSection />
      </QueryClientProvider>
    );

    expect(await screen.findByRole("cell", { name: "dsp_AbCdEf12…" })).toBeDefined();
    expect(screen.getByText("Never used")).toBeDefined();

    fireEvent.change(screen.getByLabelText("Token label"), { target: { value: "reviewer" } });
    fireEvent.click(screen.getByRole("button", { name: "New token" }));

    const plaintext = "dsp_abcdefghijklmnopqrstuvwxyz0123456789-ABCDEFG";
    const origin = window.location.origin;
    const config = JSON.stringify(
      { dispatch: { enabled: true, serverUrl: origin, token: plaintext } },
      null,
      2
    );
    expect(await screen.findByText(plaintext)).toBeDefined();
    const snippets = Array.from(document.querySelectorAll("pre code"));
    const configSnippet = snippets.find((snippet) => snippet.textContent?.includes('"dispatch"'));
    expect(configSnippet?.textContent).toBe(config);
    const paragraphs = Array.from(document.querySelectorAll("p"), (p) => p.textContent);
    expect(paragraphs).toContain("Put this in ~/.config/legion/envoy.json:");
    expect(paragraphs).toContain(
      "If the file exists, replace its dispatch section with this one and keep the rest. A session under a named omp profile, or with CLAUDE_CONFIG_DIR or OPENCODE_CONFIG_DIR set, reads the envoy.json in that directory instead; any other session reads the file ENVOY_CONFIG_FILE names when it is set."
    );
    const envSnippet = snippets.find((snippet) => snippet.textContent?.includes("DISPATCH_URL="));
    expect(envSnippet?.textContent).toBe(`DISPATCH_URL=${origin}\nDISPATCH_TOKEN=${plaintext}`);

    fireEvent.click(screen.getByRole("button", { name: "Copy token" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(plaintext));

    fireEvent.click(screen.getByRole("button", { name: "Revoke architect" }));
    await waitFor(() => expect(revokeAgentToken).toHaveBeenCalledWith(token.id, keyedAttempt));
    await waitFor(() => expect(screen.getByText("Revoked")).toBeDefined());
    expect(screen.queryByRole("button", { name: "Revoke architect" })).toBeNull();
  } finally {
    cleanup();
    queryClient.clear();
    listAgentTokens.mockRestore();
    createAgentToken.mockRestore();
    revokeAgentToken.mockRestore();
    window.confirm = originalConfirm;
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
  }
});

test("Settings copies a new token through the legacy clipboard fallback", async () => {
  const plaintext = "dsp_abcdefghijklmnopqrstuvwxyz0123456789-ABCDEFG";
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([]);
  const createAgentToken = spyOn(api, "createAgentToken").mockResolvedValue({
    ...token,
    id: "22222222-2222-2222-2222-222222222222",
    name: "reviewer",
    token: plaintext,
  });
  const originalClipboard = navigator.clipboard;
  const originalExecCommand = document.execCommand;
  const execCommand = spyOn({ execCommand: (_command: string) => true }, "execCommand");
  Object.defineProperty(document, "execCommand", { configurable: true, value: execCommand });
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: undefined });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <AgentTokensSection />
      </QueryClientProvider>
    );
    fireEvent.change(screen.getByLabelText("Token label"), { target: { value: "reviewer" } });
    fireEvent.click(screen.getByRole("button", { name: "New token" }));
    await screen.findByText(plaintext);

    fireEvent.click(screen.getByRole("button", { name: "Copy token" }));
    expect(await screen.findByText("Copied", { exact: true })).toBeDefined();
  } finally {
    cleanup();
    queryClient.clear();
    listAgentTokens.mockRestore();
    createAgentToken.mockRestore();
    Object.defineProperty(document, "execCommand", {
      configurable: true,
      value: originalExecCommand,
    });
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
  }
});

/** A live token labelled `name`, made `offsetMs` from now: after a press when positive, before
 *  it when negative. */
function madeAt(id: string, name: string, offsetMs: number): AgentToken {
  return { ...token, created_at: new Date(Date.now() + offsetMs).toISOString(), id, name };
}

/** The Settings token section, its mint answered `409 KEY_COMMITTED` and its list then read as
 *  `listed`; the list read before the press holds those of `listed` made before it, which the page
 *  tells apart from the lost mint's by id. `confirmAnswers` answers each `window.confirm` in
 *  turn. */
async function mintLostWith(listed: AgentToken[], confirmAnswers: boolean[] = []) {
  const now = Date.now();
  const listAgentTokens = spyOn(api, "listAgentTokens")
    .mockResolvedValueOnce(listed.filter((made) => Date.parse(made.created_at) <= now))
    .mockResolvedValue(listed);
  const createAgentToken = spyOn(api, "createAgentToken")
    .mockRejectedValueOnce(new ApiError(409, { code: "KEY_COMMITTED", error: "already made" }))
    .mockResolvedValue({ ...token, id: "replacement", name: "reviewer", token: "dsp_replacement" });
  const revokeAgentToken = spyOn(api, "revokeAgentToken").mockResolvedValue();
  const asked: string[] = [];
  const originalConfirm = window.confirm;
  window.confirm = (message?: string) => {
    asked.push(message ?? "");
    return confirmAnswers.shift() ?? false;
  };
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  render(
    <QueryClientProvider client={queryClient}>
      <AgentTokensSection />
    </QueryClientProvider>
  );
  // The tokens made before the press share the fixture's prefix.
  await screen.findAllByRole("cell", { name: "dsp_AbCdEf12…" });
  const label = screen.getByLabelText<HTMLInputElement>("Token label");
  fireEvent.change(label, { target: { value: "reviewer" } });
  fireEvent.click(screen.getByRole("button", { name: "New token" }));
  await screen.findByTestId("landed-without-answer");
  return {
    asked,
    createAgentToken,
    label,
    restore: () => {
      cleanup();
      queryClient.clear();
      window.confirm = originalConfirm;
      listAgentTokens.mockRestore();
      createAgentToken.mockRestore();
      revokeAgentToken.mockRestore();
    },
    revokeAgentToken,
  };
}

/** What the replace offer asks before it revokes `made`. */
function confirmFor(made: AgentToken): string {
  return `Revoke “${made.name}”, created ${new Date(made.created_at).toLocaleString()}? Agents using it stop working at once, and a new token “${made.name}” is minted in its place.`;
}

// A mint Dispatch made and lost the answer to (`409 KEY_COMMITTED`) made a token whose value,
// shown only once, is gone. The form lets go of its label, so a second press mints nothing, and
// says the value was lost. Once the list is read again, the one live token of that label made
// since the press - never an older one of the same label - is offered for revoking, and the
// reader confirms it by its label and when it was made before it is revoked and another minted.
test("a mint answered KEY_COMMITTED offers the one token made since the press, confirmed first", async () => {
  const older = madeAt("older", "reviewer", -86_400_000);
  const lost = madeAt("lost", "reviewer", 60_000);
  const page = await mintLostWith([lost, older, token], [false, true]);

  try {
    expect(screen.getByTestId("landed-without-answer").textContent).toBe(
      "Token “reviewer” was made, but Dispatch lost its answer, so it may take a moment to appear. Its value is shown only once and was lost with the answer, so revoke it and mint another."
    );
    expect(page.label.value).toBe("");
    expect(screen.queryByRole("alert")).toBeNull();
    fireEvent.submit(page.label.closest("form") as HTMLFormElement);
    await Bun.sleep(20);
    expect(page.createAgentToken).toHaveBeenCalledTimes(1);

    const replace = await screen.findByRole("button", { name: "Revoke it and mint another" });
    // Cancelled: nothing revoked.
    fireEvent.click(replace);
    expect(page.asked).toEqual([confirmFor(lost)]);
    await Bun.sleep(20);
    expect(page.revokeAgentToken).not.toHaveBeenCalled();

    fireEvent.click(replace);
    await waitFor(() => expect(page.revokeAgentToken).toHaveBeenCalledWith(lost.id, keyedAttempt));
    await waitFor(() => expect(page.createAgentToken).toHaveBeenCalledTimes(2));
    expect(page.createAgentToken.mock.calls[1]?.[0]).toEqual({ name: "reviewer" });
    expect(await screen.findByText("dsp_replacement")).toBeDefined();
    expect(screen.queryByTestId("landed-without-answer")).toBeNull();
    expect(page.revokeAgentToken).toHaveBeenCalledTimes(1);
  } finally {
    page.restore();
  }
});

// No live token of that label made since the press is listed - only an older one of the same
// label, and a revoked new one: the section says so and revokes nothing, and offers no button.
test("a lost mint with no token made since the press says so and revokes nothing", async () => {
  const older = madeAt("older", "reviewer", -86_400_000);
  const revoked = { ...madeAt("revoked", "reviewer", 60_000), revoked_at: "2099-01-01T00:00:00Z" };
  const page = await mintLostWith([older, revoked, token]);

  try {
    expect(
      await screen.findByText(
        "No live token “reviewer” made since you pressed New token is listed, so none was revoked."
      )
    ).toBeDefined();
    expect(screen.queryByRole("button", { name: "Revoke it and mint another" })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Revoke the one created/ })).toBeNull();
    expect(page.asked).toEqual([]);
    expect(page.revokeAgentToken).not.toHaveBeenCalled();
  } finally {
    page.restore();
  }
});

// More than one live token of that label was made since the press (another tab minted one
// too): the section lists them, newest first, and the reader picks the one to revoke, confirming
// it by when it was made; only that one is revoked.
test("a lost mint with several tokens made since the press lists them and revokes the one picked", async () => {
  const first = madeAt("first", "reviewer", 60_000);
  const second = madeAt("second", "reviewer", 120_000);
  const page = await mintLostWith([first, second, token], [true]);

  try {
    expect(
      await screen.findByText(
        "2 live tokens “reviewer” were made since you pressed New token. Pick the one to revoke:"
      )
    ).toBeDefined();
    const picks = screen.getAllByRole("button", { name: /^Revoke the one created/ });
    expect(picks.map((pick) => pick.textContent)).toEqual([
      `Revoke the one created ${new Date(second.created_at).toLocaleString()}`,
      `Revoke the one created ${new Date(first.created_at).toLocaleString()}`,
    ]);
    expect(screen.queryByRole("button", { name: "Revoke it and mint another" })).toBeNull();

    fireEvent.click(picks[1] as HTMLElement);
    expect(page.asked).toEqual([confirmFor(first)]);
    await waitFor(() => expect(page.revokeAgentToken).toHaveBeenCalledWith(first.id, keyedAttempt));
    await waitFor(() => expect(page.createAgentToken).toHaveBeenCalledTimes(2));
    expect(page.revokeAgentToken).toHaveBeenCalledTimes(1);
  } finally {
    page.restore();
  }
});
