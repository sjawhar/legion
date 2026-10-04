// The license notices of the third-party packages a bundle inlines. A bundle is a copy of every
// package it inlines, and most of their licenses (MIT, BSD, ISC, Apache-2.0) require shipping their
// notices with every copy, so a build that writes a bundle writes this beside it.
import { existsSync, realpathSync } from "node:fs";
import { readdir, readFile, realpath, stat, writeFile } from "node:fs/promises";
import { createRequire } from "node:module";
import { dirname, isAbsolute, join, resolve, sep } from "node:path";
import parseSpdxExpression from "spdx-expression-parse";
import spdxLicenses from "spdx-license-list/full";
import { z } from "zod";

/** A file that holds a package's license terms. A NOTICE alone does not. */
const LICENSE_FILE = /^(licen[cs]e|copying)([.-].*)?$/i;
/** A NOTICE file, which Apache-2.0 requires passing on beside the license. */
const NOTICE_FILE = /^notice([.-].*)?$/i;
const NODE_MODULES = `${sep}node_modules${sep}`;

interface BundledPackage {
  name: string;
  version: string;
  license: string | null;
  source: string | null;
  files: { name: string; text: string }[];
  /** The SPDX License List's standard text of each declared license, for a package that ships none. */
  standardTexts: { id: string; text: string }[];
}

/** The installed package root a module path sits in, or null for a path outside node_modules. */
function installedRootOf(modulePath: string): string | null {
  const at = modulePath.lastIndexOf(NODE_MODULES);
  if (at < 0) return null;
  const rest = modulePath.slice(at + NODE_MODULES.length).split(sep);
  const depth = rest[0]?.startsWith("@") ? 2 : 1;
  return modulePath.slice(0, at + NODE_MODULES.length) + rest.slice(0, depth).join(sep);
}

async function readManifest(directory: string): Promise<Record<string, unknown> | null> {
  try {
    return JSON.parse(await readFile(join(directory, "package.json"), "utf8"));
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return null;
    throw error;
  }
}

/**
 * The workspace package a module of this repository sits in, when that package carries a license
 * of its own (a copy of someone else's code, such as packages/proof-editor), else null. The walk
 * stops at the workspace root, whose license is the repository's.
 */
async function licensedWorkspaceRootOf(modulePath: string): Promise<string | null> {
  let directory = dirname(modulePath);
  while (directory !== dirname(directory)) {
    const manifest = await readManifest(directory);
    if (manifest !== null) {
      if ("workspaces" in manifest) return null;
      const names = await readdir(directory);
      return names.some((name) => LICENSE_FILE.test(name)) ? directory : null;
    }
    directory = dirname(directory);
  }
  return null;
}

const PackageIdentity = z.object({ name: z.string().min(1), version: z.string().min(1) });
/** npm's deprecated license object, also the entry of its older `licenses` array. */
const LicenseObject = z.object({ type: z.string() });
/**
 * The license fields of a manifest, in every shape npm defines: `license` as a string or the
 * deprecated `{ type, url }` object, or the older `licenses` array of those objects (format@0.2.2
 * still uses it), whose entries offer a choice of licenses. Any other shape is refused.
 * `repository` and `homepage` only point at the source, so a shape this does not read drops them.
 */
const PackageLicense = z.object({
  license: z.union([z.string(), LicenseObject]).optional(),
  licenses: z.array(LicenseObject).min(1).optional(),
  repository: z
    .union([z.string(), z.object({ url: z.string() })])
    .optional()
    .catch(undefined),
  homepage: z.string().optional().catch(undefined),
});

/** Every license an SPDX expression names. */
function licenseLeaves(expression: parseSpdxExpression.Info): parseSpdxExpression.LicenseInfo[] {
  if ("license" in expression) return [expression];
  return [...licenseLeaves(expression.left), ...licenseLeaves(expression.right)];
}

async function readPackage(root: string): Promise<BundledPackage> {
  const manifest: unknown = JSON.parse(await readFile(join(root, "package.json"), "utf8"));
  const identity = PackageIdentity.safeParse(manifest);
  if (!identity.success) {
    throw new Error(`${root}/package.json names no package name and version`);
  }
  const { name, version } = identity.data;
  const label = `${name}@${version}`;
  const fields = PackageLicense.safeParse(manifest);
  if (!fields.success) {
    throw new Error(
      `${label} declares its license in a shape npm does not define ` +
        `(${fields.error.issues.map((issue) => issue.path.join(".")).join(", ")}); ` +
        "find its terms before bundling it"
    );
  }
  const { license: declared, licenses, repository, homepage } = fields.data;
  const license =
    typeof declared === "string"
      ? declared
      : (declared?.type ?? licenses?.map((entry) => entry.type).join(" OR ") ?? null);
  const entries = (await readdir(root, { withFileTypes: true }))
    .filter((entry) => entry.isFile())
    .map((entry) => entry.name)
    .sort();
  const licenseNames = entries.filter((entry) => LICENSE_FILE.test(entry));
  const names = entries.filter((entry) => LICENSE_FILE.test(entry) || NOTICE_FILE.test(entry));
  const files = await Promise.all(
    names.map(async (file) => ({
      name: file,
      text: (await readFile(join(root, file), "utf8")).trim(),
    }))
  );
  // The terms must ship either way: in the package's own license file, or, for licenses on the
  // SPDX list, as that list's standard text. A license the list does not hold (LicenseRef-,
  // DocumentRef-), an exception, or no declaration at all leaves the license file as the only
  // source of its terms.
  const standardTexts: { id: string; text: string }[] = [];
  if (license === null) {
    if (licenseNames.length === 0) {
      throw new Error(
        `${label} declares no license and ships no license file; find its terms before bundling it`
      );
    }
  } else {
    let expression: parseSpdxExpression.Info;
    try {
      expression = parseSpdxExpression(license);
    } catch {
      throw new Error(
        `${label} declares an unrecognized license ${license}; find its terms before bundling it`
      );
    }
    if (licenseNames.length === 0) {
      for (const leaf of licenseLeaves(expression)) {
        const text = spdxLicenses[leaf.license]?.licenseText;
        if ("exception" in leaf || text === undefined) {
          throw new Error(
            `${label} declares ${license}, whose terms are not on the SPDX License List, and ` +
              "ships no license file; find its terms before bundling it"
          );
        }
        standardTexts.push({ id: leaf.license, text: text.trim() });
      }
    }
  }
  // Where a recipient gets the package's source, which a copyleft license such as EPL-2.0 (elkjs, in
  // the Dispatch web bundle) requires the notices to say.
  const source =
    (typeof repository === "string" ? repository : repository?.url) ?? homepage ?? null;
  return { name, version, license, source, files, standardTexts };
}

/**
 * The notices text for a bundle built from `inputs`: the module file paths a bundler lists (a Bun
 * metafile's `Object.keys(result.metafile.inputs)`), absolute or relative to `base`. A module
 * installed under node_modules counts as its package's; a module of this repository counts only
 * when its workspace package carries a license file of its own. Packages are sorted by name and
 * version, so the same inputs always give the same text.
 */
export async function thirdPartyNotices(inputs: Iterable<string>, base: string): Promise<string> {
  const roots = new Set<string>();
  for (const input of inputs) {
    const modulePath = await realpath(resolve(base, input));
    const root = installedRootOf(modulePath) ?? (await licensedWorkspaceRootOf(modulePath));
    if (root !== null) roots.add(root);
  }
  const packages = new Map<string, BundledPackage>();
  for (const root of roots) {
    const bundled = await readPackage(root);
    packages.set(`${bundled.name}@${bundled.version}`, bundled);
  }
  const sorted = [...packages].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
  const sections = sorted.map(([key, bundled]) => {
    const lines = [key, `License: ${bundled.license ?? "(not declared in package.json)"}`];
    if (bundled.source !== null) lines.push(`Source: ${bundled.source}`);
    for (const standard of bundled.standardTexts) {
      lines.push(
        "",
        `--- ${standard.id}: the SPDX License List's standard text (the package ships no license file) ---`,
        "",
        standard.text
      );
    }
    for (const file of bundled.files) {
      lines.push("", `--- ${file.name} ---`, "", file.text);
    }
    return lines.join("\n");
  });
  const rule = `\n\n${"=".repeat(80)}\n\n`;
  return (
    "Third-party software inlined into the bundles in this directory, with each package's license.\n" +
    rule.trimStart() +
    sections.join(rule) +
    "\n"
  );
}

/**
 * The directory of the package `name` as Node finds it from `from`: the first ancestor's
 * `node_modules/<name>`, read regardless of the package's `exports`, since only its root is wanted.
 */
export function packageRoot(name: string, from: string): string {
  for (let directory = resolve(from); ; directory = dirname(directory)) {
    const candidate = join(directory, "node_modules", name);
    if (existsSync(join(candidate, "package.json"))) return realpathSync(candidate);
    if (directory === dirname(directory)) {
      throw new Error(`no package ${name} is installed above ${from}`);
    }
  }
}

/**
 * Code a Vite build emits from no source file it records: a bundler's runtime helper, or a script
 * or stylesheet a framework generates. `id` matches the module id, or the asset's file name;
 * `packages` are the directories of the packages whose code it is (`packageRoot`), and none for a
 * wrapper around a module whose own file the bundle records.
 */
export interface GeneratedCode {
  id: RegExp;
  packages: string[];
}

/** What `viteBundleInputs` reads from a Vite build: its plugin context and its output bundle. */
interface ViteBuild {
  getModuleIds(): Iterable<string>;
  /** Absent from Rolldown, Vite 8's bundler. */
  getWatchFiles?(): string[];
}
type ViteOutput =
  | { type: "chunk"; modules: Record<string, unknown> }
  | { type: "asset"; fileName: string; originalFileNames: readonly string[] };

/** Tailwind's `@plugin` and `@config`: JavaScript it runs while compiling a stylesheet, whose
 *  output lands in the CSS although the package never enters the module graph. */
const TAILWIND_JS_DIRECTIVE = /@(?:plugin|config)\s+["']([^"']+)["']/g;

/**
 * The files a Vite build copies into its output, as `thirdPartyNotices` inputs: the modules the
 * chunks render (tree-shaken modules are left out), the source file of every emitted asset, the
 * files a plugin read that are no module at all (the stylesheets a CSS `@import` pulls in), the
 * packages a stylesheet names to Tailwind as plugins, and the package `generated` names for each
 * module or asset that has no source file. A module id or asset of that kind which `generated`
 * does not match fails the build, naming it. A `?query` suffix names the file before it, and a
 * relative id is relative to `root`, Vite's root.
 */
export async function viteBundleInputs(
  build: ViteBuild,
  bundle: Record<string, ViteOutput>,
  root: string,
  generated: GeneratedCode[]
): Promise<Set<string>> {
  const file = (id: string) => resolve(root, id.replace(/\?.*$/, ""));
  const inputs = new Set<string>();
  const unaccounted = new Set<string>();
  const generatedBy = (id: string) => {
    const rule = generated.find((candidate) => candidate.id.test(id));
    if (rule === undefined) unaccounted.add(id);
    else for (const directory of rule.packages) inputs.add(join(directory, "package.json"));
  };
  for (const output of Object.values(bundle)) {
    if (output.type === "chunk") {
      for (const id of Object.keys(output.modules)) {
        if (isAbsolute(id)) inputs.add(file(id));
        else generatedBy(id);
      }
    } else if (output.originalFileNames.length === 0) {
      generatedBy(output.fileName);
    } else {
      for (const id of output.originalFileNames) inputs.add(file(id));
    }
  }
  if (unaccounted.size > 0) {
    throw new Error(
      `the build emits ${JSON.stringify([...unaccounted])}, which no source file or known ` +
        "generator accounts for; name the package that generates each"
    );
  }
  const modules = new Set([...build.getModuleIds()].filter((id) => isAbsolute(id)).map(file));
  for (const id of build.getWatchFiles?.() ?? []) {
    // A watch entry may also be a directory or a glob Tailwind scans for class names.
    const path = file(id);
    if (!modules.has(path) && (await stat(path).catch(() => null))?.isFile()) inputs.add(path);
  }
  for (const input of [...inputs].filter((path) => path.endsWith(".css"))) {
    const resolveFrom = createRequire(input).resolve;
    for (const [, specifier] of (await readFile(input, "utf8")).matchAll(TAILWIND_JS_DIRECTIVE)) {
      // The pattern's one group matches whenever the pattern does.
      inputs.add(resolveFrom(specifier as string));
    }
  }
  return inputs;
}

// `bun scripts/third-party-notices.ts <metafile> <out>` writes the notices for a `bun build
// --metafile=<metafile>` run in the current directory, which the metafile's input paths are
// relative to.
if (import.meta.main) {
  const [metafile, out] = process.argv.slice(2);
  if (!metafile || !out) {
    process.stderr.write("usage: bun scripts/third-party-notices.ts <metafile> <out>\n");
    process.exit(2);
  }
  const { inputs } = JSON.parse(await readFile(metafile, "utf8")) as {
    inputs: Record<string, unknown>;
  };
  await writeFile(out, await thirdPartyNotices(Object.keys(inputs), process.cwd()));
}
