// The license notices of the third-party packages a bundle inlines. A bundle is a copy of every
// package it inlines, and most of their licenses (MIT, BSD, ISC, Apache-2.0) require shipping their
// notices with every copy, so a build that writes a bundle writes this beside it.
import { readdir, readFile, realpath } from "node:fs/promises";
import { dirname, join, resolve, sep } from "node:path";

const LICENSE_FILE = /^(licen[cs]e|copying|notice)([.-].*)?$/i;
const NODE_MODULES = `${sep}node_modules${sep}`;

interface BundledPackage {
  name: string;
  version: string;
  license: string | null;
  files: { name: string; text: string }[];
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

async function readPackage(root: string): Promise<BundledPackage> {
  const manifest = JSON.parse(await readFile(join(root, "package.json"), "utf8")) as {
    name?: string;
    version?: string;
    license?: string | { type?: string };
  };
  if (!manifest.name || !manifest.version) {
    throw new Error(`${root}/package.json names no package name and version`);
  }
  const license =
    typeof manifest.license === "string" ? manifest.license : (manifest.license?.type ?? null);
  const names = (await readdir(root)).filter((name) => LICENSE_FILE.test(name)).sort();
  const files = await Promise.all(
    names.map(async (name) => ({ name, text: (await readFile(join(root, name), "utf8")).trim() }))
  );
  if (license === null && files.length === 0) {
    throw new Error(
      `${manifest.name}@${manifest.version} declares no license and ships no license file; ` +
        "find its terms before bundling it"
    );
  }
  return { name: manifest.name, version: manifest.version, license, files };
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
    if (bundled.files.length === 0) {
      lines.push(
        "",
        "The package ships no license file; its package.json declares the license above."
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
