// Oh My Pi's native modules for the tests that run the real binary under a fresh HOME
// (test-omp-harness.ts's ompRoot). Oh My Pi extracts them from its own binary into
// .omp/natives/<version> under any HOME that lacks them, about 354 MiB at the pin, and has no
// setting for that directory short of XDG_DATA_HOME, which moves its whole profile. Each HOME's
// natives are instead hardlinks to one copy per binary under the user's cache directory.
//
// The copy is the one the Go tests keep (packages/daemon-go/internal/testbin/natives.go,
// testbin.OMPHome): the same directory, name, layout, lock and fill, so a machine holds one copy per
// pin for both languages and either fills it for the other. A change to one is a change to both.
import {
  chmod,
  link,
  mkdir,
  mkdtemp,
  readdir,
  rename,
  rm,
  stat,
  writeFile,
} from "node:fs/promises";
import * as path from "node:path";

/** Each binary's one lookup in this process, since naming its cache reads the whole binary. */
const caches = new Map<string, Promise<string>>();

/**
 * Hardlinks `binary`'s natives into `home`'s .omp/natives from the shared copy, filling the copy
 * first if no process has. The test's environment must name no XDG_DATA_HOME, as none does here:
 * Oh My Pi keeps its natives under $XDG_DATA_HOME/omp when that directory exists.
 */
export async function linkOmpNatives(binary: string, home: string): Promise<void> {
  let cache = caches.get(binary);
  if (cache === undefined) {
    cache = fillNativesCache(nativesCacheRoot(), binary);
    caches.set(binary, cache);
  }
  await linkTree(await cache, path.join(home, ".omp", "natives"));
}

/** Where the Go tests' os.UserCacheDir puts the cache on Linux, with its refusals. */
function nativesCacheRoot(): string {
  const xdg = process.env.XDG_CACHE_HOME;
  if (xdg) {
    if (!path.isAbsolute(xdg)) throw new Error(`XDG_CACHE_HOME ${xdg} is relative`);
    return path.join(xdg, "legion", "test-omp-natives");
  }
  const home = process.env.HOME;
  if (!home)
    throw new Error("neither XDG_CACHE_HOME nor HOME is set, so the natives have no cache");
  return path.join(home, ".cache", "legion", "test-omp-natives");
}

/**
 * Returns the directory under `root` holding the natives `binary` extracts, named for the binary's
 * content. The first process to ask fills it under an exclusive lock on its own lock file, so of
 * the processes that ask at once, Go test binaries included, one runs the binary and the rest wait
 * and find it filled. The binary extracts into a staging HOME, and its natives directory is renamed
 * into place whole once it holds every file the binary embeds at its embedded size, so a directory
 * that exists is complete. A staging directory the lock's holder finds is a dead filler's, which it
 * removes.
 *
 * Oh My Pi writes the embedded files one after another, and when a later write fails (a full disk)
 * it loads one it did write and still exits 0; a cache missing the other would have every later
 * HOME extract it. The check against the embedded list is what refuses that fill.
 */
export async function fillNativesCache(root: string, binary: string): Promise<string> {
  const { digest, embedded } = await readBinary(binary);
  await mkdir(root, { recursive: true, mode: 0o700 });
  const cache = path.join(root, digest);
  const lockFile = `${cache}.lock`;
  await writeFile(lockFile, "", { flag: "a", mode: 0o600 });
  const release = await holdLock(lockFile);
  try {
    if (await exists(cache)) return cache;
    for (const name of await readdir(root)) {
      if (name.startsWith(`${digest}.fill-`)) await rm(path.join(root, name), { recursive: true });
    }
    const staging = await mkdtemp(path.join(root, `${digest}.fill-`));
    try {
      const home = path.join(staging, "home");
      // Any command that loads the natives extracts them; this one reads a setting and calls nothing.
      const extract = Bun.spawn([binary, "config", "get", "compaction.remoteEndpoint", "--json"], {
        cwd: staging,
        env: { HOME: home, PATH: "/usr/bin:/bin" },
        stdout: "pipe",
        stderr: "pipe",
      });
      const [out, err, code] = await Promise.all([
        new Response(extract.stdout).text(),
        new Response(extract.stderr).text(),
        extract.exited,
      ]);
      if (code !== 0) {
        throw new Error(
          `${binary} config get under the fresh HOME ${home} exited ${code}\n${out}${err}`
        );
      }
      const natives = path.join(home, ".omp", "natives");
      const extracted = new Map<string, number>();
      for (const relative of await readdir(natives, { recursive: true })) {
        const file = path.join(natives, relative);
        const info = await stat(file);
        if (!info.isFile()) continue;
        extracted.set(path.basename(file), info.size);
        await chmod(file, 0o500);
      }
      for (const name of [...embedded.keys()].sort()) {
        const size = extracted.get(name);
        if (size === undefined) {
          throw new Error(
            `${binary} embeds ${name}, but its extraction under ${natives} lacks it (a write failed, as on a full disk), so nothing is cached`
          );
        }
        if (size !== embedded.get(name)) {
          throw new Error(
            `${binary} embeds ${name} at ${embedded.get(name)} bytes, but extracted it at ${size} under ${natives}, so nothing is cached`
          );
        }
      }
      await rename(natives, cache);
      return cache;
    } finally {
      await rm(staging, { recursive: true, force: true });
    }
  } finally {
    await release();
  }
}

/**
 * One native file the binary embeds, as pi-natives' scripts/embed-native.ts writes each into the
 * compiled binary: `{ variant: "modern", filename: "pi_natives.linux-x64-modern.node", size:
 * 185671184 },`. The scan finds each by its marker, since a regular expression over the whole
 * binary takes seconds.
 */
const embeddedMarker = Buffer.from('filename: "pi_natives.');
const embeddedNative = /^filename: "(pi_natives\.[^"/]+)", size: ([0-9]+)/;

/**
 * The digest that names the binary's cache (the first 16 bytes of its sha256, in hex, as the Go
 * tests name it) and the native files it embeds, each with its size: what a complete extraction
 * holds. A binary that lists none is refused, since then no extraction could be told from a partial
 * one. The binary is read, not mapped: while a Bun.mmap of it is live, spawning it fails with
 * ETXTBSY.
 */
async function readBinary(
  binary: string
): Promise<{ digest: string; embedded: Map<string, number> }> {
  const buffer = Buffer.from(await Bun.file(binary).arrayBuffer());
  const embedded = new Map<string, number>();
  for (
    let at = buffer.indexOf(embeddedMarker);
    at !== -1;
    at = buffer.indexOf(embeddedMarker, at + 1)
  ) {
    const match = embeddedNative.exec(
      buffer.toString("latin1", at, Math.min(buffer.length, at + 256))
    );
    if (match === null) continue;
    const [, name = "", listed = ""] = match;
    const size = Number(listed);
    const earlier = embedded.get(name);
    if (earlier !== undefined && earlier !== size) {
      throw new Error(`${binary} lists ${name} at ${earlier} bytes and at ${size}`);
    }
    embedded.set(name, size);
  }
  if (embedded.size === 0) {
    throw new Error(
      `${binary} lists no embedded natives (${embeddedNative.source}), so a fill could not tell a complete extraction from a partial one`
    );
  }
  const digest = new Bun.CryptoHasher("sha256").update(buffer).digest("hex").slice(0, 32);
  return { digest, embedded };
}

/**
 * Holds the exclusive flock(2) lock the Go fill takes on `lockFile`, through util-linux `flock`:
 * the shell prints a line once flock holds the lock and blocks on its stdin, so the lock lasts
 * until the returned release closes that stdin, or until this process dies and the pipe closes.
 */
async function holdLock(lockFile: string): Promise<() => Promise<void>> {
  const holder = Bun.spawn(["flock", "--exclusive", lockFile, "sh", "-c", "echo && read _"], {
    stdin: "pipe",
    stdout: "pipe",
    stderr: "inherit",
  });
  const { done } = await holder.stdout.getReader().read();
  if (done)
    throw new Error(`flock --exclusive ${lockFile} exited ${await holder.exited} without the lock`);
  return async () => {
    holder.stdin.end();
    await holder.exited;
  };
}

async function exists(file: string): Promise<boolean> {
  try {
    await stat(file);
    return true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return false;
    throw error;
  }
}

/** Makes each directory under `source` at `destination` and hardlinks each file into it. */
export async function linkTree(source: string, destination: string): Promise<void> {
  await mkdir(destination, { recursive: true, mode: 0o755 });
  for (const relative of await readdir(source, { recursive: true })) {
    const from = path.join(source, relative);
    const to = path.join(destination, relative);
    if ((await stat(from)).isDirectory()) {
      await mkdir(to, { recursive: true, mode: 0o755 });
      continue;
    }
    await mkdir(path.dirname(to), { recursive: true, mode: 0o755 });
    try {
      await link(from, to);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EXDEV") throw error;
      throw new Error(
        `link ${from} ${to}: the cache and the test's temporary directory are on different filesystems; point XDG_CACHE_HOME or TMPDIR so they share one`
      );
    }
  }
}
