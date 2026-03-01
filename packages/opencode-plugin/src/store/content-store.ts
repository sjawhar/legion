import { Database } from "bun:sqlite";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const DEFAULT_MAX_SIZE_MB = 50;
const DEFAULT_SEARCH_LIMIT = 10;
const MAX_VOCAB_TERMS = 20;

const STOP_WORDS = new Set([
  "the",
  "and",
  "for",
  "with",
  "that",
  "this",
  "from",
  "into",
  "are",
  "was",
  "were",
  "you",
  "your",
  "have",
  "has",
  "had",
  "will",
  "would",
  "should",
  "can",
  "could",
  "not",
  "but",
  "use",
  "using",
  "run",
  "runs",
  "out",
  "all",
  "any",
  "also",
]);

interface Chunk {
  title: string;
  content: string;
}

interface PorterRow {
  content: string;
  source: string;
  title: string;
  score: number;
}

interface TrigramRow {
  content: string;
  source: string;
  title: string;
  score: number;
}

interface LikeRow {
  content: string;
  source: string;
  title: string;
}

export interface IndexResult {
  chunkCount: number;
  byteCount: number;
  vocabulary: string[];
  source: string;
}

export interface SearchResult {
  content: string;
  source: string;
  title: string;
  score: number;
}

export interface ContentStoreStats {
  totalChunks: number;
  totalBytes: number;
  sources: string[];
}

export class ContentStore {
  private readonly db: Database;
  private readonly dbPath: string;
  private readonly maxSizeBytes: number;
  private totalChunks = 0;
  private totalBytes = 0;
  private readonly sourceChunks = new Map<string, number>();
  private readonly sourceBytes = new Map<string, number>();
  private readonly sources = new Set<string>();

  constructor(options?: { maxSizeMB?: number; dbPath?: string }) {
    this.maxSizeBytes = Math.floor((options?.maxSizeMB ?? DEFAULT_MAX_SIZE_MB) * 1024 * 1024);
    this.dbPath = options?.dbPath ?? path.join(os.tmpdir(), `legion-context-${process.pid}.db`);
    this.db = new Database(this.dbPath, { create: true, strict: true });
    this.initialize();
  }

  index(input: { content: string; source: string; session?: string }): IndexResult {
    const content = input.content ?? "";
    const source = input.source;
    const session = input.session ?? "";

    if (!source) {
      throw new Error("source is required");
    }

    const byteCount = Buffer.byteLength(content, "utf8");
    const prevBytes = this.sourceBytes.get(source) ?? 0;
    const prevChunks = this.sourceChunks.get(source) ?? 0;
    if (this.totalBytes - prevBytes + byteCount > this.maxSizeBytes) {
      throw new Error("Content store size cap exceeded");
    }

    const chunks = this.chunkContent(content);

    const deletePorter = this.db.query("DELETE FROM porter_index WHERE source = ?");
    const deleteTrigram = this.db.query("DELETE FROM trigram_index WHERE source = ?");
    const insertPorter = this.db.query(
      "INSERT INTO porter_index (source, session, title, content) VALUES (?, ?, ?, ?)"
    );
    const insertTrigram = this.db.query(
      "INSERT INTO trigram_index (source, session, title, content) VALUES (?, ?, ?, ?)"
    );

    const insertTransaction = this.db.transaction((rows: Chunk[]) => {
      deletePorter.run(source);
      deleteTrigram.run(source);
      for (const row of rows) {
        insertPorter.run(source, session, row.title, row.content);
        insertTrigram.run(source, session, row.title, row.content);
      }
    });

    insertTransaction(chunks);

    this.totalChunks = this.totalChunks - prevChunks + chunks.length;
    this.totalBytes = this.totalBytes - prevBytes + byteCount;
    this.sourceChunks.set(source, chunks.length);
    this.sourceBytes.set(source, byteCount);
    this.sources.add(source);

    return {
      chunkCount: chunks.length,
      byteCount,
      vocabulary: this.extractVocabulary(chunks),
      source,
    };
  }

  search(input: {
    queries: string[];
    source?: string;
    session?: string;
    limit?: number;
  }): SearchResult[] {
    const limit = input.limit ?? DEFAULT_SEARCH_LIMIT;
    const filters = { source: input.source, session: input.session };
    const collected = new Map<string, SearchResult>();

    for (const queryText of input.queries) {
      const query = queryText.trim();
      if (!query) {
        continue;
      }

      let matches = this.searchPorter(query, filters, limit);
      if (matches.length === 0) {
        matches = this.searchTrigram(query, filters, limit);
      }
      if (matches.length === 0) {
        matches = this.searchFuzzy(query, filters, limit);
      }

      for (const match of matches) {
        const key = `${match.source}\0${match.title}\0${match.content}`;
        const existing = collected.get(key);
        if (!existing || match.score > existing.score) {
          collected.set(key, match);
        }
      }
    }

    return Array.from(collected.values())
      .sort((a, b) => b.score - a.score)
      .slice(0, limit);
  }

  getStats(): ContentStoreStats {
    return {
      totalChunks: this.totalChunks,
      totalBytes: this.totalBytes,
      sources: Array.from(this.sources).sort(),
    };
  }

  deleteSession(sessionID: string): void {
    if (!sessionID) {
      return;
    }

    const prefix = `${sessionID}:`;
    let removedChunks = 0;
    let removedBytes = 0;
    const removedSources: string[] = [];

    for (const [source, sourceBytes] of this.sourceBytes.entries()) {
      if (!source.startsWith(prefix)) {
        continue;
      }
      removedSources.push(source);
      removedBytes += sourceBytes;
      removedChunks += this.sourceChunks.get(source) ?? 0;
    }

    const deletePorter = this.db.query("DELETE FROM porter_index WHERE session = ?");
    const deleteTrigram = this.db.query("DELETE FROM trigram_index WHERE session = ?");
    const deleteTransaction = this.db.transaction((session: string) => {
      deletePorter.run(session);
      deleteTrigram.run(session);
    });

    deleteTransaction(sessionID);

    this.totalBytes = Math.max(0, this.totalBytes - removedBytes);
    this.totalChunks = Math.max(0, this.totalChunks - removedChunks);
    for (const source of removedSources) {
      this.sourceBytes.delete(source);
      this.sourceChunks.delete(source);
      this.sources.delete(source);
    }
  }

  close(): void {
    this.db.run("PRAGMA wal_checkpoint(TRUNCATE)");
    this.db.close();
    const dbFiles = [this.dbPath, `${this.dbPath}-wal`, `${this.dbPath}-shm`];
    for (const dbFile of dbFiles) {
      if (fs.existsSync(dbFile)) {
        fs.rmSync(dbFile, { force: true });
      }
    }
  }

  private initialize(): void {
    this.db.exec("PRAGMA journal_mode=WAL;");
    this.db.exec("PRAGMA synchronous=NORMAL;");
    this.db.exec(`PRAGMA max_page_count=${Math.max(256, Math.floor(this.maxSizeBytes / 4096))};`);
    this.db.exec(
      "CREATE VIRTUAL TABLE IF NOT EXISTS porter_index USING fts5(source UNINDEXED, session UNINDEXED, title, content, tokenize='porter unicode61');"
    );
    this.db.exec(
      "CREATE VIRTUAL TABLE IF NOT EXISTS trigram_index USING fts5(source UNINDEXED, session UNINDEXED, title, content, tokenize='trigram');"
    );
  }

  private deleteSource(source: string): void {
    const prevChunks = this.sourceChunks.get(source) ?? 0;
    const prevBytes = this.sourceBytes.get(source) ?? 0;
    this.totalChunks -= prevChunks;
    this.totalBytes -= prevBytes;
    this.sourceChunks.delete(source);
    this.sourceBytes.delete(source);
    this.sources.delete(source);
    const porterDelete = this.db.query("DELETE FROM porter_index WHERE source = ?");
    const trigramDelete = this.db.query("DELETE FROM trigram_index WHERE source = ?");
    porterDelete.run(source);
    trigramDelete.run(source);
  }

  private searchPorter(
    queryText: string,
    filters: { source?: string; session?: string },
    limit = DEFAULT_SEARCH_LIMIT
  ): SearchResult[] {
    const matchQuery = this.toMatchQuery(queryText);
    if (!matchQuery) {
      return [];
    }

    let sql =
      "SELECT content, source, title, bm25(porter_index) as score FROM porter_index WHERE porter_index MATCH ?";
    const params: (string | number)[] = [matchQuery];

    if (filters.source) {
      sql += " AND source = ?";
      params.push(filters.source);
    }
    if (filters.session) {
      sql += " AND session = ?";
      params.push(filters.session);
    }
    sql += " ORDER BY score LIMIT ?";
    params.push(limit);

    const rows = this.db.query(sql).all(...params);
    return (rows as PorterRow[]).map((row) => ({
      content: row.content,
      source: row.source,
      title: row.title,
      score: this.normalizeFtsScore(row.score, 1),
    }));
  }

  private searchTrigram(
    queryText: string,
    filters: { source?: string; session?: string },
    limit = DEFAULT_SEARCH_LIMIT
  ): SearchResult[] {
    const matchQuery = this.toMatchQuery(queryText);
    if (!matchQuery) {
      return [];
    }

    let sql =
      "SELECT content, source, title, bm25(trigram_index) as score FROM trigram_index WHERE trigram_index MATCH ?";
    const params: (string | number)[] = [matchQuery];

    if (filters.source) {
      sql += " AND source = ?";
      params.push(filters.source);
    }
    if (filters.session) {
      sql += " AND session = ?";
      params.push(filters.session);
    }
    sql += " ORDER BY score LIMIT ?";
    params.push(limit);

    const rows = this.db.query(sql).all(...params);
    return (rows as TrigramRow[]).map((row) => ({
      content: row.content,
      source: row.source,
      title: row.title,
      score: this.normalizeFtsScore(row.score, 0.8),
    }));
  }

  private searchFuzzy(
    queryText: string,
    filters: { source?: string; session?: string },
    limit = DEFAULT_SEARCH_LIMIT
  ): SearchResult[] {
    const likeToken = this.correctedFuzzyTerm(queryText);
    const escaped = this.escapeLike(likeToken);
    const likeTerm = `%${escaped}%`;

    let sql =
      "SELECT content, source, title FROM porter_index WHERE lower(content) LIKE lower(?) ESCAPE '\\'";
    const params: (string | number)[] = [likeTerm];

    if (filters.source) {
      sql += " AND source = ?";
      params.push(filters.source);
    }
    if (filters.session) {
      sql += " AND session = ?";
      params.push(filters.session);
    }
    sql += " LIMIT ?";
    params.push(limit);

    const rows = this.db.query(sql).all(...params);
    return (rows as LikeRow[]).map((row) => ({
      content: row.content,
      source: row.source,
      title: row.title,
      score: this.fuzzyScore(queryText, row.content),
    }));
  }

  private correctedFuzzyTerm(queryText: string): string {
    const queryTokens = this.extractTerms(queryText);
    const token = queryTokens[0] ?? queryText.toLowerCase();
    if (token.length < 3) {
      return token;
    }

    const terms = this.extractCorpusTerms();
    let best = token;
    let bestDistance = Number.POSITIVE_INFINITY;
    for (const term of terms) {
      if (Math.abs(term.length - token.length) > 4) {
        continue;
      }
      const distance = this.levenshtein(token, term);
      if (distance < bestDistance) {
        best = term;
        bestDistance = distance;
      }
      if (bestDistance === 0) {
        break;
      }
    }

    return bestDistance <= Math.ceil(token.length * 0.4) ? best : token;
  }

  private extractCorpusTerms(): string[] {
    const rows = this.db
      .query("SELECT content FROM porter_index ORDER BY rowid DESC LIMIT 200")
      .all() as Array<{ content: string }>;
    const terms = new Set<string>();
    for (const row of rows) {
      for (const term of this.extractTerms(row.content)) {
        terms.add(term);
      }
    }
    return Array.from(terms);
  }

  private fuzzyScore(query: string, content: string): number {
    const queryToken = this.extractTerms(query)[0] ?? query.toLowerCase();
    const contentTerms = this.extractTerms(content).slice(0, 200);
    let bestDistance = Number.POSITIVE_INFINITY;
    for (const term of contentTerms) {
      const distance = this.levenshtein(queryToken, term);
      if (distance < bestDistance) {
        bestDistance = distance;
      }
    }
    if (!Number.isFinite(bestDistance)) {
      return 0;
    }
    const denom = Math.max(queryToken.length, 1);
    return Math.max(0.05, 1 - bestDistance / denom) * 0.6;
  }

  private normalizeFtsScore(raw: number, weight: number): number {
    const abs = Math.abs(raw);
    return (weight * abs) / (1 + abs);
  }

  private toMatchQuery(queryText: string): string | null {
    const terms = this.extractTerms(queryText);
    if (terms.length === 0) {
      return null;
    }
    return terms.map((term) => `"${term.replaceAll('"', '""')}"`).join(" AND ");
  }

  private escapeLike(value: string): string {
    return value.replaceAll("\\", "\\\\").replaceAll("%", "\\%").replaceAll("_", "\\_");
  }

  private chunkContent(content: string): Chunk[] {
    if (!content.trim()) {
      return [];
    }
    if (this.looksLikeMarkdown(content)) {
      return this.chunkMarkdown(content);
    }
    return this.chunkPlainText(content);
  }

  private looksLikeMarkdown(content: string): boolean {
    return /(^|\n)#{1,6}\s+.+/m.test(content);
  }

  private chunkMarkdown(content: string): Chunk[] {
    const lines = content.split(/\r?\n/);
    const chunks: Chunk[] = [];
    const headingStack: string[] = [];
    let buffer: string[] = [];
    let inCodeBlock = false;
    let currentTitle = "Document";

    const flush = () => {
      const body = buffer.join("\n").trim();
      if (!body) {
        buffer = [];
        return;
      }
      chunks.push({
        title: currentTitle,
        content: body,
      });
      buffer = [];
    };

    for (const line of lines) {
      if (/^(```|~~~)/.test(line)) {
        inCodeBlock = !inCodeBlock;
      }

      // h1 headings are excluded — they represent document titles, not section breaks
      const headingMatch = !inCodeBlock ? line.match(/^(#{2,6})\s+(.+)$/) : null;
      if (headingMatch) {
        flush();
        const level = headingMatch[1].length;
        const text = headingMatch[2].trim();
        const index = level - 2;
        headingStack.length = Math.max(0, index);
        headingStack[index] = text;
        currentTitle = headingStack.filter(Boolean).join(" > ") || "Document";
        buffer.push(line);
        continue;
      }

      buffer.push(line);
    }

    flush();
    return chunks.length > 0 ? chunks : this.chunkPlainText(content);
  }

  private chunkPlainText(content: string): Chunk[] {
    const normalized = content.replace(/\r\n/g, "\n");
    const sections = normalized
      .split(/\n\s*\n/g)
      .map((section) => section.trim())
      .filter((section) => section.length > 0);
    const chunks: Chunk[] = [];
    const maxLines = 40;
    const overlap = 8;

    let chunkIndex = 1;
    for (const section of sections) {
      const lines = section.split("\n");
      if (lines.length <= maxLines) {
        chunks.push({
          title: `Chunk ${chunkIndex}`,
          content: section,
        });
        chunkIndex += 1;
        continue;
      }

      let start = 0;
      while (start < lines.length) {
        const end = Math.min(lines.length, start + maxLines);
        const chunkText = lines.slice(start, end).join("\n").trim();
        if (chunkText) {
          chunks.push({
            title: `Chunk ${chunkIndex}`,
            content: chunkText,
          });
          chunkIndex += 1;
        }
        if (end >= lines.length) {
          break;
        }
        start = Math.max(start + maxLines - overlap, start + 1);
      }
    }

    return chunks;
  }

  private extractVocabulary(chunks: Chunk[]): string[] {
    const documentCount = chunks.length;
    const docFreq = new Map<string, number>();
    const termFreq = new Map<string, number>();

    for (const chunk of chunks) {
      const terms = this.extractTerms(chunk.content);
      const seenInChunk = new Set<string>();
      for (const term of terms) {
        termFreq.set(term, (termFreq.get(term) ?? 0) + 1);
        if (!seenInChunk.has(term)) {
          docFreq.set(term, (docFreq.get(term) ?? 0) + 1);
          seenInChunk.add(term);
        }
      }
    }

    return Array.from(termFreq.entries())
      .map(([term, tf]) => {
        const df = docFreq.get(term) ?? 1;
        const idf = Math.log((documentCount + 1) / (df + 1)) + 1;
        return {
          term,
          score: tf * idf,
        };
      })
      .sort((a, b) => b.score - a.score)
      .slice(0, MAX_VOCAB_TERMS)
      .map((entry) => entry.term);
  }

  private extractTerms(value: string): string[] {
    return value
      .toLowerCase()
      .split(/[^a-z0-9]+/g)
      .map((term) => term.trim())
      .filter((term) => term.length >= 3 && !STOP_WORDS.has(term));
  }

  private levenshtein(a: string, b: string): number {
    if (a === b) {
      return 0;
    }
    if (a.length === 0) {
      return b.length;
    }
    if (b.length === 0) {
      return a.length;
    }

    const prev = new Array<number>(b.length + 1);
    const curr = new Array<number>(b.length + 1);

    for (let j = 0; j <= b.length; j += 1) {
      prev[j] = j;
    }

    for (let i = 1; i <= a.length; i += 1) {
      curr[0] = i;
      for (let j = 1; j <= b.length; j += 1) {
        const cost = a[i - 1] === b[j - 1] ? 0 : 1;
        curr[j] = Math.min(prev[j] + 1, curr[j - 1] + 1, prev[j - 1] + cost);
      }
      for (let j = 0; j <= b.length; j += 1) {
        prev[j] = curr[j];
      }
    }

    return prev[b.length];
  }
}
