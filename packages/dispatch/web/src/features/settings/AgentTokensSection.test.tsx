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

// A mint Dispatch made and lost the answer to (`409 KEY_COMMITTED`) made a token whose value,
// shown only once, is gone: the form lets go of its label, so a second press mints nothing, says
// the value was lost, and offers to revoke the token - once it is listed - and mint another.
test("a mint answered KEY_COMMITTED says its value was lost, and replaces it on request", async () => {
  const lost: AgentToken = {
    ...token,
    created_at: "2026-09-13T00:00:00Z",
    id: "33333333-3333-3333-3333-333333333333",
    name: "reviewer",
  };
  const listAgentTokens = spyOn(api, "listAgentTokens")
    .mockResolvedValueOnce([token])
    .mockResolvedValue([lost, token]);
  const createAgentToken = spyOn(api, "createAgentToken")
    .mockRejectedValueOnce(new ApiError(409, { code: "KEY_COMMITTED", error: "already made" }))
    .mockResolvedValueOnce({
      ...lost,
      id: "44444444-4444-4444-4444-444444444444",
      token: "dsp_replacement",
    });
  const revokeAgentToken = spyOn(api, "revokeAgentToken").mockResolvedValue();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <AgentTokensSection />
      </QueryClientProvider>
    );
    await screen.findByRole("cell", { name: "dsp_AbCdEf12…" });
    const label = screen.getByLabelText<HTMLInputElement>("Token label");
    fireEvent.change(label, { target: { value: "reviewer" } });
    fireEvent.click(screen.getByRole("button", { name: "New token" }));

    const made = await screen.findByTestId("landed-without-answer");
    expect(made.textContent).toBe(
      "Token “reviewer” was made, but Dispatch lost its answer, so it may take a moment to appear. Its value is shown only once and was lost with the answer, so revoke it and mint another."
    );
    expect(label.value).toBe("");
    expect(screen.queryByRole("alert")).toBeNull();

    // The emptied form mints nothing on a second press.
    fireEvent.submit(label.closest("form") as HTMLFormElement);
    await Bun.sleep(20);
    expect(createAgentToken).toHaveBeenCalledTimes(1);

    // Once the list is read again the lost token is there, and the offer revokes it and mints
    // another under its label.
    const replace = await screen.findByRole("button", { name: "Revoke it and mint another" });
    fireEvent.click(replace);
    await waitFor(() => expect(revokeAgentToken).toHaveBeenCalledWith(lost.id, keyedAttempt));
    await waitFor(() => expect(createAgentToken).toHaveBeenCalledTimes(2));
    expect(createAgentToken.mock.calls[1]?.[0]).toEqual({ name: "reviewer" });
    expect(await screen.findByText("dsp_replacement")).toBeDefined();
    expect(screen.queryByTestId("landed-without-answer")).toBeNull();
  } finally {
    cleanup();
    queryClient.clear();
    listAgentTokens.mockRestore();
    createAgentToken.mockRestore();
    revokeAgentToken.mockRestore();
  }
});
