import { expect, test } from "bun:test";
import { existsSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "./temporary-directories";
import {
  versionControlEnvironment,
  versionControlIdentity,
  versionControlNames,
} from "./version-control-environment";

const root = temporaryDirectory("test-fixtures-version-control");

test("names the home it was given, a config home under it and a jj configuration file in it", () => {
  const home = join(root, "named");
  const environment = versionControlEnvironment(home);
  expect(environment["HOME"]).toBe(home);
  expect(environment["XDG_CONFIG_HOME"]).toBe(join(home, ".config"));
  expect(environment["JJ_CONFIG"]).toBe(join(home, "jj-config.toml"));
  expect(statSync(join(home, ".config")).isDirectory()).toBe(true);
  expect(readFileSync(join(home, "jj-config.toml"), "utf8")).toStartWith("#");
});

test("leaves git no configuration to read and no prompt to raise, and names the identity", () => {
  const environment = versionControlEnvironment(join(root, "git"));
  expect(environment["GIT_CONFIG_GLOBAL"]).toBe("/dev/null");
  expect(environment["GIT_CONFIG_NOSYSTEM"]).toBe("1");
  expect(environment["GIT_TERMINAL_PROMPT"]).toBe("0");
  expect(environment["JJ_USER"]).toBe(versionControlIdentity.JJ_USER);
  expect(environment["JJ_EMAIL"]).toBe(versionControlIdentity.JJ_EMAIL);
});

test("sets exactly the names it publishes, and nothing on this process", () => {
  const home = join(root, "untouched");
  const before = versionControlNames.map((name) => process.env[name]);
  const environment = versionControlEnvironment(home);
  expect(Object.keys(environment).sort()).toEqual([...versionControlNames].sort());
  expect(versionControlNames.map((name) => process.env[name])).toEqual(before);
});

test("makes a home under a parent that does not exist yet", () => {
  const home = join(root, "deep", "under", "here");
  expect(existsSync(home)).toBe(false);
  versionControlEnvironment(home);
  expect(statSync(home).isDirectory()).toBe(true);
});
