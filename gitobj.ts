// The git object model, pkt-line codec and packfile read/write. Pure
// functions over Buffers: no I/O, no store. Only node:crypto and node:zlib.
//
// Object formats follow git exactly, because the suite's git client
// recomputes every object's SHA-1 and compares commit bytes byte for byte:
//   commit: "tree <oid>\n" + "parent <oid>\n"* + "author <who> <ts> +0000\n"
//           + "committer <who> <ts> +0000\n" + "\n" + message
//   tag:    "object <oid>\ntype <type>\ntag <name>\ntagger <who> <ts> +0000\n"
//           + "\n" + message
//   tree:   "<mode> <name>\0<20-byte oid>" per entry, sorted in git's order
//           (directories compare as name + "/").
//   blob:   the raw bytes.
// Packfiles are version 2, plain objects (no deltas on write; deltas are
// resolved on read so a delta pack never breaks an unpack).

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';
import { isOid, ZERO_OID } from './util.ts';

export type ObjType = 'blob' | 'tree' | 'commit' | 'tag';

export interface GitObject {
  type: ObjType;
  data: Buffer;
}

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

export interface Identity {
  name: string;
  email: string;
}

export interface ParsedCommit {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  authorTs: number;
  committerTs: number;
  message: string;
}

export interface ParsedTag {
  object: string;
  type: string;
  tag: string;
  tagger: string;
  message: string;
}

const TYPE_CODES: Record<ObjType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPES: Record<number, ObjType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

/** The id of an object: SHA-1 over "<type> <size>\0" + data. */
export function hashObject(type: ObjType, data: Buffer): string {
  const header = Buffer.from(`${type} ${data.length}\0`, 'utf8');
  return createHash('sha1').update(header).update(data).digest('hex');
}

/** Git's tree order: directories compare as their name plus "/". */
function treeSortKey(entry: TreeEntry): string {
  return entry.mode === '40000' ? `${entry.name}/` : entry.name;
}

/** Build a tree object from entries (sorted in git's order). */
export function buildTree(entries: TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => {
    const ka = treeSortKey(a);
    const kb = treeSortKey(b);
    return ka < kb ? -1 : ka > kb ? 1 : 0;
  });
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

/** Parse a tree object into its entries. */
export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let at = 0;
  while (at < data.length) {
    const space = data.indexOf(0x20, at);
    if (space < 0) throw new Error('malformed tree: no space after mode');
    const mode = data.subarray(at, space).toString('utf8');
    const nul = data.indexOf(0x00, space + 1);
    if (nul < 0) throw new Error('malformed tree: no NUL after name');
    const name = data.subarray(space + 1, nul).toString('utf8');
    if (nul + 21 > data.length) throw new Error('malformed tree: truncated oid');
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, oid });
    at = nul + 21;
  }
  return entries;
}

export interface CommitInput {
  tree: string;
  parents: string[];
  message: string;
  author: Identity;
  committer: Identity;
  timestamp: number;
}

/** Build a commit object. The message is used verbatim. */
export function buildCommit(input: CommitInput): Buffer {
  const who = (id: Identity): string => `${id.name} <${id.email}> ${input.timestamp} +0000`;
  let text = `tree ${input.tree}\n`;
  for (const parent of input.parents) text += `parent ${parent}\n`;
  text += `author ${who(input.author)}\n`;
  text += `committer ${who(input.committer)}\n`;
  text += `\n${input.message}`;
  return Buffer.from(text, 'utf8');
}

/** Parse a commit object. */
export function parseCommit(data: Buffer): ParsedCommit {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const header = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  let authorTs = 0;
  let committerTs = 0;
  for (const line of header.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) {
      author = line.slice(7);
      authorTs = parseWhoTs(author);
    } else if (line.startsWith('committer ')) {
      committer = line.slice(10);
      committerTs = parseWhoTs(committer);
    }
  }
  return { tree, parents, author, committer, authorTs, committerTs, message };
}

function parseWhoTs(who: string): number {
  const match = / (\d+) [+-]\d{4}$/.exec(who);
  return match === null ? 0 : Number(match[1]);
}

export interface TagInput {
  object: string;
  type: string;
  tag: string;
  tagger: Identity;
  timestamp: number;
  message: string;
}

/** Build an annotated tag object. */
export function buildTag(input: TagInput): Buffer {
  const text =
    `object ${input.object}\n` +
    `type ${input.type}\n` +
    `tag ${input.tag}\n` +
    `tagger ${input.tagger.name} <${input.tagger.email}> ${input.timestamp} +0000\n` +
    `\n${input.message}`;
  return Buffer.from(text, 'utf8');
}

/** Parse an annotated tag object. */
export function parseTag(data: Buffer): ParsedTag {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const header = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  let object = '';
  let type = '';
  let tag = '';
  let tagger = '';
  for (const line of header.split('\n')) {
    if (line.startsWith('object ')) object = line.slice(7).trim();
    else if (line.startsWith('type ')) type = line.slice(5).trim();
    else if (line.startsWith('tag ')) tag = line.slice(4).trim();
    else if (line.startsWith('tagger ')) tagger = line.slice(7);
  }
  return { object, type, tag, tagger, message };
}

// ---------------------------------------------------------- pkt-lines

/** One pkt-line: a 4-hex length (including the header) plus the payload. */
export function encodePktLine(data: string | Buffer): Buffer {
  const payload = typeof data === 'string' ? Buffer.from(data, 'utf8') : data;
  const len = payload.length + 4;
  if (len > 65520) throw new Error(`pkt-line of ${len} bytes exceeds the 65520 limit`);
  const header = Buffer.from(len.toString(16).padStart(4, '0'), 'ascii');
  return Buffer.concat([header, payload]);
}

/** The flush packet, `0000`. */
export const FLUSH_PKT = Buffer.from('0000', 'ascii');
/** The delimiter packet, `0001`. */
export const DELIM_PKT = Buffer.from('0001', 'ascii');
/** The response-end packet, `0002`. */
export const END_PKT = Buffer.from('0002', 'ascii');

/** Encode a list of lines (or 'flush'/'delim' markers) into pkt-lines. */
export function encodePktLines(lines: Array<string | 'flush' | 'delim' | 'end'>): Buffer {
  const parts: Buffer[] = [];
  for (const line of lines) {
    if (line === 'flush') parts.push(FLUSH_PKT);
    else if (line === 'delim') parts.push(DELIM_PKT);
    else if (line === 'end') parts.push(END_PKT);
    else parts.push(encodePktLine(line));
  }
  return Buffer.concat(parts);
}

export type PktKind = 'data' | 'flush' | 'delim' | 'end';

export interface Pkt {
  kind: PktKind;
  payload: Buffer;
}

/**
 * A streaming pkt-line decoder: feed it chunks, pull packets out. A packet
 * split across chunks is reassembled; a malformed length throws.
 */
export class PktLineDecoder {
  private buf: Buffer = Buffer.alloc(0);

  push(chunk: Buffer): void {
    this.buf = this.buf.length === 0 ? chunk : Buffer.concat([this.buf, chunk]);
  }

  next(): Pkt | null {
    if (this.buf.length < 4) return null;
    const hex = this.buf.subarray(0, 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(hex)) throw new Error(`bad pkt-line length ${JSON.stringify(hex)}`);
    const len = parseInt(hex, 16);
    if (len === 0) {
      this.buf = this.buf.subarray(4);
      return { kind: 'flush', payload: Buffer.alloc(0) };
    }
    if (len === 1) {
      this.buf = this.buf.subarray(4);
      return { kind: 'delim', payload: Buffer.alloc(0) };
    }
    if (len === 2) {
      this.buf = this.buf.subarray(4);
      return { kind: 'end', payload: Buffer.alloc(0) };
    }
    if (len < 4 || len > 65520) throw new Error(`bad pkt-line length ${len}`);
    if (this.buf.length < len) return null;
    const payload = this.buf.subarray(4, len);
    this.buf = this.buf.subarray(len);
    return { kind: 'data', payload };
  }

  /** Bytes not yet consumed (a trailing partial packet). */
  get rest(): Buffer {
    return this.buf;
  }

  /** True when the stream ended with no partial packet left. */
  finish(): void {
    if (this.buf.length > 0) throw new Error(`trailing ${this.buf.length} byte(s) after the last pkt-line`);
  }
}

/** Decode every packet of a complete buffer (throws on malformed input). */
export function decodePktLines(buffer: Buffer): Pkt[] {
  const decoder = new PktLineDecoder();
  decoder.push(buffer);
  const out: Pkt[] = [];
  for (;;) {
    const pkt = decoder.next();
    if (pkt === null) break;
    out.push(pkt);
  }
  decoder.finish();
  return out;
}

/** The data packets of a buffer as strings (no trailing newline). */
export function pktLinesAsStrings(buffer: Buffer): string[] {
  return decodePktLines(buffer)
    .filter((pkt) => pkt.kind === 'data')
    .map((pkt) => pkt.payload.toString('utf8').replace(/\n$/, ''));
}

// ---------------------------------------------------------- packfiles

export interface PackObjectInput {
  type: ObjType;
  data: Buffer;
}

/** Encode one object's pack header (type + size varint). */
function packObjectHeader(type: number, size: number): Buffer {
  const bytes: number[] = [];
  let first = (type << 4) | (size & 0x0f);
  size >>>= 4;
  if (size > 0) first |= 0x80;
  bytes.push(first);
  while (size > 0) {
    let byte = size & 0x7f;
    size >>>= 7;
    if (size > 0) byte |= 0x80;
    bytes.push(byte);
  }
  return Buffer.from(bytes);
}

/**
 * Write a packfile (version 2, plain objects, deflate level 1 for speed):
 * `PACK` + version + count + objects + SHA-1 trailer over the body.
 */
export function writePack(objects: readonly PackObjectInput[]): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  parts.push(header);
  for (const obj of objects) {
    parts.push(packObjectHeader(TYPE_CODES[obj.type], obj.data.length));
    parts.push(deflateSync(obj.data, { level: 1 }));
  }
  const body = Buffer.concat(parts);
  const trailer = createHash('sha1').update(body).digest();
  return Buffer.concat([body, trailer]);
}

/** Apply a git delta to a base buffer. */
export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let at = 0;
  const readVarint = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      if (at >= delta.length) throw new Error('delta varint overruns the buffer');
      const byte = delta[at] as number;
      at += 1;
      value |= (byte & 0x7f) << shift;
      shift += 7;
      if ((byte & 0x80) === 0) return value;
    }
  };
  const sourceSize = readVarint();
  const targetSize = readVarint();
  if (sourceSize !== base.length) throw new Error(`delta source size ${sourceSize} does not match the base ${base.length}`);
  const out: Buffer[] = [];
  let written = 0;
  while (at < delta.length) {
    const op = delta[at] as number;
    at += 1;
    if ((op & 0x80) !== 0) {
      // Copy from the base: offset and size are little-endian bit fields.
      let offset = 0;
      let size = 0;
      for (let i = 0; i < 4; i += 1) {
        if ((op & (1 << i)) !== 0) {
          if (at >= delta.length) throw new Error('delta copy overruns the buffer');
          offset |= (delta[at] as number) << (8 * i);
          at += 1;
        }
      }
      for (let i = 0; i < 3; i += 1) {
        if ((op & (0x10 << i)) !== 0) {
          if (at >= delta.length) throw new Error('delta copy overruns the buffer');
          size |= (delta[at] as number) << (8 * i);
          at += 1;
        }
      }
      if (size === 0) size = 0x10000;
      if (offset + size > base.length) throw new Error('delta copy reads past the base');
      out.push(base.subarray(offset, offset + size));
      written += size;
    } else if (op > 0) {
      // Insert literal bytes.
      if (at + op > delta.length) throw new Error('delta insert overruns the buffer');
      out.push(delta.subarray(at, at + op));
      at += op;
      written += op;
    } else {
      throw new Error('delta opcode 0 is reserved');
    }
  }
  const result = Buffer.concat(out);
  if (written !== targetSize || result.length !== targetSize) {
    throw new Error(`delta target size ${result.length} does not match ${targetSize}`);
  }
  return result;
}

export interface ReadPackResult {
  objects: Map<string, GitObject>;
  deltaCount: number;
}

/**
 * Read a packfile: verify the header, version and trailer checksum, inflate
 * every entry, resolve OFS_DELTA and REF_DELTA against the pack itself or
 * `baseOf` (a lookup for thin-pack bases the caller holds), and verify each
 * object's id and declared size. Throws on any corruption.
 */
export function readPack(pack: Buffer, baseOf?: (oid: string) => GitObject | undefined): ReadPackResult {
  if (pack.length < 32) throw new Error('packfile too short');
  if (pack.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('packfile does not start with PACK');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const count = pack.readUInt32BE(8);
  const trailer = pack.subarray(pack.length - 20);
  const actual = createHash('sha1').update(pack.subarray(0, pack.length - 20)).digest();
  if (!actual.equals(trailer)) throw new Error('pack trailer checksum does not match its bytes');
  const objects = new Map<string, GitObject>();
  const byOffset = new Map<number, string>();
  let deltaCount = 0;
  let at = 12;
  const end = pack.length - 20;
  for (let index = 0; index < count; index += 1) {
    const entryOffset = at;
    if (at >= end) throw new Error('packfile ends before its declared object count');
    // Type + size varint.
    let byte = pack[at] as number;
    at += 1;
    const typeCode = (byte >> 4) & 0x07;
    let size = byte & 0x0f;
    let shift = 4;
    while ((byte & 0x80) !== 0) {
      if (at >= end) throw new Error('pack object header overruns the pack');
      byte = pack[at] as number;
      at += 1;
      size |= (byte & 0x7f) << shift;
      shift += 7;
    }
    let type: ObjType;
    let data: Buffer;
    if (typeCode === 6) {
      // OFS_DELTA: a base offset encoded as a negative-offset varint.
      if (at >= end) throw new Error('ofs-delta overruns the pack');
      byte = pack[at] as number;
      at += 1;
      let offset = byte & 0x7f;
      while ((byte & 0x80) !== 0) {
        if (at >= end) throw new Error('ofs-delta overruns the pack');
        byte = pack[at] as number;
        at += 1;
        offset = ((offset + 1) << 7) | (byte & 0x7f);
      }
      const baseOffset = entryOffset - offset;
      const baseOid = byOffset.get(baseOffset);
      if (baseOid === undefined) throw new Error('ofs-delta base is not in the pack');
      const base = objects.get(baseOid);
      if (base === undefined) throw new Error('ofs-delta base object is missing');
      const { data: inflated, consumed } = inflateOne(pack, at);
      at += consumed;
      // The header size of a delta is the delta data's size, not the
      // result's: check the inflated delta, before applying it.
      if (inflated.length !== size) throw new Error(`pack delta size ${inflated.length} does not match its header ${size}`);
      data = applyDelta(base.data, inflated);
      type = base.type;
      deltaCount += 1;
    } else if (typeCode === 7) {
      // REF_DELTA: a 20-byte base id.
      if (at + 20 > end) throw new Error('ref-delta overruns the pack');
      const baseOid = pack.subarray(at, at + 20).toString('hex');
      at += 20;
      const base = objects.get(baseOid) ?? baseOf?.(baseOid);
      if (base === undefined) throw new Error(`ref-delta base ${baseOid} is not available`);
      const { data: inflated, consumed } = inflateOne(pack, at);
      at += consumed;
      // The header size of a delta is the delta data's size, not the result's.
      if (inflated.length !== size) throw new Error(`pack delta size ${inflated.length} does not match its header ${size}`);
      data = applyDelta(base.data, inflated);
      type = base.type;
      deltaCount += 1;
    } else {
      type = CODE_TYPES[typeCode] as ObjType;
      if (type === undefined) throw new Error(`unknown pack object type ${typeCode}`);
      const { data: inflated, consumed } = inflateOne(pack, at);
      at += consumed;
      // A plain object: the header size is the object's own size.
      if (inflated.length !== size) throw new Error(`pack object size ${inflated.length} does not match its header ${size}`);
      data = inflated;
    }
    const oid = hashObject(type, data);
    if (!objects.has(oid)) objects.set(oid, { type, data });
    byOffset.set(entryOffset, oid);
  }
  return { objects, deltaCount };
}

/** Adler-32 of `data` (the checksum a zlib stream ends with). The modulo is
 * deferred to 5552-byte blocks, as zlib does, so a large object stays fast. */
function adler32(data: Buffer): number {
  let a = 1;
  let b = 0;
  const BLOCK = 5552;
  for (let off = 0; off < data.length; off += BLOCK) {
    const end = Math.min(data.length, off + BLOCK);
    for (let i = off; i < end; i += 1) {
      a += data[i] as number;
      b += a;
    }
    a %= 65521;
    b %= 65521;
  }
  return ((b << 16) | a) >>> 0;
}

/**
 * Inflate the one zlib stream starting at `at` and say how many bytes it
 * consumes. inflateSync ignores trailing bytes, so it returns the first
 * stream's data but not its length; the length comes from the stream's own
 * Adler-32 trailer. The trailer is the last 4 bytes of the stream, so the
 * stream ends where those 4 bytes sit — found by scanning for them, and
 * each candidate is verified by re-inflating the prefix (a match inside the
 * deflate data inflates truncated and fails, so the first candidate that
 * verifies is the real end; the real end always comes before any match past
 * it, so a verifying candidate is never past the stream). Two inflateSync
 * calls per object, not the binary search's log2(n).
 */
function inflateOne(pack: Buffer, at: number): { data: Buffer; consumed: number } {
  const data = inflateSync(pack.subarray(at));
  const needle = Buffer.alloc(4);
  needle.writeUInt32BE(adler32(data), 0);
  let from = at + 2;
  for (;;) {
    const idx = pack.indexOf(needle, from);
    if (idx < 0) throw new Error('zlib stream end not found');
    const end = idx + 4;
    try {
      const check = inflateSync(pack.subarray(at, end));
      if (check.equals(data)) return { data, consumed: end - at };
    } catch {
      // A match inside the deflate data: the truncated prefix cannot inflate.
    }
    from = idx + 1;
  }
}

// ---------------------------------------------------------- reachability

export interface FetchPlan {
  /** Objects to send, in pack order. */
  objects: GitObject[];
  /** Boundary commits whose history was cut off by the depth limit. */
  shallow: string[];
  /** Previously-shallow commits that are no longer boundaries. */
  unshallow: string[];
}

interface CommitWalk {
  /** Commits reachable from the wants, with depth (1 = a want). */
  included: Map<string, number>;
  /** Commits the client already holds (haves, not past shallow lines). */
  held: Set<string>;
}

/**
 * The fetch plan for one upload-pack request. `wants` are the tips, `haves`
 * the commits the client holds, `depth` the shallow limit (undefined = the
 * whole history), `priorShallow` the client's current shallow boundaries.
 *
 * The pack holds exactly the objects reachable from the wants within the
 * depth that the client does not already hold: a commit the client holds is
 * excluded, but the walk passes through it (a deepen re-fetches ancestors
 * of a held want), and a held commit's tree and blobs are excluded too.
 * Boundary commits (at exactly the depth limit, with parents) are reported
 * as shallow; previously-shallow commits that are now fully covered are
 * reported as unshallow.
 */
export function planFetch(
  lookup: (oid: string) => GitObject | undefined,
  wants: string[],
  haves: string[],
  depth: number | undefined,
  priorShallow: string[],
): FetchPlan {
  const limit = depth === undefined ? Infinity : depth;
  // 1. The want closure, with depth.
  const included = new Map<string, number>();
  const queue: Array<{ oid: string; d: number }> = [];
  let qhead = 0;
  for (const want of wants) {
    if (isOid(want) && want !== ZERO_OID && !included.has(want)) {
      included.set(want, 1);
      queue.push({ oid: want, d: 1 });
    }
  }
  while (qhead < queue.length) {
    const { oid, d } = queue[qhead++] as { oid: string; d: number };
    if (d >= limit) continue;
    const obj = lookup(oid);
    if (obj === undefined || obj.type !== 'commit') continue;
    for (const parent of parseCommit(obj.data).parents) {
      if (!included.has(parent)) {
        included.set(parent, d + 1);
        queue.push({ oid: parent, d: d + 1 });
      }
    }
  }
  // 2. The held closure: from the haves, never past a prior shallow line.
  const held = new Set<string>();
  const priorSet = new Set(priorShallow);
  const hqueue: string[] = [];
  let hhead = 0;
  for (const have of haves) {
    if (isOid(have) && have !== ZERO_OID && !held.has(have)) {
      held.add(have);
      hqueue.push(have);
    }
  }
  while (hhead < hqueue.length) {
    const oid = hqueue[hhead++] as string;
    if (priorSet.has(oid)) continue; // a shallow boundary: held, but not its parents
    const obj = lookup(oid);
    if (obj === undefined || obj.type !== 'commit') continue;
    for (const parent of parseCommit(obj.data).parents) {
      if (!held.has(parent)) {
        held.add(parent);
        hqueue.push(parent);
      }
    }
  }
  // 3. The pack commits: included, not held.
  const packCommits: string[] = [];
  for (const oid of included.keys()) {
    if (!held.has(oid)) packCommits.push(oid);
  }
  // 4. The held objects (a held commit's whole tree), for exclusion.
  const heldObjects = new Set<string>();
  const collectTreeObjects = (rootOid: string, into: Set<string>): void => {
    const seen = new Set<string>();
    const walk = (treeOid: string): void => {
      if (seen.has(treeOid)) return;
      seen.add(treeOid);
      into.add(treeOid);
      const tree = lookup(treeOid);
      if (tree === undefined || tree.type !== 'tree') return;
      for (const entry of parseTree(tree.data)) {
        if (entry.mode === '40000') walk(entry.oid);
        else if (entry.mode !== '160000') into.add(entry.oid);
      }
    };
    walk(rootOid);
  };
  for (const oid of held) {
    heldObjects.add(oid);
    const obj = lookup(oid);
    if (obj !== undefined && obj.type === 'commit') collectTreeObjects(parseCommit(obj.data).tree, heldObjects);
  }
  // 5. The pack objects: each pack commit plus its tree, minus held objects.
  const objects = new Map<string, GitObject>();
  const addTree = (treeOid: string): void => {
    const seen = new Set<string>();
    const walk = (oid: string): void => {
      if (seen.has(oid) || heldObjects.has(oid)) return;
      seen.add(oid);
      const tree = lookup(oid);
      if (tree === undefined) return;
      objects.set(oid, tree);
      for (const entry of parseTree(tree.data)) {
        if (entry.mode === '40000') walk(entry.oid);
        else if (entry.mode !== '160000' && !heldObjects.has(entry.oid)) {
          const blob = lookup(entry.oid);
          if (blob !== undefined) objects.set(entry.oid, blob);
        }
      }
    };
    walk(treeOid);
  };
  for (const oid of packCommits) {
    const obj = lookup(oid);
    if (obj === undefined) continue;
    objects.set(oid, obj);
    if (obj.type === 'commit') addTree(parseCommit(obj.data).tree);
    else if (obj.type === 'tag') {
      const target = parseTag(obj.data).object;
      const peeled = lookup(target);
      if (peeled !== undefined && peeled.type === 'commit') {
        objects.set(target, peeled);
        addTree(parseCommit(peeled.data).tree);
      }
    }
  }
  // 6. Shallow / unshallow.
  const shallow: string[] = [];
  if (Number.isFinite(limit)) {
    for (const [oid, d] of included) {
      if (d !== limit) continue;
      const obj = lookup(oid);
      if (obj === undefined || obj.type !== 'commit') continue;
      if (parseCommit(obj.data).parents.length > 0) shallow.push(oid);
    }
  }
  const boundary = new Set(shallow);
  const unshallow: string[] = [];
  for (const oid of priorShallow) {
    if (included.has(oid) && !boundary.has(oid)) unshallow.push(oid);
  }
  return { objects: [...objects.values()], shallow, unshallow };
}

/** Every object reachable from `tips` (commits, trees, blobs), by id. */
export function reachableObjects(lookup: (oid: string) => GitObject | undefined, tips: string[]): Set<string> {
  const out = new Set<string>();
  const queue: string[] = [];
  let qhead = 0;
  for (const tip of tips) {
    if (isOid(tip) && tip !== ZERO_OID && !out.has(tip)) {
      out.add(tip);
      queue.push(tip);
    }
  }
  while (qhead < queue.length) {
    const oid = queue[qhead++] as string;
    const obj = lookup(oid);
    if (obj === undefined) continue;
    if (obj.type === 'commit') {
      const commit = parseCommit(obj.data);
      if (!out.has(commit.tree)) {
        out.add(commit.tree);
        queue.push(commit.tree);
      }
      for (const parent of commit.parents) {
        if (!out.has(parent)) {
          out.add(parent);
          queue.push(parent);
        }
      }
    } else if (obj.type === 'tree') {
      for (const entry of parseTree(obj.data)) {
        if (entry.mode === '160000') continue;
        if (!out.has(entry.oid)) {
          out.add(entry.oid);
          queue.push(entry.oid);
        }
      }
    } else if (obj.type === 'tag') {
      const target = parseTag(obj.data).object;
      if (!out.has(target)) {
        out.add(target);
        queue.push(target);
      }
    }
  }
  return out;
}
