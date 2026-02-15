import { createServer } from "node:net";

export class PortAllocator {
  private readonly basePort: number;
  private readonly maxPorts: number;
  private readonly allocated = new Set<number>();

  constructor(basePort = 13381, maxPorts = 100) {
    this.basePort = basePort;
    this.maxPorts = maxPorts;
  }

  allocate(): number {
    for (let offset = 0; offset < this.maxPorts; offset += 1) {
      const port = this.basePort + offset;
      if (!this.allocated.has(port)) {
        this.allocated.add(port);
        return port;
      }
    }

    throw new Error("No available ports in allocator range");
  }

  release(port: number): void {
    this.allocated.delete(port);
  }

  isAllocated(port: number): boolean {
    return this.allocated.has(port);
  }
}

export function isPortFree(port: number): Promise<boolean> {
  return new Promise((resolve) => {
    const server = createServer();
    server.once("error", () => resolve(false));
    server.listen(port, "127.0.0.1", () => {
      server.close(() => resolve(true));
    });
  });
}
