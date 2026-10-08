import { describe, expect, test } from "bun:test";
import {
  dispatchCommandHead,
  isSingleArchitectCommand,
  isSingleLegionCommand,
} from "./shell-command";

/** Whether a real non-interactive bash, fed `command` and then a second command over a pipe as
 * the `bash` tool's persistent shell is fed, runs that second command: false when `command` left a
 * here-document open that swallowed it. `dispatch` and `legion` are stand-ins that drain stdin. */
function bashRunsNextCommand(command: string): boolean {
  const script = [
    "dispatch() { cat >/dev/null; }",
    "legion() { cat >/dev/null; }",
    command,
    "echo NEXT_COMMAND_RAN",
    "",
  ].join("\n");
  const run = Bun.spawnSync(["bash", "--noprofile", "--norc"], {
    stdin: new TextEncoder().encode(script),
    stdout: "pipe",
    stderr: "pipe",
  });
  return run.stdout.toString().includes("NEXT_COMMAND_RAN");
}

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

  test("ends a here-document where bash does: a tab before the delimiter counts only under <<-", () => {
    const tabbedPlain = lines("dispatch x <<'EOF'", "\tEOF");
    const tabbedMessage = lines(
      "dispatch message --issue LEGION-2 --body-file - <<'EOF'",
      "SOME TEXT HERE",
      "\tEOF"
    );
    const tabbedDash = lines(
      "dispatch message --issue LEGION-2 --body-file - <<-'EOF'",
      "\tbody",
      "\tEOF"
    );
    const tabbedBodyLine = lines("dispatch x <<'EOF'", "\tEOF", "EOF");
    const canonical = lines(
      "dispatch message --issue LEGION-2 --body-file - <<'EOF'",
      "a body",
      "EOF"
    );

    expect(isSingleArchitectCommand(tabbedPlain)).toBe(false);
    expect(dispatchCommandHead(tabbedPlain)).toBeUndefined();
    expect(isSingleArchitectCommand(tabbedMessage)).toBe(false);
    expect(dispatchCommandHead(tabbedDash)).toBe("dispatch message --issue LEGION-2 --body-file -");
    expect(dispatchCommandHead(tabbedBodyLine)).toBe("dispatch x");
    expect(dispatchCommandHead(canonical)).toBe("dispatch message --issue LEGION-2 --body-file -");
  });

  test("every here-document it accepts is one a real bash closes before the next command", () => {
    const accepted = [
      lines("dispatch message --issue LEGION-2 --body-file - <<'EOF'", "a body", "EOF"),
      lines("dispatch message --issue LEGION-2 --body-file - <<-'EOF'", "\tbody", "\tEOF"),
      lines("dispatch x <<'EOF'", "\tEOF", "EOF"),
      lines("legion handoff write <<'EOF'", "{}", "EOF"),
    ];
    for (const command of accepted) {
      expect({ command, accepted: isSingleArchitectCommand(command) }).toEqual({
        command,
        accepted: true,
      });
      expect({ command, nextRan: bashRunsNextCommand(command) }).toEqual({
        command,
        nextRan: true,
      });
    }
    // The divergence the scan refuses: bash keeps this here-document open and swallows the next
    // command as its body.
    const swallowing = lines("dispatch x <<'EOF'", "\tEOF");
    expect(bashRunsNextCommand(swallowing)).toBe(false);
    expect(isSingleArchitectCommand(swallowing)).toBe(false);
  });

  test("keeps the legion command's own scan", () => {
    expect(isSingleLegionCommand("legion gh -- pr view 1")).toBe(true);
    expect(isSingleLegionCommand("echo hi && legion gh")).toBe(false);
    expect(isSingleLegionCommand("dispatch whoami")).toBe(false);
  });
});
