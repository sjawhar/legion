import { expect, test } from "bun:test"
import { cp, mkdtemp, readdir, readFile, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { isAbsolute, join, resolve } from "node:path"
import ts from "typescript"
import { BUNDLE_ENTRYPOINTS, buildBundles } from "../scripts/build"

const packageRoot = resolve(import.meta.dir, "..")
const distDirectory = join(packageRoot, "dist")

async function findExternalDependencySpecifiers(source: string): Promise<string[]> {
  const resolverRoot = await mkdtemp(join(tmpdir(), "claude-envoy-module-resolution-"))
  try {
    const parsed = ts.createSourceFile(
      "bundle.js",
      source,
      ts.ScriptTarget.Latest,
      true,
      ts.ScriptKind.JS,
    )
    const specifiers: string[] = []
    const inspectSpecifier = (specifier: string): void => {
      try {
        const resolved = Bun.resolveSync(specifier, resolverRoot)
        const resolvesToBuiltin =
          resolved.startsWith("node:") ||
          resolved.startsWith("bun:") ||
          (resolved === specifier && !specifier.includes(":"))
        if (!resolvesToBuiltin || isAbsolute(resolved)) specifiers.push(specifier)
      } catch {
        specifiers.push(specifier)
      }
    }
    const visit = (node: ts.Node): void => {
      if (ts.isCallExpression(node)) {
        let kind: string | undefined
        if (ts.isIdentifier(node.expression) && node.expression.text === "require") {
          kind = "require()"
        } else if (
          ts.isPropertyAccessExpression(node.expression) &&
          node.expression.name.text === "require" &&
          ts.isMetaProperty(node.expression.expression) &&
          node.expression.expression.keywordToken === ts.SyntaxKind.ImportKeyword &&
          node.expression.expression.name.text === "meta"
        ) {
          kind = "import.meta.require()"
        } else if (node.expression.kind === ts.SyntaxKind.ImportKeyword) {
          kind = "import()"
        }
        if (kind) {
          const argument = node.arguments[0]
          if (!argument || !ts.isStringLiteral(argument)) {
            specifiers.push(`<non-literal ${kind}>`)
          } else {
            inspectSpecifier(argument.text)
          }
        }
      } else if (
        (ts.isImportDeclaration(node) || ts.isExportDeclaration(node)) &&
        node.moduleSpecifier &&
        ts.isStringLiteral(node.moduleSpecifier)
      ) {
        inspectSpecifier(node.moduleSpecifier.text)
      }
      ts.forEachChild(node, visit)
    }
    visit(parsed)
    return specifiers
  } finally {
    await rm(resolverRoot, { recursive: true, force: true })
  }
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

test("the dependency guard ignores comments and detects external module specifiers", async () => {
  const comments = [
    '// require("/tmp/node_modules/comment.js")',
    '/* import.meta.require("/tmp/node_modules/comment.js") */',
    'const mention = "require(\\"/tmp/node_modules/escaped.js\\")"',
  ].join("\n")
  expect(await findExternalDependencySpecifiers(comments)).toEqual([])

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
  const matches = await findExternalDependencySpecifiers(specifiers.map(({ source }) => source).join("\n"))
  for (const { path } of specifiers) expect(matches).toContain(path)
})

test("the dependency guard detects calls after comment-like strings", async () => {
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
    {
      source: 'const template = `${import.meta.require("/tmp/node_modules/template.js")}`',
      path: "/tmp/node_modules/template.js",
    },
    {
      source: 'const matcher = /"\\//; import.meta.require("/tmp/node_modules/regex.js")',
      path: "/tmp/node_modules/regex.js",
    },
  ]
  for (const { source, path } of cases) {
    expect(await findExternalDependencySpecifiers(source)).toContain(path)
  }
})

test("the dependency guard rejects nonliteral module specifiers", async () => {
  const source = 'require(path); import.meta.require(path); import(path)'
  expect(await findExternalDependencySpecifiers(source)).toEqual([
    "<non-literal require()>",
    "<non-literal import.meta.require()>",
    "<non-literal import()>",
  ])
})

test("the dependency guard permits runtime builtin module specifiers", async () => {
  const source = 'require("fs"); import("fs/promises"); import("node:fs"); import("bun:ffi"); require("bun")'
  expect(await findExternalDependencySpecifiers(source)).toEqual([])
})

test("the dependency guard rejects nonbuiltin literal module specifiers", async () => {
  const specifiers = [
    { source: 'import.meta.require("node:never-a-runtime-builtin")', path: "node:never-a-runtime-builtin" },
    {
      source: 'import.meta.require("workspace:@legion/contracts")',
      path: "workspace:@legion/contracts",
    },
    { source: 'require("zod")', path: "zod" },
    { source: 'import("./local.js")', path: "./local.js" },
    { source: 'export { value } from "/tmp/node_modules/entry.js"', path: "/tmp/node_modules/entry.js" },
    { source: 'import("https://example.test/module.js")', path: "https://example.test/module.js" },
  ]
  const matches = await findExternalDependencySpecifiers(specifiers.map(({ source }) => source).join("\n"))
  for (const { path } of specifiers) expect(matches).toContain(path)
})

test("the committed bundles contain only runtime builtin module specifiers", async () => {
  for (const name of Object.keys(BUNDLE_ENTRYPOINTS)) {
    const bundle = await readFile(join(distDirectory, `${name}.js`), "utf8")
    expect(await findExternalDependencySpecifiers(bundle)).toEqual([])
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
