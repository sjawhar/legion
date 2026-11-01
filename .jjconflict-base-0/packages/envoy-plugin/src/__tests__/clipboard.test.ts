import { describe, expect, it, mock } from "bun:test";
import { copyNative, copyToClipboard } from "../clipboard";

describe("copyToClipboard", () => {
  it("copies via the renderer OSC 52 writer and does not fall back when it succeeds", () => {
    const copyToClipboardOSC52 = mock((_text: string) => true);
    const ok = copyToClipboard("ses_abc", { copyToClipboardOSC52 });
    expect(ok).toBe(true);
    expect(copyToClipboardOSC52).toHaveBeenCalledWith("ses_abc");
  });
});

describe("copyNative", () => {
  it("uses wl-copy on Linux/Wayland when available", () => {
    const run = mock((_cmd: string, _args: string[], _text: string) => true);
    const ok = copyNative("ses_abc", {
      os: "linux",
      env: { WAYLAND_DISPLAY: "wayland-0" },
      which: (cmd) => cmd === "wl-copy",
      run,
    });
    expect(ok).toBe(true);
    expect(run).toHaveBeenCalledWith("wl-copy", [], "ses_abc");
  });

  it("falls back to xclip on Linux/X11", () => {
    const run = mock((_cmd: string, _args: string[], _text: string) => true);
    const ok = copyNative("ses_abc", {
      os: "linux",
      env: {},
      which: (cmd) => cmd === "xclip",
      run,
    });
    expect(ok).toBe(true);
    expect(run).toHaveBeenCalledWith("xclip", ["-selection", "clipboard"], "ses_abc");
  });

  it("falls back to xsel when xclip is missing", () => {
    const run = mock((_cmd: string, _args: string[], _text: string) => true);
    copyNative("x", { os: "linux", env: {}, which: (cmd) => cmd === "xsel", run });
    expect(run).toHaveBeenCalledWith("xsel", ["--clipboard", "--input"], "x");
  });

  it("uses osascript on macOS with escaped quotes", () => {
    const run = mock((_cmd: string, _args: string[], _text: string) => true);
    copyNative('a"b\\c', { os: "darwin", which: () => true, run });
    expect(run).toHaveBeenCalledWith("osascript", ["-e", 'set the clipboard to "a\\"b\\\\c"'], "");
  });

  it("returns false on Linux when no clipboard tool is installed", () => {
    const run = mock(() => true);
    const ok = copyNative("x", { os: "linux", env: {}, which: () => false, run });
    expect(ok).toBe(false);
    expect(run).not.toHaveBeenCalled();
  });
});
