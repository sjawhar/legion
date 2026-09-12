import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { api } from "../../api/client";
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
    expect(screen.getByText("This merges into your existing envoy.json file.")).toBeDefined();
    const envSnippet = snippets.find((snippet) => snippet.textContent?.includes("DISPATCH_URL="));
    expect(envSnippet?.textContent).toBe(`DISPATCH_URL=${origin}\nDISPATCH_TOKEN=${plaintext}`);

    fireEvent.click(screen.getByRole("button", { name: "Copy token" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(plaintext));

    fireEvent.click(screen.getByRole("button", { name: "Revoke architect" }));
    await waitFor(() => expect(revokeAgentToken).toHaveBeenCalledWith(token.id));
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
