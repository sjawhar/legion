import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { IssueDetails, ReportInput, ReportResponse, ReportSettings } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { ReportProblemDialog } from "./ReportProblemDialog";
import type { ReportPageContext } from "./report-context";

const context: ReportPageContext = {
  page: "https://dispatch.example/issues/CORE-1",
  page_build: "/assets/index-abc123.js",
  user_agent: "Mozilla/5.0 Test",
  viewport: { height: 800, width: 1280 },
};

function settings(project: string | null, agentRoute: string | null): ReportSettings {
  return { agent_route: agentRoute, project, updated_at: null, updated_by: null };
}

function filed(key: string, route: string | null): ReportResponse {
  return { key, route, title: "The Send button overlaps the box" } as unknown as ReportResponse;
}

interface Sent {
  readonly input: ReportInput;
  readonly pictures: readonly File[];
}

function renderDialog(
  stored: ReportSettings,
  create: (input: ReportInput, pictures: readonly File[]) => Promise<ReportResponse> = async () =>
    filed("CORE-12", null)
) {
  const read = spyOn(api, "getReportSettings").mockResolvedValue(stored);
  const sent: Sent[] = [];
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <ReportProblemDialog
            context={() => context}
            createReport={(input, pictures) => {
              sent.push({ input, pictures });
              return create(input, pictures);
            }}
            onClose={() => {}}
          />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    sent,
    unmount: () => {
      view.unmount();
      queryClient.clear();
      read.mockRestore();
    },
  };
}

function png(name: string): File {
  return new File([new Uint8Array([0x89, 0x50, 0x4e, 0x47])], name, { type: "image/png" });
}

test("with no project set, the dialog says reports are not set up and points to Settings", async () => {
  const view = renderDialog(settings(null, null));
  try {
    expect(await screen.findByText(/Reports are not set up/)).toBeTruthy();
    const link = screen.getByRole("link", { name: "Settings → Problem reports" });
    expect(link.getAttribute("href")).toBe("/settings#problem-reports");
    expect(screen.queryByRole("button", { name: "Send report" })).toBeNull();
    expect(screen.queryByLabelText("What went wrong")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("with no agent set, the box to ask one is hidden, and the box for the words has focus", async () => {
  const view = renderDialog(settings("CORE", null));
  try {
    const field = await screen.findByLabelText("What went wrong");
    await waitFor(() => expect(document.activeElement).toBe(field));
    expect(screen.queryByLabelText("Ask an agent to fix this")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("sends the words, the page's details and the pictures, then names the new issue", async () => {
  const view = renderDialog(settings("CORE", null));
  try {
    const field = await screen.findByLabelText("What went wrong");
    fireEvent.change(field, { target: { value: "The Send button overlaps the box" } });
    fireEvent.paste(field, { clipboardData: { files: [png("shot.png")] } });
    expect(screen.getByText("shot.png")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Send report" }));
    await waitFor(() => expect(view.sent).toHaveLength(1));
    expect(view.sent[0]?.input).toEqual({
      ...context,
      ask_agent: false,
      body: "The Send button overlaps the box",
    });
    expect(view.sent[0]?.pictures.map((picture) => picture.name)).toEqual(["shot.png"]);
    expect(await screen.findByText(/Reported as/)).toBeTruthy();
    const link = screen.getByRole("link", { name: "CORE-12" });
    expect(link.getAttribute("href")).toBe("/issues/CORE-12");
    // The box is gone; focus stays in the dialog, on the new issue's link.
    await waitFor(() => expect(document.activeElement).toBe(link));
  } finally {
    view.unmount();
  }
});

test("a pasted or dropped picture is listed and can be removed; text pastes as text; other files are refused", async () => {
  const view = renderDialog(settings("CORE", null));
  try {
    const field = await screen.findByLabelText("What went wrong");
    // A paste of text alone is the field's own: nothing is listed.
    fireEvent.paste(field, { clipboardData: { files: [] } });
    expect(screen.queryByRole("list", { name: "Pictures" })).toBeNull();
    fireEvent.drop(field, { dataTransfer: { files: [png("dropped.png")] } });
    fireEvent.paste(field, {
      clipboardData: {
        files: [png("pasted.png"), new File(["notes"], "notes.txt", { type: "text/plain" })],
      },
    });
    const list = screen.getByRole("list", { name: "Pictures" });
    expect(list.textContent).toContain("dropped.png");
    expect(list.textContent).toContain("pasted.png");
    expect(screen.getByRole("alert").textContent).toContain("notes.txt");
    fireEvent.click(screen.getByRole("button", { name: "Remove dropped.png" }));
    expect(screen.getByRole("list", { name: "Pictures" }).textContent).not.toContain("dropped.png");
    fireEvent.change(field, { target: { value: "Broken" } });
    fireEvent.click(screen.getByRole("button", { name: "Send report" }));
    await waitFor(() => expect(view.sent).toHaveLength(1));
    expect(view.sent[0]?.pictures.map((picture) => picture.name)).toEqual(["pasted.png"]);
  } finally {
    view.unmount();
  }
});

test("two pictures of one name are removed apart", async () => {
  const view = renderDialog(settings("CORE", null));
  try {
    const field = await screen.findByLabelText("What went wrong");
    fireEvent.paste(field, { clipboardData: { files: [png("image.png"), png("image.png")] } });
    fireEvent.click(screen.getByRole("button", { name: "Remove image.png, picture 1" }));
    expect(screen.getByRole("button", { name: "Remove image.png" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("a refusal that says the settings changed reads them again", async () => {
  const view = renderDialog(settings("CORE", "role:dispatch-fixer"), async () => {
    throw new ApiError(409, {
      code: "REPORTS_AGENT_NOT_CONFIGURED",
      error: "no agent fixes reports: a person names its role in Settings",
    });
  });
  const read = api.getReportSettings as unknown as { mock: { calls: unknown[] } };
  try {
    fireEvent.click(await screen.findByLabelText("Ask an agent to fix this"));
    fireEvent.change(screen.getByLabelText("What went wrong"), { target: { value: "Broken" } });
    const before = read.mock.calls.length;
    fireEvent.click(screen.getByRole("button", { name: "Send report" }));
    expect(await screen.findByText(/no agent fixes reports/)).toBeTruthy();
    await waitFor(() => expect(read.mock.calls.length).toBeGreaterThan(before));
  } finally {
    view.unmount();
  }
});

test("with an agent set, ticking the box asks it, and the answer says whether it was there", async () => {
  const view = renderDialog(settings("CORE", "role:dispatch-fixer"), async () =>
    filed("CORE-13", "role:dispatch-fixer")
  );
  const read = spyOn(api, "getIssue").mockResolvedValue({
    ...filed("CORE-13", "role:dispatch-fixer"),
    route_holder: null,
    route_status: "no_holder",
  } as unknown as IssueDetails);
  try {
    const box = (await screen.findByLabelText("Ask an agent to fix this")) as HTMLInputElement;
    expect(box.checked).toBe(false);
    fireEvent.click(box);
    fireEvent.change(screen.getByLabelText("What went wrong"), { target: { value: "Broken" } });
    fireEvent.click(screen.getByRole("button", { name: "Send report" }));
    await waitFor(() => expect(view.sent).toHaveLength(1));
    expect(view.sent[0]?.input.ask_agent).toBe(true);
    expect(await screen.findByText(/No agent holds role:dispatch-fixer right now/)).toBeTruthy();
    expect(read).toHaveBeenCalledWith("CORE-13");
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

test("Send waits for words, and a refusal keeps them", async () => {
  const view = renderDialog(settings("CORE", null), async () => {
    throw new ApiError(409, {
      code: "REPORTS_NOT_CONFIGURED",
      error: "reports are not set up: a person names the project they go to in Settings",
    });
  });
  try {
    const field = await screen.findByLabelText("What went wrong");
    const send = screen.getByRole("button", { name: "Send report" }) as HTMLButtonElement;
    expect(send.disabled).toBe(true);
    fireEvent.change(field, { target: { value: "Broken" } });
    expect(send.disabled).toBe(false);
    fireEvent.click(send);
    expect(await screen.findByText(/reports are not set up/)).toBeTruthy();
    expect((screen.getByLabelText("What went wrong") as HTMLTextAreaElement).value).toBe("Broken");
  } finally {
    view.unmount();
  }
});
