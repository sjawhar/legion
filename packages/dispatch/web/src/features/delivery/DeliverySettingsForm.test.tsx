import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { seedDeliveryReads } from "../../__tests__/delivery-live-harness";
import { ApiError, api } from "../../api/client";
import type { DeliverySettings } from "../../api/types";
import { DeliverySettingsForm } from "./DeliverySettingsForm";

const record: DeliverySettings = {
  deploy_repo: "acme/widgets",
  deploy_workflow_path: ".github/workflows/deploy.yml",
  excluded_repos: ["acme/dojo", "acme/widgets-smoke"],
  last_error: null,
  last_event_at: null,
  last_reconcile_at: null,
  population_authors: ["octocat", "octocat-agent[bot]"],
  pr_checks_rollup_job_name: null,
  pr_checks_workflow_path: ".github/workflows/pr-checks.yml",
  production_job_names: ["widgets-release / widgets-release", "release / release"],
  production_apply_step_name: null,
  staging_apply_job_name: null,
  staging_e2e_job_name: null,
  updated_at: "2026-10-07T12:00:00Z",
  updated_by: { id: "alice@example.com", kind: "user" },
};

function renderForm() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <DeliverySettingsForm />
    </QueryClientProvider>
  );
  return queryClient;
}

function field(label: string): HTMLInputElement | HTMLTextAreaElement {
  return screen.getByLabelText(label) as HTMLInputElement | HTMLTextAreaElement;
}

test("the form starts from the stored record, one list entry per line", async () => {
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(record);

  try {
    renderForm();

    expect(await screen.findByDisplayValue("acme/widgets")).toBeDefined();
    expect(field("Deploy workflow").value).toBe(".github/workflows/deploy.yml");
    expect(field("Production jobs").value).toBe(
      "widgets-release / widgets-release\nrelease / release"
    );
    expect(field("PR checks workflow").value).toBe(".github/workflows/pr-checks.yml");
    expect(field("Population authors").value).toBe("octocat\noctocat-agent[bot]");
    expect(field("Excluded repositories").value).toBe("acme/dojo\nacme/widgets-smoke");
  } finally {
    cleanup();
    getDeliverySettings.mockRestore();
  }
});

test("with no record the fields are empty and their placeholders say what each holds", async () => {
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);

  try {
    renderForm();

    await screen.findByRole("button", { name: "Save delivery settings" });
    const placeholders = [
      ["Deploy repository", "owner/repo"],
      ["Deploy workflow", ".github/workflows/<deploy>.yml"],
      ["Production jobs", "One job name per line, as a deploy run lists it"],
      ["PR checks workflow", ".github/workflows/<checks>.yml"],
      ["Population authors", "One GitHub login per line"],
      ["Excluded repositories", "One owner/repo per line"],
    ];
    for (const [label, placeholder] of placeholders) {
      const input = field(label ?? "");
      expect(input.value).toBe("");
      expect(input.placeholder).toBe(placeholder ?? "");
    }
  } finally {
    cleanup();
    getDeliverySettings.mockRestore();
  }
});

test("Save sends the fields trimmed and each list split into its entries", async () => {
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue(record);

  try {
    const queryClient = renderForm();
    const reads = seedDeliveryReads(queryClient);
    const invalidate = spyOn(queryClient, "invalidateQueries");
    await screen.findByRole("button", { name: "Save delivery settings" });

    fireEvent.change(field("Deploy repository"), { target: { value: "acme/widgets" } });
    fireEvent.change(field("Deploy workflow"), {
      target: { value: ".github/workflows/deploy.yml " },
    });
    fireEvent.change(field("Production jobs"), {
      target: { value: " widgets-release / widgets-release \n\nrelease / release\n" },
    });
    fireEvent.change(field("PR checks workflow"), {
      target: { value: ".github/workflows/pr-checks.yml" },
    });
    fireEvent.change(field("Population authors"), {
      target: { value: " octocat\n\noctocat-agent[bot],  hubot \n" },
    });
    fireEvent.change(field("Excluded repositories"), {
      target: { value: "acme/dojo\r\n acme/widgets-smoke\n" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save delivery settings" }));

    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(1));
    expect(putDeliverySettings.mock.calls[0]?.[0]).toEqual({
      deploy_repo: "acme/widgets",
      deploy_workflow_path: ".github/workflows/deploy.yml",
      excluded_repos: ["acme/dojo", "acme/widgets-smoke"],
      population_authors: ["octocat", "octocat-agent[bot]", "hubot"],
      pr_checks_rollup_job_name: null,
      pr_checks_workflow_path: ".github/workflows/pr-checks.yml",
      production_job_names: ["widgets-release / widgets-release", "release / release"],
      production_apply_step_name: null,
      staging_apply_job_name: null,
      staging_e2e_job_name: null,
    });
    // A save refreshes the record and the delivery reads that are not live (an open run's
    // drill-down); a live view learns of the save from Dispatch, as a delta.
    await waitFor(() =>
      expect(invalidate.mock.calls.map(([filters]) => filters?.queryKey)).toEqual([
        ["delivery-settings"],
        ["delivery"],
      ])
    );
    expect(queryClient.getQueryState(reads.run)?.isInvalidated).toBe(true);
    expect(queryClient.getQueryState(reads.live)?.isInvalidated).toBe(false);
  } finally {
    cleanup();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

test("the three optional pipeline job names round-trip, and an emptied one saves null", async () => {
  const stored: DeliverySettings = {
    ...record,
    pr_checks_rollup_job_name: "checks-complete",
    staging_apply_job_name: "staging-release / apply",
    staging_e2e_job_name: "staging-e2e / run",
  };
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(stored);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue(stored);

  try {
    renderForm();
    await screen.findByDisplayValue("acme/widgets");
    expect(field("Staging end-to-end job").value).toBe("staging-e2e / run");
    expect(field("Staging deploy job").value).toBe("staging-release / apply");
    expect(field("PR checks roll-up job").value).toBe("checks-complete");
    expect(field("Staging end-to-end job").placeholder).toBe(
      "The staging end-to-end job, as a deploy run lists it (optional)"
    );
    expect(field("Staging deploy job").placeholder).toBe("The staging deploy job (optional)");
    expect(field("PR checks roll-up job").placeholder).toBe(
      "The PR-checks roll-up job the failing-jobs table leaves out (optional)"
    );
    expect(field("Staging end-to-end job").required).toBe(false);

    fireEvent.change(field("Staging deploy job"), { target: { value: " staging-release / go " } });
    fireEvent.change(field("PR checks roll-up job"), { target: { value: "   " } });
    fireEvent.click(screen.getByRole("button", { name: "Save delivery settings" }));

    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(1));
    expect(putDeliverySettings.mock.calls[0]?.[0]).toMatchObject({
      pr_checks_rollup_job_name: null,
      staging_apply_job_name: "staging-release / go",
      staging_e2e_job_name: "staging-e2e / run",
    });
  } finally {
    cleanup();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

test("the production apply step round-trips trimmed and an empty value saves null", async () => {
  const stored: DeliverySettings = {
    ...record,
    production_apply_step_name: "Run ./.github/actions/apply",
  };
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(stored);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue(stored);

  try {
    renderForm();
    await screen.findByDisplayValue("acme/widgets");
    expect(field("Production apply step").value).toBe("Run ./.github/actions/apply");
    fireEvent.change(field("Production apply step"), {
      target: { value: " Run ./.github/actions/apply " },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save delivery settings" }));
    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(1));
    expect(putDeliverySettings.mock.calls[0]?.[0]).toMatchObject({
      production_apply_step_name: "Run ./.github/actions/apply",
    });

    fireEvent.change(field("Production apply step"), { target: { value: "   " } });
    fireEvent.click(screen.getByRole("button", { name: "Save delivery settings" }));
    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(2));
    expect(putDeliverySettings.mock.calls[1]?.[0]).toMatchObject({
      production_apply_step_name: null,
    });
  } finally {
    cleanup();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

test("a required field holding only whitespace is empty, so Save is refused before any request", async () => {
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue(record);
  const filled: [string, string][] = [
    ["Deploy repository", "acme/widgets"],
    ["Deploy workflow", ".github/workflows/deploy.yml"],
    ["Production jobs", "widgets-release"],
    ["PR checks workflow", ".github/workflows/pr-checks.yml"],
    ["Population authors", "octocat"],
  ];
  const blank: [string, string][] = [
    ["Deploy workflow", "   "],
    ["Production jobs", " \n\t \n"],
    ["PR checks workflow", " "],
    ["Population authors", " \n,\n  , "],
  ];

  try {
    renderForm();
    await screen.findByRole("button", { name: "Save delivery settings" });
    for (const [label, value] of filled) {
      fireEvent.change(field(label), { target: { value } });
    }

    for (const [label, whitespace] of blank) {
      fireEvent.change(field(label), { target: { value: whitespace } });
      expect(field(label).value).toBe("");
      // A save TanStack started would call the API only after its own await, so flush first.
      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: "Save delivery settings" }));
      });
      expect(putDeliverySettings).not.toHaveBeenCalled();
      fireEvent.change(field(label), {
        target: { value: filled.find(([name]) => name === label)?.[1] },
      });
    }

    // With every field holding text again the same button saves, so the refusals above were the
    // whitespace's and nothing else's.
    fireEvent.click(screen.getByRole("button", { name: "Save delivery settings" }));
    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(1));
  } finally {
    cleanup();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

test("a refused save shows the server's message as it wrote it, and the draft stays", async () => {
  const message =
    "the GitHub App is not installed on acme/widgets: its token cannot read acme/widgets";
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(record);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockRejectedValue(
    new ApiError(409, { code: "DELIVERY_SETTINGS_ACCESS", error: message })
  );

  try {
    renderForm();
    await screen.findByDisplayValue("acme/widgets");

    fireEvent.change(field("Deploy repository"), { target: { value: "acme/elsewhere" } });
    fireEvent.click(screen.getByRole("button", { name: "Save delivery settings" }));

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(message);
    expect(field("Deploy repository").value).toBe("acme/elsewhere");
  } finally {
    cleanup();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});
