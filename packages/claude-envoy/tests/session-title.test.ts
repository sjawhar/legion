import { expect, test } from "bun:test"
import { execFileSync } from "node:child_process"
import { existsSync, mkdtempSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import {
  isAncestor,
  queryTmux,
  sessionTitle,
  type TmuxQuery,
  tmuxPaneTitle,
} from "../src/session-title"

const SESSION_ID = "3084299d-32a7-41a3-ba9b-f61675b03b8d"
const SERVER_PID = "4242"
const TMUX = `/tmp/tmux-1000/default,${SERVER_PID},0`
/** The pane's first process, and this session's ancestors, which hold it. */
const PANE_PID = "777"
const ANCESTORS: ReadonlySet<string> = new Set(["900", "800", PANE_PID])
const ours = (panePid: string) => ANCESTORS.has(panePid)

/** A tmux server `pid` that answers every query with a pane in `session`, its window `window`
 *  with `automatic-rename` `automatic`, in group `group`, tab-joined as `display-message -p`
 *  prints them, and records the socket and pane each query named. */
function tmuxAnswering(
  session: string,
  window: string,
  automatic: string,
  {
    group = "",
    pid = SERVER_PID,
    panePid = PANE_PID,
  }: { group?: string; pid?: string; panePid?: string } = {},
): TmuxQuery & { readonly asked: string[][] } {
  const asked: string[][] = []
  const query = (socket: string, pane: string) => {
    asked.push([socket, pane])
    return `${[session, window, automatic, group, pid, panePid].join("\t")}\n`
  }
  return Object.assign(query, { asked })
}

const noTmux: TmuxQuery = () => {
  throw new Error("tmux must not be asked")
}

const paneTitle = (query: TmuxQuery) => tmuxPaneTitle("s", SERVER_PID, "%3", ours, query)

test("ENVOY_SESSION_TITLE names the session, ahead of tmux, on one line", () => {
  const title = sessionTitle(
    { ENVOY_SESSION_TITLE: "  review\n  the \u0007 listener ", TMUX, TMUX_PANE: "%3" },
    "/work/legion",
    { query: noTmux },
  )
  expect(title(SESSION_ID)).toBe("review the listener")
  expect(title("ses_after_clear")).toBe("review the listener")
})

test("a blank or invisible ENVOY_SESSION_TITLE falls through to tmux, asked of the server TMUX names", () => {
  for (const blank of [
    "  ",
    "\u200b",
    " \u200b\u2060 ",
    "\u0001",
    "\u001b",
    "\ufe0f",
    "\u3164",
    "\u2800",
  ]) {
    const tmux = tmuxAnswering("work", "review", "0")
    const title = sessionTitle(
      { ENVOY_SESSION_TITLE: blank, TMUX, TMUX_PANE: "%3" },
      "/work/legion",
      { query: tmux, ownPane: ours },
    )
    expect(title(SESSION_ID)).toBe("work:review 3084299d")
    expect(tmux.asked).toEqual([["/tmp/tmux-1000/default", "%3"]])
  }
})

test("the name the user gave the session stands alone, with no short id after it", () => {
  const title = sessionTitle({}, "/work/legion", {
    query: noTmux,
    ownPane: ours,
    chosen: () => "review the listener",
  })
  expect(title(SESSION_ID)).toBe("review the listener")
})

test("ENVOY_SESSION_TITLE beats the chosen name, which is never even read", () => {
  const title = sessionTitle({ ENVOY_SESSION_TITLE: "pinned" }, "/work/legion", {
    query: noTmux,
    ownPane: ours,
    chosen: () => {
      throw new Error("the chosen name must not be read")
    },
  })
  expect(title(SESSION_ID)).toBe("pinned")
})

test("a blank, invisible or missing chosen name falls through to the directory and short id", () => {
  for (const blank of [undefined, "  ", "\u200b", "\u001b[31m\u001b[0m"]) {
    const title = sessionTitle({}, "/work/legion", {
      query: noTmux,
      ownPane: ours,
      chosen: () => blank,
    })
    expect(title(SESSION_ID)).toBe("legion 3084299d")
  }
})

test("a multi-line or colour-coded chosen name is put on one line", () => {
  const title = sessionTitle({}, "/work/legion", {
    query: noTmux,
    ownPane: ours,
    chosen: () => "\u001b[31mreview\n  the \u0007 listener\u001b[0m\n",
  })
  expect(title(SESSION_ID)).toBe("review the listener")
})

test("the chosen name is read on every call, so a rename reaches the next registration", () => {
  let chosen: string | undefined
  const title = sessionTitle({}, "/work/legion", {
    query: noTmux,
    ownPane: ours,
    chosen: () => chosen,
  })
  expect(title(SESSION_ID)).toBe("legion 3084299d")

  chosen = "review the listener"

  expect(title(SESSION_ID)).toBe("review the listener")
  chosen = undefined
  expect(title(SESSION_ID)).toBe("legion 3084299d")
})

test("the socket is TMUX up to its last two commas, so a path holding a comma still names it", () => {
  const tmux = tmuxAnswering("work", "review", "0")
  sessionTitle({ TMUX: `/tmp/a,b/default,${SERVER_PID},0`, TMUX_PANE: "%3" }, "/work/legion", {
    query: tmux,
    ownPane: ours,
  })
  expect(tmux.asked).toEqual([["/tmp/a,b/default", "%3"]])
})

test("a server started again on the same socket lends no name: its pane ids are another's", () => {
  // A `claude` that outlived its tmux server keeps the old TMUX, and the new server gives out %3
  // again, to someone else's pane.
  const restarted = tmuxAnswering("unrelated", "secret", "0", { pid: "9999" })
  expect(paneTitle(restarted)).toBeUndefined()
  expect(
    sessionTitle({ TMUX, TMUX_PANE: "%3" }, "/work/legion", {
      query: restarted,
      ownPane: ours,
    })(SESSION_ID),
  ).toBe("legion 3084299d")
})

test("a window whose automatic-rename is off lends its name", () => {
  expect(paneTitle(tmuxAnswering("work", "review", "0"))).toBe("work:review")
  // A tmux that prints the flag as on/off rather than 1/0.
  expect(paneTitle(tmuxAnswering("work", "review", "off"))).toBe("work:review")
})

test("a pane of grouped sessions is named after the group, whichever session tmux answers with", () => {
  expect(paneTitle(tmuxAnswering("work-view", "review", "0", { group: "work" }))).toBe(
    "work:review",
  )
  expect(paneTitle(tmuxAnswering("work", "claude", "1"))).toBe("work")
})

test("a tmux too old to expand the format lends no name", () => {
  const unexpanded: TmuxQuery = () =>
    "#{session_name}\t#{window_name}\t#{automatic-rename}\t#{session_group}\t#{pid}\t#{pane_pid}\n"
  expect(
    tmuxPaneTitle("s", "#{pid}", "%3", (panePid) => panePid === "#{pane_pid}", unexpanded),
  ).toBeUndefined()
})

test("an automatically named window reads as the session alone, never its index", () => {
  // automatic-rename names the window after the running command, which is the same in every pane,
  // and a window index moves under renumber-windows.
  expect(paneTitle(tmuxAnswering("work", "claude", "1"))).toBe("work")
  // A tmux too old to expand option names leaves the flag empty: the window's name is not trusted.
  expect(paneTitle(tmuxAnswering("work", "claude", ""))).toBe("work")
})

test("a session named only by digits lends no name in a window nobody named, so the project names it", () => {
  // Plain `tmux` names its first session `0`, which on every machine says nothing.
  expect(paneTitle(tmuxAnswering("0", "claude", "1"))).toBeUndefined()
  expect(
    sessionTitle({ TMUX, TMUX_PANE: "%3" }, "/work/legion", {
      query: tmuxAnswering("0", "claude", "1"),
      ownPane: ours,
    })(SESSION_ID),
  ).toBe("legion 3084299d")
  // A window someone named still names it.
  expect(paneTitle(tmuxAnswering("0", "review", "0"))).toBe("0:review")
})

test("sessions sharing a tmux window or session keep apart by their short ids", () => {
  const tmux = tmuxAnswering("split", "review", "0")
  const title = sessionTitle({ TMUX, TMUX_PANE: "%3" }, "/work/legion", {
    query: tmux,
    ownPane: ours,
  })
  expect(title("aaaaaaaa-1111")).toBe("split:review aaaaaaaa")
  expect(title("bbbbbbbb-2222")).toBe("split:review bbbbbbbb")
})

test("outside tmux, or when tmux cannot answer, the title is the project directory and the short id", () => {
  expect(sessionTitle({}, "/work/legion", { query: noTmux })(SESSION_ID)).toBe("legion 3084299d")
  // A pane id names a pane only on its own server: without TMUX, tmux is not asked.
  expect(sessionTitle({ TMUX_PANE: "%3" }, "/work/legion", { query: noTmux })(SESSION_ID)).toBe(
    "legion 3084299d",
  )
  expect(
    sessionTitle({ TMUX, TMUX_PANE: "%3" }, "/work/legion", {
      query: () => undefined,
      ownPane: ours,
    })(SESSION_ID),
  ).toBe("legion 3084299d")
  expect(
    sessionTitle({ TMUX, TMUX_PANE: "%3" }, "/work/legion", {
      query: tmuxAnswering("", "zsh", "1"),
      ownPane: ours,
    })(SESSION_ID),
  ).toBe("legion 3084299d")
  expect(
    sessionTitle({ TMUX, TMUX_PANE: "%3" }, "/work/legion", {
      query: () => "work\n",
      ownPane: ours,
    })(SESSION_ID),
  ).toBe("legion 3084299d")
})

test("a project directory's own name is cleaned like any other title", () => {
  expect(sessionTitle({}, "/work/a\u202eb", { query: noTmux })(SESSION_ID)).toBe("ab 3084299d")
  expect(sessionTitle({}, "/work/two\nlines", { query: noTmux })(SESSION_ID)).toBe(
    "two lines 3084299d",
  )
})

test("a title of combining marks alone falls through", () => {
  expect(
    sessionTitle({ ENVOY_SESSION_TITLE: "\u0301" }, "/work/legion", { query: noTmux })(SESSION_ID),
  ).toBe("legion 3084299d")
})

test("the short id follows the id it is given, so a /clear handoff's new id shows", () => {
  const title = sessionTitle({}, "/work/legion/", { query: noTmux })
  expect(title("aaaaaaaa-1111")).toBe("legion aaaaaaaa")
  expect(title("bbbbbbbb-2222")).toBe("legion bbbbbbbb")
  expect(sessionTitle({}, "/", { query: noTmux })("cccccccc-3333")).toBe("/ cccccccc")
})

test("queryTmux answers undefined for a server that does not exist", () => {
  expect(queryTmux("/nonexistent/tmux.sock", "%0", "#{session_name}")).toBeUndefined()
})

test("a TMUX_PANE left over from elsewhere names no pane but this session's own", () => {
  // tmux hands a popup or run-shell job the TMUX_PANE its server was started with, which names a
  // pane this session is not in.
  const elsewhere = tmuxAnswering("other", "secret", "0", { panePid: "555" })
  expect(paneTitle(elsewhere)).toBeUndefined()
  expect(
    sessionTitle({ TMUX, TMUX_PANE: "%3" }, "/work/legion", {
      query: elsewhere,
      ownPane: ours,
    })(SESSION_ID),
  ).toBe("legion 3084299d")
})

test("isAncestor finds this process and its parent, and not a process outside its chain", () => {
  expect(isAncestor(String(process.pid))).toBe(true)
  expect(isAncestor(String(process.ppid))).toBe(true)
  expect(isAncestor("1")).toBe(false)
  const child = Bun.spawnSync([process.execPath, "-e", "process.stdout.write(String(process.pid))"])
  expect(isAncestor(child.stdout.toString())).toBe(false)
})

test("a title that shows nothing once cleaned falls through, from every source", () => {
  for (const blank of ["\u001b[0m", "\u001b[31m\u001b[0m", "\u001b[1m\u200b"]) {
    expect(
      sessionTitle({ ENVOY_SESSION_TITLE: blank }, "/work/legion", { query: noTmux })(SESSION_ID),
    ).toBe("legion 3084299d")
  }
  expect(paneTitle(tmuxAnswering("\u200b", "claude", "1"))).toBeUndefined()
  expect(paneTitle(tmuxAnswering("work", "\u200b", "0"))).toBe("work")
  expect(sessionTitle({}, "/work/\u001b[0m", { query: noTmux })(SESSION_ID)).toBe(
    "session 3084299d",
  )
})

test("a colour code (a CSI sequence) leaves nothing of itself in a title", () => {
  expect(
    sessionTitle({ ENVOY_SESSION_TITLE: "\t\u001b[31mred\u001b[0m" }, "/work/legion", {
      query: noTmux,
    })(SESSION_ID),
  ).toBe("red")
})

test("a title keeps no bidirectional override, and keeps a private-use glyph", () => {
  expect(
    sessionTitle({ ENVOY_SESSION_TITLE: "\u202eevil.txt" }, "/work/legion", { query: noTmux })(
      SESSION_ID,
    ),
  ).toBe("evil.txt")
  expect(paneTitle(tmuxAnswering("adv", "\u202ewin", "0"))).toBe("adv:win")
  expect(
    sessionTitle({ ENVOY_SESSION_TITLE: "\uf121" }, "/work/legion", { query: noTmux })(SESSION_ID),
  ).toBe("\uf121")
  // A zero-width joiner inside an emoji sequence stays.
  expect(
    sessionTitle({ ENVOY_SESSION_TITLE: "\u{1f469}\u200d\u{1f4bb} dev" }, "/work/legion", {
      query: noTmux,
    })(SESSION_ID),
  ).toBe("\u{1f469}\u200d\u{1f4bb} dev")
})

// This one drives a real tmux server, and skips without one, as the tmux runtime's own tests do.
const hasTmux = Bun.which("tmux") !== null

test.skipIf(!hasTmux)(
  "a real tmux server answers a pane it does not have with no answer, and names its own pid",
  () => {
    // A running server answers a pane it does not have with an empty line and exit status 0.
    const directory = mkdtempSync(join(tmpdir(), "title-tmux-"))
    const socket = join(directory, "tmux.sock")
    execFileSync("tmux", ["-S", socket, "-f", "/dev/null", "new-session", "-d", "-s", "probe"])
    try {
      expect(queryTmux(socket, "%999999999", "#{session_name}")).toBeUndefined()
      const pid = execFileSync("tmux", ["-S", socket, "display-message", "-p", "#{pid}"], {
        encoding: "utf8",
      }).trim()
      // `-u`, as queryTmux passes it: with neither TMUX nor a UTF-8 locale, tmux prints tabs as `_`.
      const [pane = "", panePid = ""] = execFileSync(
        "tmux",
        ["-u", "-S", socket, "display-message", "-p", "#{pane_id}\t#{pane_pid}"],
        { encoding: "utf8" },
      )
        .trim()
        .split("\t")
      // Unbounded, unlike queryTmux: a loaded machine can take tmux past one second, and this
      // test is about reading the real server's answer, not the bound.
      const unbounded: TmuxQuery = (at, target, format) =>
        execFileSync("tmux", ["-u", "-S", at, "display-message", "-p", "-t", target, format], {
          encoding: "utf8",
        })
      expect(
        tmuxPaneTitle(socket, pid, pane, (candidate) => candidate === panePid, unbounded),
      ).toBe("probe")
      expect(
        tmuxPaneTitle(socket, "1", pane, (candidate) => candidate === panePid, unbounded),
      ).toBeUndefined()
      // A pane whose process is not one of this session's ancestors is not this session's.
      expect(
        tmuxPaneTitle(socket, pid, pane, (candidate) => candidate === "1", unbounded),
      ).toBeUndefined()
    } finally {
      execFileSync("tmux", ["-S", socket, "kill-server"])
      rmSync(directory, { recursive: true, force: true })
    }
  },
)

test.skipIf(!hasTmux)(
  "queryTmux keeps the tabs between fields when its process has neither TMUX nor a UTF-8 locale",
  () => {
    const directory = mkdtempSync(join(tmpdir(), "title-tmux-"))
    const socket = join(directory, "tmux.sock")
    execFileSync("tmux", ["-S", socket, "-f", "/dev/null", "new-session", "-d", "-s", "probe"])
    try {
      const pane = execFileSync("tmux", ["-S", socket, "display-message", "-p", "#{pane_id}"], {
        encoding: "utf8",
      }).trim()
      const pid = execFileSync("tmux", ["-S", socket, "display-message", "-p", "#{pid}"], {
        encoding: "utf8",
      }).trim()
      // A child of its own with no TMUX, LANG, LC_ALL or LC_CTYPE: tmux prints a tab as `_` to a
      // client with neither TMUX nor a UTF-8 locale, and this process's own environment may have one.
      const source = join(import.meta.dir, "../src/session-title.ts")
      const child = Bun.spawnSync(
        [
          process.execPath,
          "-e",
          `import { queryTmux } from ${JSON.stringify(source)}; process.stdout.write(JSON.stringify(queryTmux(${JSON.stringify(socket)}, ${JSON.stringify(pane)}, "#{session_name}\\t#{pid}")))`,
        ],
        { env: { PATH: process.env["PATH"] ?? "", HOME: process.env["HOME"] ?? "" } },
      )
      expect(child.stdout.toString()).toBe(JSON.stringify(`probe\t${pid}\n`))
    } finally {
      execFileSync("tmux", ["-S", socket, "kill-server"])
      rmSync(directory, { recursive: true, force: true })
    }
  },
)

test("queryTmux is bounded even by a tmux that ignores SIGTERM", () => {
  const directory = mkdtempSync(join(tmpdir(), "title-tmux-"))
  const marker = join(directory, "ran")
  writeFileSync(join(directory, "tmux"), `#!/bin/sh\ntrap "" TERM\ntouch ${marker}\nsleep 5\n`, {
    mode: 0o755,
  })
  // A child of its own, so the stub is first on the PATH it starts with: execFileSync looks
  // `tmux` up on the PATH the process was launched with. The child times its own call and prints
  // the milliseconds beside the answer, because what is bounded is `queryTmux`, not Bun's
  // start-up and this import, which on a loaded machine take longer than the bound itself.
  const source = join(import.meta.dir, "../src/session-title.ts")
  try {
    const child = Bun.spawnSync(
      [
        process.execPath,
        "-e",
        `import { queryTmux } from ${JSON.stringify(source)}; const started = Date.now(); const answer = queryTmux("/nonexistent/tmux.sock", "%0", "x"); process.stdout.write(\`\${String(answer)} \${Date.now() - started}\`)`,
      ],
      { env: { ...process.env, PATH: `${directory}:${process.env["PATH"] ?? ""}` } },
    )
    const [answer, elapsed] = child.stdout.toString().split(" ")
    expect(answer).toBe("undefined")
    expect(existsSync(marker)).toBe(true)
    // The stub ignores TERM and would sleep five seconds: the call waits out the one-second
    // timeout, then is killed rather than waited for.
    expect(Number(elapsed)).toBeGreaterThanOrEqual(1_000)
    expect(Number(elapsed)).toBeLessThan(3_000)
  } finally {
    rmSync(directory, { recursive: true, force: true })
  }
})
