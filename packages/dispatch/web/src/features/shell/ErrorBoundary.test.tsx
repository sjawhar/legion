import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { Link, MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

import { ErrorBoundary } from "./ErrorBoundary";

function Throwing(): never {
  throw new Error("payload.deliveries is not iterable");
}

test("a render error names the region and its cause instead of blanking the tree", () => {
  const logged: unknown[] = [];
  const consoleError = console.error;
  console.error = (...args: unknown[]) => void logged.push(args);
  const view = render(
    <div>
      <p>sidebar</p>
      <ErrorBoundary region="this page">
        <Throwing />
      </ErrorBoundary>
    </div>
  );
  try {
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent("Something went wrong in this page.");
    expect(alert).toHaveTextContent("payload.deliveries is not iterable");
    // Everything outside the boundary keeps rendering: a failed region is not a blank page.
    expect(screen.getByText("sidebar")).toBeTruthy();
    expect(logged.length).toBeGreaterThan(0);
  } finally {
    console.error = consoleError;
    view.unmount();
  }
});

function NavigationHarness(): ReactNode {
  const location = useLocation();
  return (
    <>
      <Link to="/">Inbox</Link>
      <ErrorBoundary region="this page" resetKey={location.pathname}>
        <Routes>
          <Route element={<p>Inbox page</p>} path="/" />
          <Route element={<Throwing />} path="/issues/:key" />
        </Routes>
      </ErrorBoundary>
    </>
  );
}

test("the sidebar Inbox navigation clears a failed page boundary for the next route", async () => {
  const user = userEvent.setup();
  const consoleError = console.error;
  console.error = () => {};
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <NavigationHarness />
    </MemoryRouter>
  );
  try {
    expect(screen.getByRole("alert")).toBeTruthy();

    await user.click(screen.getByRole("link", { name: /^Inbox$/ }));

    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByText("Inbox page")).toBeTruthy();
  } finally {
    console.error = consoleError;
    view.unmount();
  }
});

test("a child that renders keeps the boundary invisible", () => {
  const view = render(
    <ErrorBoundary region="this page">
      <p>content</p>
    </ErrorBoundary>
  );
  try {
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByText("content")).toBeTruthy();
  } finally {
    view.unmount();
  }
});
