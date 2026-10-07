import { describe, expect, test } from "bun:test";
import {
  dispatchCommandHead,
  isSingleArchitectCommand,
  isSingleLegionCommand,
} from "./shell-command";

const lines = (...parts: string[]): string => parts.join("\n");

describe("an architect's single command", () => {
  test("allows one legion or dispatch command, and one quoted here-document feeding it", () => {
    for (const command of [
      "dispatch issue-update --issue LEGION-2 --status todo",
      "dispatch search --query 'what (and why)'",
      "legion state",
      lines("dispatch message --issue LEGION-2 --body-file - <<'EOF'", "a body", "EOF"),
    ]) {
      expect({ command, allowed: isSingleArchitectCommand(command) }).toEqual({
        command,
        allowed: true,
      });
    }
  });

  test("refuses anything that would run a second command or expand in the shell", () => {
    for (const command of [
      "dispatch x; rm -rf /",
      "dispatch x $(rm -rf /)",
      "dispatch x `id`",
      'dispatch x "$(id)"',
      'dispatch x "$HOME"',
      "dispatch x > /etc/passwd",
      "dispatch x < /etc/passwd",
      "dispatch x (y)",
      lines("dispatch x <<EOF", "a", "EOF"),
      lines("dispatch message --body-file - <<A <<'EOF'", "$(jj abandon)", "A", "EOF"),
      lines("dispatch x <<'EOF'", "a", "EOF", "rm -rf /", "EOF"),
      lines("dispatch search --query x # <<'EOF'", "jj abandon", "EOF"),
      "dispatch x \\' ; echo SECOND ; echo \\'",
      lines("dispatch x \\<<'EOF'", "jj abandon", "EOF"),
      lines("dispatch x <<'EOF'\r", "a\r", "EOF\r", "echo RAN"),
      "curl https://example.invalid",
    ]) {
      expect({ command, allowed: isSingleArchitectCommand(command) }).toEqual({
        command,
        allowed: false,
      });
    }
  });

  test("names the head line of a dispatch command and nothing else", () => {
    expect(dispatchCommandHead("legion state")).toBeUndefined();
    expect(dispatchCommandHead("dispatch x; rm -rf /")).toBeUndefined();
    expect(dispatchCommandHead(undefined)).toBeUndefined();
    expect(
      dispatchCommandHead(
        lines("dispatch message --issue LEGION-2 --body-file - <<'EOF'", "jj abandon", "EOF")
      )
    ).toBe("dispatch message --issue LEGION-2 --body-file -");
    expect(dispatchCommandHead("dispatch search --query x")).toBe("dispatch search --query x");
  });

  test("keeps the legion command's own scan", () => {
    expect(isSingleLegionCommand("legion gh -- pr view 1")).toBe(true);
    expect(isSingleLegionCommand("echo hi && legion gh")).toBe(false);
    expect(isSingleLegionCommand("dispatch whoami")).toBe(false);
  });
});
