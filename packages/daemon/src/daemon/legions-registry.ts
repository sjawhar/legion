import {
	closeSync,
	constants,
	openSync,
	readFileSync,
	writeSync,
} from "node:fs";
import {
	copyFile,
	mkdir,
	readFile,
	rename,
	unlink,
	writeFile,
} from "node:fs/promises";
import path from "node:path";
import { LegionsRegistrySchema } from "./schemas";

export interface LegionEntry {
	port: number;
	pid: number;
	startedAt: string;
}

type LegionsRegistry = Record<string, LegionEntry>;

export function isPidAlive(pid: number): boolean {
	try {
		process.kill(pid, 0);
	} catch {
		return false;
	}
	if (process.platform === "linux") {
		try {
			const cmdline = readFileSync(`/proc/${pid}/cmdline`, "utf-8");
			return cmdline.includes("legion") || cmdline.includes("bun");
		} catch {
			// Permission and process-race failures do not establish that the process is dead.
		}
	}
	return true;
}

export async function readLegionsRegistry(
	filePath: string,
): Promise<LegionsRegistry> {
	try {
		const raw = await readFile(filePath, "utf-8");
		if (!raw.trim()) return {};
		const result = LegionsRegistrySchema.safeParse(JSON.parse(raw));
		if (result.success) return result.data;

		console.warn(
			`[legions-registry] Schema validation failed for ${filePath}, backing up and resetting. Error: ${result.error.message}`,
		);
		await copyFile(filePath, `${filePath}.bak.${Date.now()}`).catch(() => {});
		return {};
	} catch (error) {
		const errno = error as NodeJS.ErrnoException;
		if (errno.code === "ENOENT") return {};
		throw error;
	}
}

export async function writeLegionEntry(
	filePath: string,
	projectId: string,
	entry: LegionEntry,
): Promise<void> {
	await withRegistryLock(filePath, async () => {
		const registry = await readLegionsRegistry(filePath);
		registry[projectId] = entry;
		await writeRegistry(filePath, registry);
	});
}

export async function removeLegionEntry(
	filePath: string,
	projectId: string,
): Promise<void> {
	await withRegistryLock(filePath, async () => {
		const registry = await readLegionsRegistry(filePath);
		delete registry[projectId];
		await writeRegistry(filePath, registry);
	});
}

export async function findLegionByProjectId(
	filePath: string,
	projectId: string,
): Promise<LegionEntry | undefined> {
	return (await readLegionsRegistry(filePath))[projectId];
}

async function writeRegistry(
	filePath: string,
	registry: LegionsRegistry,
): Promise<void> {
	await mkdir(path.dirname(filePath), { recursive: true });
	const temporary = `${filePath}.tmp-${process.pid}-${Date.now()}`;
	await writeFile(temporary, JSON.stringify(registry, null, 2), "utf-8");
	await rename(temporary, filePath);
}

async function withRegistryLock<T>(
	filePath: string,
	operation: () => Promise<T>,
): Promise<T> {
	const lockPath = `${filePath}.lock`;
	const startedAt = Date.now();
	let delayMs = 50;
	while (Date.now() - startedAt < 3_000) {
		try {
			const descriptor = openSync(
				lockPath,
				constants.O_CREAT | constants.O_EXCL | constants.O_WRONLY,
			);
			writeSync(
				descriptor,
				JSON.stringify({ pid: process.pid, timestamp: Date.now() }),
			);
			closeSync(descriptor);
			try {
				return await operation();
			} finally {
				await unlink(lockPath).catch(() => {});
			}
		} catch (error) {
			const errno = error as NodeJS.ErrnoException;
			if (errno.code !== "EEXIST") throw error;
		}

		try {
			const lock = JSON.parse(await readFile(lockPath, "utf-8")) as {
				pid?: unknown;
			};
			if (typeof lock.pid === "number" && !isPidAlive(lock.pid)) {
				await unlink(lockPath).catch(() => {});
				continue;
			}
		} catch {
			continue;
		}
		await new Promise((resolve) => setTimeout(resolve, delayMs));
		delayMs = Math.min(delayMs * 2, 400);
	}
	throw new Error(`Timed out acquiring registry lock: ${lockPath}`);
}
