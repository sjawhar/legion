import { expect, test } from "bun:test"
import { cp, mkdtemp, readdir, readFile, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { BUNDLE_ENTRYPOINTS, buildBundles } from "../scripts/build"

const packageRoot = resolve(import.meta.dir, "..")
const distDirectory = join(packageRoot, "dist")

interface JavaScriptToken {
  readonly kind: "identifier" | "string" | "punctuation"
  readonly value: string
}

const identifierCharacter = /[A-Za-z0-9_$]/

function tokenizeJavaScript(source: string): JavaScriptToken[] {
  const tokens: JavaScriptToken[] = []
  for (let index = 0; index < source.length; ) {
    const character = source[index]!
    if (/\s/.test(character)) {
      index += 1
      continue
    }
    if (character === "/" && source[index + 1] === "/") {
      const lineEnd = source.indexOf("\n", index + 2)
      if (lineEnd === -1) break
      index = lineEnd + 1
      continue
    }
    if (character === "/" && source[index + 1] === "*") {
      const commentEnd = source.indexOf("*/", index + 2)
      if (commentEnd === -1) break
      index = commentEnd + 2
      continue
    }
    if (character === "'" || character === '"' || character === "`") {
      const quote = character
      let value = ""
      index += 1
      while (index < source.length && source[index] !== quote) {
        if (source[index] === "\\" && index + 1 < source.length) index += 1
        value += source[index]!
        index += 1
      }
      if (source[index] === quote) index += 1
      tokens.push({ kind: "string", value })
      continue
    }
    if (identifierCharacter.test(character)) {
      const start = index
      do index += 1
      while (index < source.length && identifierCharacter.test(source[index]!))
      tokens.push({ kind: "identifier", value: source.slice(start, index) })
      continue
    }
    tokens.push({ kind: "punctuation", value: character })
    index += 1
  }
  return tokens
}

function scanImportMetaRequirePaths(source: string): string[] {
  const tokens = tokenizeJavaScript(source)
  const paths: string[] = []
  for (let index = 0; index + 6 < tokens.length; index += 1) {
    const importToken = tokens[index]!
    const firstDot = tokens[index + 1]!
    const metaToken = tokens[index + 2]!
    const secondDot = tokens[index + 3]!
    const requireToken = tokens[index + 4]!
    const openParenthesis = tokens[index + 5]!
    const path = tokens[index + 6]!
    if (
      importToken.value === "import" &&
      firstDot.value === "." &&
      metaToken.value === "meta" &&
      secondDot.value === "." &&
      requireToken.value === "require" &&
      openParenthesis.value === "(" &&
      path.kind === "string"
    ) {
      paths.push(path.value)
    }
  }
  return paths
}

function findExternalDependencySpecifiers(source: string): string[] {
  const paths = new Bun.Transpiler({ loader: "js" }).scanImports(source).map(({ path }) => path)
  paths.push(...scanImportMetaRequirePaths(source))
  return paths.filter((path) => path.includes("node_modules/"))
}

test("the committed bundle starts without node_modules and demands ENVOY_NATS_URL", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-dist-"))
  try {
    await cp(distDirectory, join(scratch, "dist"), { recursive: true })
    const run = Bun.spawn(["bun", join(scratch, "dist", "envoy-channel.js")], {
      cwd: scratch,
      env: {
        PATH: process.env["PATH"] ?? "",
        HOME: scratch,
        CLAUDE_CODE_SESSION_ID: "test",
        CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      },
      stdin: "ignore",
      stdout: "pipe",
      stderr: "pipe",
    })
    const [exitCode, stderr] = await Promise.all([run.exited, new Response(run.stderr).text()])

    expect(exitCode).toBe(1)
    expect(stderr).toContain("ENVOY_NATS_URL is required")
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

test("the dependency guard ignores comments and detects runtime node_modules specifiers", () => {
  const comments = [
    '// require("/tmp/node_modules/comment.js")',
    '/* import.meta.require("/tmp/node_modules/comment.js") */',
  ].join("\n")
  expect(findExternalDependencySpecifiers(comments)).toEqual([])

  const specifiers = [
    { source: 'require("/tmp/node_modules/require.js")', path: "/tmp/node_modules/require.js" },
    {
      source: 'import.meta.require("/tmp/node_modules/meta-require.js")',
      path: "/tmp/node_modules/meta-require.js",
    },
    { source: 'import("/tmp/node_modules/import.js")', path: "/tmp/node_modules/import.js" },
    {
      source: 'import dependency from "/tmp/node_modules/static-import.js"',
      path: "/tmp/node_modules/static-import.js",
    },
    {
      source: 'export { dependency } from "/tmp/node_modules/static-export.js"',
      path: "/tmp/node_modules/static-export.js",
    },
  ]
  const matches = findExternalDependencySpecifiers(specifiers.map(({ source }) => source).join("\n"))
  for (const { path } of specifiers) expect(matches).toContain(path)
})

test("the dependency guard detects calls after comment-like strings", () => {
  const cases = [
    {
      source: 'const url = "https://not-a-comment"; require("/tmp/node_modules/https.js")',
      path: "/tmp/node_modules/https.js",
    },
    {
      source:
        'const opener = "/*"; require("/tmp/node_modules/block.js"); const closer = "*/"',
      path: "/tmp/node_modules/block.js",
    },
  ]
  for (const { source, path } of cases) {
    expect(findExternalDependencySpecifiers(source)).toContain(path)
  }
})

test("the committed bundles contain no external dependency call specifiers", async () => {
  for (const name of Object.keys(BUNDLE_ENTRYPOINTS)) {
    const bundle = await readFile(join(distDirectory, `${name}.js`), "utf8")
    expect(findExternalDependencySpecifiers(bundle)).toEqual([])
  }
})

/** Where two bundles part ways, with 80 characters of each side around it. */
function describeMismatch(file: string, fresh: Buffer, current: Buffer): string {
  const shorter = Math.min(fresh.length, current.length)
  let offset = 0
  while (offset < shorter && fresh[offset] === current[offset]) offset += 1
  const context = (bytes: Buffer): string =>
    JSON.stringify(bytes.subarray(Math.max(0, offset - 80), offset + 80).toString("utf8"))
  return [
    `${file}: fresh build differs from committed dist/`,
    `fresh ${fresh.length} bytes, committed ${current.length} bytes, first difference at byte ${offset}`,
    `fresh:     ${context(fresh)}`,
    `committed: ${context(current)}`,
  ].join("\n")
}

test("rebuilding reproduces the committed bundle byte for byte", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-rebuild-"))
  try {
    await buildBundles(scratch)
    const committed = (await readdir(distDirectory)).sort()
    expect((await readdir(scratch)).sort()).toEqual(committed)
    for (const file of committed) {
      const fresh = await readFile(join(scratch, file))
      const current = await readFile(join(distDirectory, file))
      if (!fresh.equals(current)) throw new Error(describeMismatch(file, fresh, current))
    }
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
}, 30_000)
