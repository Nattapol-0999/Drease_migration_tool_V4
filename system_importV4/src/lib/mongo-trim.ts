import "server-only";

import { createReadStream, createWriteStream } from "node:fs";
import { stat } from "node:fs/promises";
import { pipeline } from "node:stream/promises";
import { Transform, type TransformCallback } from "node:stream";

import { MONGO_COLLECTIONS_USED } from "./types";

/**
 * Drops the collections system_CLIV4 does not read, before the CLI sees the file.
 *
 * WHY THIS EXISTS
 * The CLI loads its input with os.ReadFile and keeps every parsed document in memory â€”
 * measured at ~6.4x the file size on this hardware (26.8 MB dump â†’ 171.6 MB peak RSS).
 * A legacy `mris` export is mostly audit logs: in the file measured, 60 of 72 collections
 * were things the parser skips anyway, and they were 59% of the bytes. Removing them here
 * turns a 231 MB upload into a ~40 MB input, which fits in a 908 MB box with the web app
 * still running. Nothing is lost that the CLI would have used: the same statements it
 * ignores are the ones removed.
 *
 * WHY BYTE-LEVEL AND NOT readline
 * Two dump shapes have to survive this: Navicat writes pretty-printed inserts across many
 * lines, while `mongoexport --jsonArray` writes ONE line per collection â€” which can be
 * hundreds of megabytes long. readline would buffer that whole line, reintroducing the
 * memory problem this function exists to solve. Scanning bytes lets a single line stream
 * through with a fixed-size buffer.
 *
 * Splitting on 0x0A is safe for UTF-8: a newline byte never appears inside a multi-byte
 * sequence, so cutting there cannot split a Thai character.
 */

const NL = 0x0a;

/**
 * How much of a line is read before deciding whether to keep it. A statement header is
 * `db.getCollection("payment_summary").insertMany([` â€” around 50 bytes. 512 leaves room
 * for odd spacing without ever holding a data line in memory.
 */
const HEADER_PEEK = 512;

/**
 * Matches the statement shapes internal/mongodump accepts (its package doc lists them):
 * db.x.insert / insertOne / insertMany / save, with or without getCollection(), and with
 * either quote style.
 */
const INSERT_RE =
  /^db\s*(?:\.\s*getCollection\s*\(\s*["']([^"']+)["']\s*\)|\.\s*([A-Za-z0-9_$]+)|\[\s*["']([^"']+)["']\s*\])\s*\.\s*(?:insert|insertOne|insertMany|save)\b/;

export interface TrimStat {
  statements: number;
  bytes: number;
}

export interface TrimResult {
  /** Bytes written to the trimmed file. */
  keptBytes: number;
  /** Bytes read from the source. */
  sourceBytes: number;
  /** Per collection that was kept. */
  kept: Record<string, TrimStat>;
  /** Collection name â†’ how many statements were dropped. */
  dropped: Record<string, number>;
  /** Statements that are not inserts at all: drop(), createIndex(), use, printâ€¦ */
  skippedOther: number;
  seconds: string;
}

/** Filter stream: passes through only insert statements for the wanted collections. */
class TrimTransform extends Transform {
  private readonly keep: Set<string>;
  private atLineStart = true;
  /** Header bytes collected while a decision is pending. */
  private header: Buffer[] = [];
  private headerLen = 0;
  /** Whether the statement currently being scanned is being written out. */
  private writing = false;
  private current = "";

  readonly kept: Record<string, TrimStat> = {};
  readonly dropped: Record<string, number> = {};
  skippedOther = 0;
  keptBytes = 0;

  constructor(keep: readonly string[]) {
    super();
    this.keep = new Set(keep);
  }

  /** Called once a line's opening bytes are known. Decides keep/drop for the statement. */
  private decide(head: Buffer): void {
    // The pattern is pure ASCII, so latin1 is enough and never throws on a byte sequence
    // cut mid-character â€” which utf8 decoding of a partial line could do.
    const text = head.toString("latin1");
    if (!text.startsWith("db")) {
      // A continuation line of the statement already being scanned (data, closing braces)
      // or a comment/blank line before any statement. Keep the current decision:
      // flipping it here is how half a document ends up in the output.
      return;
    }
    const m = INSERT_RE.exec(text);
    if (!m) {
      // db.x.drop() / createCollection() / createIndex() / use â€” never wanted. drop() in
      // particular must not travel: the CLI skips it, but a file that does not contain it
      // is one less thing to be wrong about.
      this.writing = false;
      this.current = "";
      this.skippedOther++;
      return;
    }
    const name = m[1] ?? m[2] ?? m[3] ?? "";
    this.current = name;
    this.writing = this.keep.has(name);
    if (this.writing) {
      const s = (this.kept[name] ??= { statements: 0, bytes: 0 });
      s.statements++;
    } else {
      this.dropped[name] = (this.dropped[name] ?? 0) + 1;
    }
  }

  /** Not named `emit`: that is EventEmitter's, and shadowing it breaks the stream. */
  private writeOut(buf: Buffer): void {
    if (buf.length === 0) return;
    this.push(buf);
    this.keptBytes += buf.length;
    if (this.current) {
      const s = this.kept[this.current];
      if (s) s.bytes += buf.length;
    }
  }

  /** Flushes the pending header once a decision has been made. */
  private flushHeader(): void {
    if (this.headerLen === 0) return;
    const head = this.header.length === 1 ? this.header[0] : Buffer.concat(this.header);
    this.header = [];
    this.headerLen = 0;
    this.decide(head);
    if (this.writing) this.writeOut(head);
  }

  override _transform(chunk: Buffer, _enc: BufferEncoding, done: TransformCallback): void {
    try {
      let i = 0;
      while (i < chunk.length) {
        if (this.atLineStart) {
          const nl = chunk.indexOf(NL, i);
          const lineEnd = nl === -1 ? chunk.length : nl;
          const room = HEADER_PEEK - this.headerLen;
          const take = Math.min(lineEnd - i, room);
          if (take > 0) {
            this.header.push(chunk.subarray(i, i + take));
            this.headerLen += take;
            i += take;
          }
          // Decide as soon as the line ends or enough of it has been seen. Waiting longer
          // would mean buffering a data line, which is the failure mode being avoided.
          if (i === lineEnd || this.headerLen >= HEADER_PEEK) {
            this.flushHeader();
            this.atLineStart = false;
          }
          continue;
        }

        // Inside a line whose fate is already decided: stream it, do not buffer it.
        const nl = chunk.indexOf(NL, i);
        const end = nl === -1 ? chunk.length : nl + 1;
        if (this.writing) this.writeOut(chunk.subarray(i, end));
        i = end;
        if (nl !== -1) this.atLineStart = true;
      }
      done();
    } catch (err) {
      // Surfaced, never swallowed: a half-written trimmed file must fail the request, not
      // quietly become the input to an import.
      done(err instanceof Error ? err : new Error(String(err)));
    }
  }

  override _flush(done: TransformCallback): void {
    try {
      this.flushHeader();
      done();
    } catch (err) {
      done(err instanceof Error ? err : new Error(String(err)));
    }
  }
}

/**
 * trimMongoDump streams `src` into `dst`, keeping only the collections in `keep`.
 * Memory stays flat regardless of file size.
 */
export async function trimMongoDump(
  src: string,
  dst: string,
  keep: readonly string[] = MONGO_COLLECTIONS_USED,
): Promise<TrimResult> {
  const started = Date.now();
  const sourceBytes = (await stat(src)).size;
  const filter = new TrimTransform(keep);

  await pipeline(
    createReadStream(src, { highWaterMark: 1 << 18 }),
    filter,
    createWriteStream(dst),
  );

  return {
    keptBytes: filter.keptBytes,
    sourceBytes,
    kept: filter.kept,
    dropped: filter.dropped,
    skippedOther: filter.skippedOther,
    seconds: ((Date.now() - started) / 1000).toFixed(1),
  };
}

/** Human-readable one-liner for the log the operator sees. */
export function describeTrim(r: TrimResult): string {
  const keptNames = Object.entries(r.kept)
    .sort((a, b) => b[1].bytes - a[1].bytes)
    .map(([n, s]) => `${n} ${(s.bytes / 1048576).toFixed(2)} MB (${s.statements})`);
  const droppedNames = Object.keys(r.dropped).sort();
  const mb = (n: number) => `${(n / 1048576).toFixed(1)} MB`;
  return [
    `à¸à¸£à¸­à¸‡ MongoDB: ${mb(r.sourceBytes)} â†’ ${mb(r.keptBytes)} (${(
      (100 * r.keptBytes) / Math.max(1, r.sourceBytes)
    ).toFixed(1)}%) à¹ƒà¸™ ${r.seconds}s`,
    `à¹€à¸à¹‡à¸š ${Object.keys(r.kept).length} collection: ${keptNames.join(" Â· ") || "(à¹„à¸¡à¹ˆà¸¡à¸µ)"}`,
    `à¸—à¸´à¹‰à¸‡ ${droppedNames.length} collection: ${droppedNames.join(", ") || "(à¹„à¸¡à¹ˆà¸¡à¸µ)"}`,
    `à¸‚à¹‰à¸²à¸¡ statement à¸—à¸µà¹ˆà¹„à¸¡à¹ˆà¹ƒà¸Šà¹ˆ insert (drop/createIndex/use): ${r.skippedOther}`,
  ].join("\n");
}
