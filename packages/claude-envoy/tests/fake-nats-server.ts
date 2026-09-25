// A NATS server stand-in over TCP for tests that drive the real `nats` client:
// INFO on connect, PONG for PING, SUB/UNSUB/PUB recorded, and PUB fanned out
// to every matching subscription (the publishing client's own included, as the
// real server does). Subjects match exactly or by a `.>` suffix.

import type { Socket, TCPSocketListener } from "bun"

interface Client {
  buffer: Buffer
  /** A PUB whose payload has not fully arrived yet. */
  pending: { readonly subject: string; readonly size: number } | null
  /** sid → subject */
  readonly subscriptions: Map<string, string>
}

export interface PublishedFrame {
  readonly subject: string
  readonly payload: string
}

interface Waiter {
  readonly predicate: () => boolean
  readonly resolve: () => void
}

const SUB = /^SUB (\S+) (?:\S+ )?(\S+)$/
const UNSUB = /^UNSUB (\S+)(?: \d+)?$/
const PUB = /^PUB (\S+) (?:\S+ )?(\d+)$/

function matches(pattern: string, subject: string): boolean {
  if (pattern.endsWith(".>")) return subject.startsWith(pattern.slice(0, -1))
  return pattern === subject
}

export class FakeNatsServer {
  /** SUB subjects in arrival order, across every client connection. */
  readonly subscribed: string[] = []
  /** Subjects whose SUB was later UNSUBed, in arrival order. */
  readonly unsubscribed: string[] = []
  /** PUB frames from clients, in arrival order. */
  readonly published: PublishedFrame[] = []
  /** Client connections accepted so far. */
  connections = 0
  private readonly clients = new Set<Socket<Client>>()
  private waiters: Waiter[] = []
  private readonly listener: TCPSocketListener<Client>
  /** Connections accepted while `holdNext()` is in force, with their INFO frames withheld. */
  private held: Socket<Client>[] | null = null

  constructor() {
    this.listener = Bun.listen<Client>({
      hostname: "127.0.0.1",
      port: 0,
      socket: {
        open: (socket) => {
          socket.data = { buffer: Buffer.alloc(0), pending: null, subscriptions: new Map() }
          this.clients.add(socket)
          this.connections += 1
          if (this.held !== null) this.held.push(socket)
          else this.greet(socket)
          this.notify()
        },
        data: (socket, chunk) => this.receive(socket, chunk),
        close: (socket) => {
          this.clients.delete(socket)
          this.notify()
        },
        error: () => {},
      },
    })
  }

  /** Client sockets currently open. */
  get liveConnections(): number {
    return this.clients.size
  }

  get url(): string {
    return `nats://127.0.0.1:${this.listener.port}`
  }

  /** Send a message to every live subscription matching `subject`. */
  deliver(subject: string, payload: string): void {
    for (const socket of this.clients) {
      for (const [sid, pattern] of socket.data.subscriptions) {
        if (!matches(pattern, subject)) continue
        socket.write(`MSG ${subject} ${sid} ${Buffer.byteLength(payload)}\r\n${payload}\r\n`)
      }
    }
  }

  /** Resolves once `predicate` holds; re-checked after every frame and connection change. */
  until(predicate: () => boolean): Promise<void> {
    if (predicate()) return Promise.resolve()
    const { promise, resolve } = Promise.withResolvers<void>()
    this.waiters.push({ predicate, resolve })
    return promise
  }

  /** Accept the next connections but withhold INFO, so their client-side connect stays pending until `release()`. */
  holdNext(): void {
    this.held = []
  }

  /** Send INFO to every held connection, letting their handshakes complete. */
  release(): void {
    const held = this.held ?? []
    this.held = null
    for (const socket of held) this.greet(socket)
  }

  private greet(socket: Socket<Client>): void {
    const info = {
      server_id: "fake",
      server_name: "fake",
      version: "2.10.22",
      go: "go1.22",
      host: "127.0.0.1",
      port: this.listener.port,
      headers: true,
      max_payload: 1_048_576,
      proto: 1,
      client_id: this.connections,
      client_ip: "127.0.0.1",
    }
    socket.write(`INFO ${JSON.stringify(info)}\r\n`)
  }

  /**
   * Make every client give up on this server for good: nats.js stops
   * reconnecting after a repeated authorization violation. Resolves once every
   * client socket is gone.
   */
  closeClients(): Promise<void> {
    for (const socket of this.clients) {
      socket.write("-ERR 'Authorization Violation'\r\n-ERR 'Authorization Violation'\r\n")
    }
    return this.until(() => this.clients.size === 0)
  }

  /** Close every client for good, then stop listening, so no reconnect loop outlives the test. */
  async stop(): Promise<void> {
    await this.closeClients()
    this.listener.stop(true)
  }

  /** The broker goes away mid-flight: every socket is dropped without a word and the port refuses from now on. */
  vanish(): void {
    this.listener.stop(true)
  }

  private receive(socket: Socket<Client>, chunk: Buffer): void {
    const client = socket.data
    client.buffer = Buffer.concat([client.buffer, chunk])
    for (;;) {
      if (client.pending) {
        const { subject, size } = client.pending
        if (client.buffer.length < size + 2) return
        const payload = client.buffer.subarray(0, size).toString()
        client.buffer = client.buffer.subarray(size + 2)
        client.pending = null
        this.published.push({ subject, payload })
        this.deliver(subject, payload)
        this.notify()
        continue
      }
      const end = client.buffer.indexOf("\r\n")
      if (end === -1) return
      const line = client.buffer.subarray(0, end).toString()
      client.buffer = client.buffer.subarray(end + 2)
      this.command(socket, line)
    }
  }

  private command(socket: Socket<Client>, line: string): void {
    if (line === "PING") {
      socket.write("PONG\r\n")
      return
    }
    if (line === "PONG" || line.startsWith("CONNECT ")) return
    const sub = SUB.exec(line)
    if (sub?.[1] !== undefined && sub[2] !== undefined) {
      socket.data.subscriptions.set(sub[2], sub[1])
      this.subscribed.push(sub[1])
      this.notify()
      return
    }
    const unsub = UNSUB.exec(line)
    if (unsub?.[1] !== undefined) {
      const subject = socket.data.subscriptions.get(unsub[1])
      if (subject !== undefined) {
        socket.data.subscriptions.delete(unsub[1])
        this.unsubscribed.push(subject)
      }
      this.notify()
      return
    }
    const pub = PUB.exec(line)
    if (pub?.[1] !== undefined && pub[2] !== undefined) {
      socket.data.pending = { subject: pub[1], size: Number(pub[2]) }
      return
    }
    throw new Error(`fake nats: unsupported command: ${line}`)
  }

  private notify(): void {
    const remaining: Waiter[] = []
    for (const waiter of this.waiters) {
      if (waiter.predicate()) waiter.resolve()
      else remaining.push(waiter)
    }
    this.waiters = remaining
  }
}
