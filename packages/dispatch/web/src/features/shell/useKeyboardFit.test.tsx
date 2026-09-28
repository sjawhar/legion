import { afterEach, expect, test } from "bun:test";
import { render } from "@testing-library/react";
import { useRef } from "react";

import { useKeyboardFit } from "./useKeyboardFit";

/** iOS Safari with its keyboard up: the visual viewport is 500 px tall at the top of the page. */
function keyboardUp(): void {
  const viewport = Object.assign(new EventTarget(), { height: 500, offsetTop: 0 });
  Object.defineProperty(window, "visualViewport", { configurable: true, value: viewport });
}

afterEach(() => {
  Reflect.deleteProperty(window, "visualViewport");
});

/** A shell whose composer already has focus when it mounts: React focuses an `autoFocus`
 *  textarea before the shell's layout effect runs, so no listener hears that focus. */
function Shell({ enabled }: { enabled: boolean }) {
  const main = useRef<HTMLElement>(null);
  useKeyboardFit(main, enabled);
  return (
    <main data-testid="main" ref={main}>
      {/* biome-ignore lint/a11y/noAutofocus: the focus restored with the keyboard up is the case under test */}
      <textarea autoFocus />
    </main>
  );
}

function customProperties(element: HTMLElement): string[] {
  return Array.from(element.style).filter((name) => name.startsWith("--"));
}

test("a route that mounts with its composer focused and the keyboard up is capped before any event", () => {
  keyboardUp();
  const view = render(<Shell enabled />);
  try {
    const main = view.getByTestId("main");
    expect(document.activeElement?.tagName).toBe("TEXTAREA");
    expect(main.style.maxHeight).toBe("500px");
    expect(customProperties(main)).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("a route the shell does not fit is never capped, whatever has focus", () => {
  keyboardUp();
  const view = render(<Shell enabled={false} />);
  try {
    const main = view.getByTestId("main");
    window.visualViewport?.dispatchEvent(new Event("resize"));
    expect(main.style.maxHeight).toBe("");
    expect(customProperties(main)).toEqual([]);
  } finally {
    view.unmount();
  }
});
