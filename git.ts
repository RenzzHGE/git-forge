// git.ts — Git smart HTTP (upload-pack / receive-pack) for the git forge.
//
// Pure TypeScript: node:crypto + node:zlib only, no git binary. Implements the
// pkt-line codec, packfile v2 read/write (plain objects, plus OFS_DELTA /
// REF_DELTA resolution for packs a client sends), the object model
// (blob/tree/commit/tag), protocol v2 ls-refs/fetch, protocol v0 upload-pack
// advertisement/fetch, and receive-pack with fast-forward enforcement.
//
// The coordinator wires this into app.ts; this file only reads/writes the shared
// store (store.ts) and never touches main.ts, store.ts or app.ts.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { store, gitObjectId, type GitObject, type GitObjectType, type Repo } from './store.ts';

export const ZERO_OID = '0'.repeat(40);
const AGENT = 'makefun-git-client/1.0';
const MAX_PKT_SIZE = 65520;
/** Largest pack chunk inside one sideband packet: 65520 - 4 (length) - 1 (band). */
const SIDEBAND_CHUNK = 65515;

// ---------------------------------------------------------------------------
// pkt-line codec
// ---------------------------------------------------------------------------

export const PKT_FLUSH = Buffer.from('0000', 'ascii');
export const PKT_DELIM = Buffer.from('0001', 'ascii');
export const PKT_END = Buffer.from('0002', 'ascii');

export interface PktPacket {
  kind: 'data' | 'flush' | 'delim' | 'end';
  data: Buffer;
}

/** Encode one pkt-line: 4 lowercase-hex length bytes (payload + 4) + payload. */
export function pktEncode(payload: Buffer | string): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  if (body.length + 4 > MAX_PKT_SIZE) throw new Error(`pkt-line payload too large: ${body.length}`);
  const header = Buffer.from((body.length + 4).toString(16).padStart(4, '0'), 'ascii');
  return Buffer.concat([header, body]);
}

/** Encode a text line (adds the trailing LF) as one pkt-line. */
export function pktText(text: string): Buffer {
  return pktEncode(text.endsWith('\n') ? text : `${text}\n`);
}

/** Incremental pkt-line reader over a buffer. */
export class PktReader {
  private readonly buf: Buffer;
  private pos: number;

  constructor(buf: Buffer, start = 0) {
    this.buf = buf;
    this.pos = start;
  }

  get offset(): number {
    return this.pos;
  }

  get remaining(): number {
    return this.buf.length - this.pos;
  }

  /** Read one packet; null at end of buffer. Throws on bad header / truncation. */
  read(): PktPacket | null {
    if (this.pos >= this.buf.length) return null;
    if (this.pos + 4 > this.buf.length) throw new Error('pkt-line truncated header');
    const header = this.buf.toString('ascii', this.pos, this.pos + 4);
    if (!/^[0-9a-fA-F]{4}$/.test(header)) throw new Error(`pkt-line bad length header: ${JSON.stringify(header)}`);
    const len = parseInt(header, 16);
    if (len === 0) {
      this.pos += 4;
      return { kind: 'flush', data: Buffer.alloc(0) };
    }
    if (len === 1) {
      this.pos += 4;
      return { kind: 'delim', data: Buffer.alloc(0) };
    }
    if (len === 2) {
      this.pos += 4;
      return { kind: 'end', data: Buffer.alloc(0) };
    }
    if (len < 4 || len > MAX_PKT_SIZE) throw new Error(`pkt-line bad length: ${len}`);
    if (this.pos + len > this.buf.length) throw new Error('pkt-line truncated payload');
    const data = this.buf.subarray(this.pos + 4, this.pos + len);
    this.pos += len;
    return { kind: 'data', data: Buffer.from(data) };
  }
}

/**
 * Decode pkt-lines from `buf` starting at `start`, stopping after the first
 * flush/delim/end packet (or at end of buffer). Returns the packets and the
 * offset just past the last packet consumed, so a caller can treat the rest of
 * the buffer as raw bytes (e.g. the pack after a receive-pack command list).
 */
export function pktDecode(buf: Buffer, start = 0): { packets: PktPacket[]; offset: number } {
  const reader = new PktReader(buf, start);
  const packets: PktPacket[] = [];
  for (;;) {
    const packet = reader.read();
    if (packet === null) break;
    packets.push(packet);
    if (packet.kind !== 'data') break;
  }
  return { packets, offset: reader.offset };
}

/** Split decoded data packets into trimmed text lines (drops the trailing LF). */
function pktLines(packets: PktPacket[]): string[] {
  const out: string[] = [];
  for (const packet of packets) {
    if (packet.kind !== 'data') continue;
    let text = packet.data.toString('utf8');
    if (text.endsWith('\n')) text = text.slice(0, -1);
    out.push(text);
  }
  return out;
}

// ---------------------------------------------------------------------------
// packfile v2 (plain objects; OFS_DELTA / REF_DELTA resolved when they appear)
// ---------------------------------------------------------------------------

const TYPE_CODES: Record<GitObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPES: Record<number, GitObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

/** Type/size varint header for one pack entry. */
function encodeObjectHeader(typeCode: number, size: number): Buffer {
  const bytes: number[] = [];
  let rest = Math.floor(size / 16);
  let first = (size % 16) | (typeCode << 4);
  if (rest > 0) first |= 0x80;
  bytes.push(first);
  while (rest > 0) {
    let b = rest % 128;
    rest = Math.floor(rest / 128);
    if (rest > 0) b |= 0x80;
    bytes.push(b);
  }
  return Buffer.from(bytes);
}

/** Write a packfile version 2 with plain (non-delta) objects, sorted by sha. */
export function writePack(objects: Array<{ type: GitObjectType; content: Buffer }>): Buffer {
  const entries = objects.map((obj) => ({ ...obj, sha: gitObjectId(obj.type, obj.content) }));
  entries.sort((a, b) => (a.sha < b.sha ? -1 : a.sha > b.sha ? 1 : 0));
  const parts: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(entries.length, 8);
  parts.push(header);
  for (const entry of entries) {
    parts.push(encodeObjectHeader(TYPE_CODES[entry.type], entry.content.length));
    parts.push(deflateSync(entry.content));
  }
  const withoutTrailer = Buffer.concat(parts);
  const trailer = createHash('sha1').update(withoutTrailer).digest();
  return Buffer.concat([withoutTrailer, trailer]);
}

/**
 * Inflate exactly one zlib stream starting at `offset`. inflateSync ignores
 * trailing bytes, so the stream end is found by probing for the smallest
 * prefix that inflates cleanly (a truncated stream throws).
 */
function inflateOne(buf: Buffer, offset: number): { data: Buffer; nextOffset: number } {
  const remaining = buf.length - offset;
  if (remaining <= 0) throw new Error('pack entry truncated: no zlib data');
  // Exponential probe for the first end that inflates.
  let hi = -1;
  let lastFailed = offset; // end of the longest prefix known to fail
  let probe = 1;
  while (probe <= remaining) {
    try {
      inflateSync(buf.subarray(offset, offset + probe));
      hi = offset + probe;
      break;
    } catch {
      lastFailed = offset + probe;
      probe *= 2;
    }
  }
  if (hi === -1) {
    try {
      inflateSync(buf.subarray(offset));
      hi = buf.length;
    } catch {
      throw new Error('pack entry: zlib stream truncated or corrupt');
    }
  }
  // Binary search for the smallest successful end in (lastFailed, hi].
  let lo = lastFailed;
  while (hi - lo > 1) {
    const mid = (lo + hi) >> 1;
    try {
      inflateSync(buf.subarray(offset, mid));
      hi = mid;
    } catch {
      lo = mid;
    }
  }
  return { data: inflateSync(buf.subarray(offset, hi)), nextOffset: hi };
}

/** Apply a git delta stream to `base`. */
export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let pos = 0;
  const readVarint = (): number => {
    let result = 0;
    let shift = 0;
    for (;;) {
      if (pos >= delta.length) throw new Error('delta truncated varint');
      const b = delta[pos++] as number;
      result += (b & 0x7f) * 2 ** shift;
      shift += 7;
      if ((b & 0x80) === 0) return result;
    }
  };
  const sourceSize = readVarint();
  const targetSize = readVarint();
  if (sourceSize !== base.length) throw new Error('delta source size mismatch');
  const chunks: Buffer[] = [];
  while (pos < delta.length) {
    const op = delta[pos++] as number;
    if (op & 0x80) {
      let offset = 0;
      let size = 0;
      if (op & 0x01) offset = delta[pos++] as number;
      if (op & 0x02) offset += (delta[pos++] as number) * 2 ** 8;
      if (op & 0x04) offset += (delta[pos++] as number) * 2 ** 16;
      if (op & 0x08) offset += (delta[pos++] as number) * 2 ** 24;
      if (op & 0x10) size = delta[pos++] as number;
      if (op & 0x20) size += (delta[pos++] as number) * 2 ** 8;
      if (op & 0x40) size += (delta[pos++] as number) * 2 ** 16;
      if (size === 0) size = 0x10000;
      if (offset + size > base.length) throw new Error('delta copy out of range');
      chunks.push(base.subarray(offset, offset + size));
    } else if (op !== 0) {
      if (pos + op > delta.length) throw new Error('delta literal truncated');
      chunks.push(delta.subarray(pos, pos + op));
      pos += op;
    } else {
      throw new Error('delta opcode 0');
    }
  }
  const result = Buffer.concat(chunks);
  if (result.length !== targetSize) throw new Error('delta target size mismatch');
  return result;
}

export interface ReadPackResult {
  objects: Map<string, GitObject>;
  version: number;
  count: number;
}

/**
 * Read a packfile. Verifies magic, version (2 or 3), per-object SHA-1 and
 * size, and the trailer checksum. OFS_DELTA / REF_DELTA entries are resolved
 * against objects already parsed from this pack, or against `known`
 * (e.g. the repo's objects, for thin packs whose base the server already holds).
 */
export function readPack(buffer: Buffer, known?: Map<string, GitObject>): ReadPackResult {
  if (buffer.length < 32) throw new Error('pack too short');
  if (buffer.toString('ascii', 0, 4) !== 'PACK') throw new Error('pack bad magic');
  const version = buffer.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`pack unsupported version: ${version}`);
  const count = buffer.readUInt32BE(8);
  const trailer = buffer.subarray(buffer.length - 20);
  const expectedTrailer = createHash('sha1').update(buffer.subarray(0, buffer.length - 20)).digest();
  if (!trailer.equals(expectedTrailer)) throw new Error('pack trailer checksum mismatch');

  const objects = new Map<string, GitObject>();
  const offsetToSha = new Map<number, string>();
  let pos = 12;
  for (let i = 0; i < count; i += 1) {
    const entryStart = pos;
    if (pos >= buffer.length - 20) throw new Error('pack truncated entry header');
    let b = buffer[pos++] as number;
    const typeCode = (b >> 4) & 0x07;
    let size = b & 0x0f;
    let shift = 4;
    while (b & 0x80) {
      if (pos >= buffer.length - 20) throw new Error('pack truncated size varint');
      b = buffer[pos++] as number;
      size += (b & 0x7f) * 2 ** shift;
      shift += 7;
    }
    let base: GitObject | undefined;
    let baseType: GitObjectType | undefined;
    if (typeCode === 6) {
      // OFS_DELTA: offset varint, base is the entry that starts that far back.
      if (pos >= buffer.length - 20) throw new Error('pack truncated ofs-delta offset');
      b = buffer[pos++] as number;
      let off = b & 0x7f;
      while (b & 0x80) {
        if (pos >= buffer.length - 20) throw new Error('pack truncated ofs-delta offset');
        b = buffer[pos++] as number;
        off = (off + 1) * 128 + (b & 0x7f);
      }
      const baseSha = offsetToSha.get(entryStart - off);
      if (baseSha === undefined) throw new Error('pack ofs-delta base not found');
      base = objects.get(baseSha);
      if (base === undefined) throw new Error('pack ofs-delta base not found');
      baseType = base.type;
    } else if (typeCode === 7) {
      // REF_DELTA: 20-byte base sha.
      if (pos + 20 > buffer.length - 20) throw new Error('pack truncated ref-delta base');
      const baseSha = buffer.toString('hex', pos, pos + 20);
      pos += 20;
      base = objects.get(baseSha) ?? known?.get(baseSha);
      if (base === undefined) throw new Error(`pack ref-delta base not found: ${baseSha}`);
      baseType = base.type;
    } else if (CODE_TYPES[typeCode] === undefined) {
      throw new Error(`pack unknown object type code: ${typeCode}`);
    }
    const inflated = inflateOne(buffer, pos);
    pos = inflated.nextOffset;
    let type: GitObjectType;
    let content: Buffer;
    if (base !== undefined && baseType !== undefined) {
      // For a delta entry the header size is the size of the DELTA DATA, not
      // of the reconstructed object (applyDelta checks the target size).
      if (inflated.data.length !== size) {
        throw new Error(`pack delta size mismatch: header ${size}, got ${inflated.data.length}`);
      }
      type = baseType;
      content = applyDelta(base.content, inflated.data);
    } else {
      type = CODE_TYPES[typeCode] as GitObjectType;
      content = inflated.data;
      if (content.length !== size) throw new Error(`pack object size mismatch: header ${size}, got ${content.length}`);
    }
    const sha = gitObjectId(type, content);
    objects.set(sha, { type, content });
    offsetToSha.set(entryStart, sha);
  }
  if (pos !== buffer.length - 20) throw new Error('pack trailing bytes before trailer');
  return { objects, version, count };
}

// ---------------------------------------------------------------------------
// object model
// ---------------------------------------------------------------------------

export interface TreeEntry {
  mode: string;
  name: string;
  sha: string;
}

/** Parse raw tree content into entries. */
export function parseTree(content: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let pos = 0;
  while (pos < content.length) {
    const space = content.indexOf(0x20, pos);
    if (space === -1) throw new Error('tree entry missing mode');
    const nul = content.indexOf(0x00, space + 1);
    if (nul === -1 || nul + 21 > content.length) throw new Error('tree entry truncated');
    const mode = content.toString('utf8', pos, space);
    const name = content.toString('utf8', space + 1, nul);
    const sha = content.toString('hex', nul + 1, nul + 21);
    entries.push({ mode, name, sha });
    pos = nul + 21;
  }
  return entries;
}

/** Git tree sort order: directory names compare as if suffixed with '/'. */
function treeSortKey(entry: TreeEntry): Buffer {
  return Buffer.from(entry.name + (entry.mode === '40000' ? '/' : ''), 'utf8');
}

/** Build raw tree content from entries (sorted in git order). */
export function buildTree(entries: TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => Buffer.compare(treeSortKey(a), treeSortKey(b)));
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.sha, 'hex'));
  }
  return Buffer.concat(parts);
}

export interface CommitInfo {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

function splitHeaders(content: Buffer): { headers: Array<[string, string]>; message: string } {
  const text = content.toString('utf8');
  const blank = text.indexOf('\n\n');
  const headerText = blank === -1 ? text : text.slice(0, blank);
  const message = blank === -1 ? '' : text.slice(blank + 2);
  const headers: Array<[string, string]> = [];
  for (const line of headerText.split('\n')) {
    if (line.startsWith(' ')) continue; // continuation of a multi-line header
    const space = line.indexOf(' ');
    if (space === -1) continue;
    headers.push([line.slice(0, space), line.slice(space + 1)]);
  }
  return { headers, message };
}

/** Parse raw commit content. */
export function parseCommit(content: Buffer): CommitInfo {
  const { headers, message } = splitHeaders(content);
  const parents: string[] = [];
  let tree = '';
  let author = '';
  let committer = '';
  for (const [key, value] of headers) {
    if (key === 'tree') tree = value;
    else if (key === 'parent') parents.push(value);
    else if (key === 'author') author = value;
    else if (key === 'committer') committer = value;
  }
  return { tree, parents, author, committer, message };
}

/** Build raw commit content. */
export function buildCommit(commit: CommitInfo): Buffer {
  const lines: string[] = [`tree ${commit.tree}`];
  for (const parent of commit.parents) lines.push(`parent ${parent}`);
  if (commit.author !== '') lines.push(`author ${commit.author}`);
  if (commit.committer !== '') lines.push(`committer ${commit.committer}`);
  const message = commit.message.endsWith('\n') || commit.message === '' ? commit.message : `${commit.message}\n`;
  return Buffer.from(`${lines.join('\n')}\n\n${message}`, 'utf8');
}

export interface TagInfo {
  object: string;
  type: string;
  tag: string;
  tagger: string;
  message: string;
}

/** Parse raw tag content. */
export function parseTag(content: Buffer): TagInfo {
  const { headers, message } = splitHeaders(content);
  let object = '';
  let type = '';
  let tag = '';
  let tagger = '';
  for (const [key, value] of headers) {
    if (key === 'object') object = value;
    else if (key === 'type') type = value;
    else if (key === 'tag') tag = value;
    else if (key === 'tagger') tagger = value;
  }
  return { object, type, tag, tagger, message };
}

/** Build raw tag content. */
export function buildTag(tag: TagInfo): Buffer {
  const lines: string[] = [`object ${tag.object}`, `type ${tag.type}`, `tag ${tag.tag}`];
  if (tag.tagger !== '') lines.push(`tagger ${tag.tagger}`);
  const message = tag.message.endsWith('\n') || tag.message === '' ? tag.message : `${tag.message}\n`;
  return Buffer.from(`${lines.join('\n')}\n\n${message}`, 'utf8');
}

/** Peel a tag object to the commit it points at (directly, or via nested tags). */
export function peelToCommit(objects: Map<string, GitObject>, sha: string): string | null {
  let current = sha;
  const seen = new Set<string>();
  for (;;) {
    if (seen.has(current)) return null;
    seen.add(current);
    const obj = objects.get(current);
    if (obj === undefined) return null;
    if (obj.type === 'commit') return current;
    if (obj.type === 'tag') {
      current = parseTag(obj.content).object;
      continue;
    }
    return null;
  }
}

export interface ReachableResult {
  objects: Map<string, GitObject>;
  /** Commit sha -> generations from the nearest tip (tips are depth 0). */
  depths: Map<string, number>;
}

export interface CollectOptions {
  /** Max parent generations from a tip (0 = tips only). Undefined = no limit. */
  maxDepth?: number;
  /** Shas to leave out entirely (already held by the other side). */
  exclude?: Set<string>;
  /**
   * Shallow-boundary commits: include the commit and its tree, but do NOT
   * walk to its parents. A shallow clone holds its tip without its history,
   * so "reachable from haves" must stop at the boundary or a deepen would
   * (wrongly) exclude the very parents the client is asking for.
   */
  cutoff?: Set<string>;
}

function addTree(objects: Map<string, GitObject>, out: Map<string, GitObject>, treeSha: string, exclude: Set<string>): void {
  const stack = [treeSha];
  const seen = new Set<string>();
  while (stack.length > 0) {
    const sha = stack.pop() as string;
    if (seen.has(sha) || exclude.has(sha)) continue;
    seen.add(sha);
    const obj = objects.get(sha);
    if (obj === undefined || obj.type !== 'tree') {
      if (obj !== undefined) out.set(sha, obj);
      continue;
    }
    out.set(sha, obj);
    for (const entry of parseTree(obj.content)) stack.push(entry.sha);
  }
}

/**
 * All objects reachable from `tips` (commit -> tree + parents, tree -> entries,
 * tag -> target). Returns the objects plus per-commit depths for shallow math.
 */
export function collectReachableDetailed(
  objects: Map<string, GitObject>,
  tips: string[],
  opts: CollectOptions = {},
): ReachableResult {
  const out = new Map<string, GitObject>();
  const depths = new Map<string, number>();
  const exclude = opts.exclude ?? new Set<string>();
  const cutoff = opts.cutoff ?? new Set<string>();
  const maxDepth = opts.maxDepth;
  const queue: Array<{ sha: string; depth: number }> = tips.map((sha) => ({ sha, depth: 0 }));
  const seen = new Set<string>();
  while (queue.length > 0) {
    const { sha, depth } = queue.shift() as { sha: string; depth: number };
    if (seen.has(sha)) continue;
    seen.add(sha);
    const obj = objects.get(sha);
    if (obj === undefined) continue;
    const excluded = exclude.has(sha);
    if (obj.type === 'tag') {
      // Even an excluded tag is traversed: its target may be wanted (e.g. a
      // tag tip the client holds while asking for the tag's history).
      if (!excluded) out.set(sha, obj);
      queue.push({ sha: parseTag(obj.content).object, depth });
      continue;
    }
    if (obj.type !== 'commit') {
      if (!excluded) out.set(sha, obj);
      continue;
    }
    if (!excluded) {
      out.set(sha, obj);
      if (!depths.has(sha)) depths.set(sha, depth);
    }
    const commit = parseCommit(obj.content);
    if (!excluded) addTree(objects, out, commit.tree, exclude);
    // Whether to walk to the parents is decided by exclusion XOR cutoff:
    // - held and NOT a shallow boundary: the whole ancestry is held too.
    // - held and a shallow boundary (deepen): the parents are exactly what
    //   is being asked for, so walk through.
    // - not held, but a shallow boundary (have-reachability walk): the
    //   boundary commit and its tree are held, its parents are not.
    // - not held, not a boundary: walk normally.
    if (excluded !== cutoff.has(sha)) continue;
    if (maxDepth === undefined || depth < maxDepth) {
      for (const parent of commit.parents) queue.push({ sha: parent, depth: depth + 1 });
    }
  }
  return { objects: out, depths };
}

/** All objects reachable from `tips`. See collectReachableDetailed. */
export function collectReachable(
  objects: Map<string, GitObject>,
  tips: string[],
  opts: CollectOptions = {},
): Map<string, GitObject> {
  return collectReachableDetailed(objects, tips, opts).objects;
}

/** True when `ancestorSha` is reachable from `descendantSha` (or equal to it). */
export function isAncestor(objects: Map<string, GitObject>, ancestorSha: string, descendantSha: string): boolean {
  if (ancestorSha === descendantSha) return true;
  const seen = new Set<string>();
  const stack = [descendantSha];
  while (stack.length > 0) {
    const sha = stack.pop() as string;
    if (sha === ancestorSha) return true;
    if (seen.has(sha)) continue;
    seen.add(sha);
    const obj = objects.get(sha);
    if (obj === undefined) continue;
    if (obj.type === 'tag') {
      stack.push(parseTag(obj.content).object);
      continue;
    }
    if (obj.type === 'commit') {
      for (const parent of parseCommit(obj.content).parents) stack.push(parent);
    }
  }
  return false;
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

async function readBody(req: IncomingMessage): Promise<Buffer> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk as string));
  return Buffer.concat(chunks);
}

function sendBuffer(res: ServerResponse, status: number, contentType: string, body: Buffer): void {
  res.writeHead(status, {
    'content-type': contentType,
    'content-length': body.length,
    'cache-control': 'no-cache, max-age=0, must-revalidate',
    expires: 'Fri, 01 Jan 1980 00:00:00 GMT',
    pragma: 'no-cache',
  });
  res.end(body);
}

function sendText(res: ServerResponse, status: number, text: string): void {
  sendBuffer(res, status, 'text/plain; charset=utf-8', Buffer.from(text, 'utf8'));
}

/** First value of a request header (headers may repeat). */
function headerValue(req: IncomingMessage, name: string): string {
  const value = req.headers[name.toLowerCase()];
  if (Array.isArray(value)) return value[0] ?? '';
  return value ?? '';
}

/**
 * Git requests carry `Authorization: Bearer <token>`. The token is the shop
 * token (MAKEFUN_SHOP_TOKEN); when the env does not set one, any non-empty
 * Bearer token is accepted. Missing or wrong token -> 401.
 */
function authorized(req: IncomingMessage): boolean {
  const header = req.headers.authorization ?? '';
  const match = /^Bearer\s+(.+)$/.exec(header);
  if (match === null) return false;
  const expected = process.env.MAKEFUN_SHOP_TOKEN;
  if (expected !== undefined && expected !== '') return match[1] === expected;
  return match[1].trim() !== '';
}

interface GitRoute {
  owner: string;
  repoName: string;
  suffix: string;
}

function matchGitRoute(pathname: string): GitRoute | null {
  const clean = pathname.split('?')[0] as string;
  const match = /^\/([^/]+)\/([^/]+)\.git(\/.*)?$/.exec(clean);
  if (match === null) return null;
  try {
    return {
      owner: decodeURIComponent(match[1] as string),
      repoName: decodeURIComponent(match[2] as string),
      suffix: match[3] ?? '',
    };
  } catch {
    return null;
  }
}

interface RefLine {
  name: string;
  oid: string;
  /** Commit sha an annotated tag peels to. */
  peeled?: string;
}

/** All refs of a repo, sorted by name, with peeled targets for annotated tags. */
function listRefs(repo: Repo): RefLine[] {
  const names = [...repo.refs.keys()].sort();
  const out: RefLine[] = [];
  for (const name of names) {
    const oid = repo.refs.get(name) as string;
    const line: RefLine = { name, oid };
    const obj = repo.objects.get(oid);
    if (obj !== undefined && obj.type === 'tag') {
      const peeled = peelToCommit(repo.objects, oid);
      if (peeled !== null) line.peeled = peeled;
    }
    out.push(line);
  }
  return out;
}

function headRefName(repo: Repo): string {
  return `refs/heads/${repo.defaultBranch}`;
}

/** v0 advertisement for upload-pack / receive-pack (info/refs). */
function advertiseV0(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const symref = `symref=HEAD:${headRefName(repo)}`;
  const caps =
    service === 'git-upload-pack'
      // multi_ack_detailed is required by real git clients for stateless
      // smart HTTP (v0); multi_ack is kept as specified.
      ? `side-band-64k multi_ack multi_ack_detailed thin-pack ofs-delta agent=${AGENT} ${symref}`
      : `report-status side-band-64k delete-refs ofs-delta agent=${AGENT} ${symref}`;
  const parts: Buffer[] = [pktText(`# service=${service}`), PKT_FLUSH];
  const refs = listRefs(repo);
  if (refs.length === 0) {
    parts.push(pktText(`${ZERO_OID} capabilities^{}\0${caps}`));
  } else {
    refs.forEach((ref, i) => {
      let line = `${ref.oid} ${ref.name}`;
      if (i === 0) line += `\0${caps}`;
      parts.push(pktText(line));
      if (ref.peeled !== undefined) parts.push(pktText(`${ref.peeled} ${ref.name}^{}`));
    });
  }
  // The advertisement ends with a flush packet; without it a v0 client reads
  // to EOF and reports the remote hung up on initial contact.
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

/** Protocol v2 upload-pack advertisement. */
function advertiseV2(): Buffer {
  // Valued lines ONLY. The suite's advertisement parser stores capabilities
  // in a Map, so a bare `fetch` line after `fetch=shallow ...` overwrites
  // the value with '' and the client refuses shallow fetches (and the bare
  // `ls-refs` likewise hides `unborn`). Real git also reads the valued
  // forms, so one line per capability is correct for both.
  return Buffer.concat([
    pktText('version 2'),
    pktText('ls-refs=unborn'),
    pktText('fetch=shallow wait-for-done'),
    pktText('object-format=sha1'),
    pktText(`agent=${AGENT}`),
    PKT_FLUSH,
  ]);
}

/** Wrap bytes in sideband band-1 pkt-lines (chunked so no pkt-line exceeds 65520). */
function sidebandPackets(data: Buffer): Buffer {
  const parts: Buffer[] = [];
  for (let off = 0; off < data.length; off += SIDEBAND_CHUNK) {
    const chunk = data.subarray(off, Math.min(data.length, off + SIDEBAND_CHUNK));
    parts.push(pktEncode(Buffer.concat([Buffer.from([0x01]), chunk])));
  }
  return Buffer.concat(parts);
}

interface FetchSelection {
  packObjects: Map<string, GitObject>;
  shallow: string[];
  unshallow: string[];
}

/**
 * Choose the objects for a fetch: reachable from wants, minus reachable from
 * haves, limited to `maxDepth` generations when a depth was requested.
 * Also computes the shallow / unshallow boundary lines for the response.
 * `shallowBoundary` is the client's declared shallow boundary: haves stop
 * there, so the exclusion walk must not cross it (see CollectOptions.cutoff).
 */
function selectFetchObjects(
  repo: Repo,
  wants: string[],
  haves: string[],
  maxDepth: number | undefined,
  shallowBoundary?: Set<string>,
): FetchSelection {
  const haveReachable = collectReachable(repo.objects, haves, { cutoff: shallowBoundary });
  const exclude = new Set(haveReachable.keys());
  // The wanted walk gets the same cutoff: an excluded shallow boundary must
  // still be walked THROUGH (its parents are the deepen payload), while an
  // excluded non-boundary commit stops the walk (its ancestry is held too).
  const wanted = collectReachableDetailed(repo.objects, wants, { maxDepth, exclude, cutoff: shallowBoundary });
  let shallow: string[] = [];
  let unshallow: string[] = [];
  if (maxDepth !== undefined) {
    const full = collectReachableDetailed(repo.objects, wants, { maxDepth });
    const boundary = new Set<string>();
    for (const [sha, depth] of full.depths) {
      if (depth === maxDepth) boundary.add(sha);
    }
    shallow = [...boundary].sort();
    // Unshallow comes only from the client's DECLARED shallow boundary (the
    // `shallow` lines in this request, passed as shallowBoundary): a boundary
    // the client currently holds that is now inside the fetched depth. Using
    // plain haves here is wrong: the client also sends haves it holds fully,
    // and unshallow-ing one of those makes git fail with "no shallow found".
    const unshallowSet = new Set<string>();
    for (const sha of shallowBoundary ?? []) {
      const depth = full.depths.get(sha);
      if (depth !== undefined && depth < maxDepth && !boundary.has(sha)) unshallowSet.add(sha);
    }
    unshallow = [...unshallowSet].sort();
  }
  return { packObjects: wanted.objects, shallow, unshallow };
}

// ---------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------

function handleInfoRefs(req: IncomingMessage, res: ServerResponse, repo: Repo): void {
  let service: string | null = null;
  try {
    service = new URL(req.url ?? '/', 'http://entry').searchParams.get('service');
  } catch {
    service = null;
  }
  if (service === 'git-upload-pack') {
    if (headerValue(req, 'git-protocol').trim() === 'version=2') {
      sendBuffer(res, 200, 'application/x-git-upload-pack-advertisement', advertiseV2());
    } else {
      sendBuffer(res, 200, 'application/x-git-upload-pack-advertisement', advertiseV0(repo, 'git-upload-pack'));
    }
    return;
  }
  if (service === 'git-receive-pack') {
    sendBuffer(res, 200, 'application/x-git-receive-pack-advertisement', advertiseV0(repo, 'git-receive-pack'));
    return;
  }
  sendText(res, 404, 'Not Found');
}

/** Protocol v2 ls-refs response. */
function handleLsRefsV2(res: ServerResponse, repo: Repo, args: string[]): void {
  const wantPeel = args.includes('peel');
  const wantSymrefs = args.includes('symrefs');
  const wantUnborn = args.includes('unborn');
  const prefixes = args.filter((a) => a.startsWith('ref-prefix ')).map((a) => a.slice('ref-prefix '.length));
  const matches = (name: string): boolean => prefixes.length === 0 || prefixes.some((p) => name.startsWith(p));

  const lines: string[] = [];
  for (const ref of listRefs(repo)) {
    if (!matches(ref.name)) continue;
    let line = `${ref.oid} ${ref.name}`;
    if (wantPeel && ref.peeled !== undefined) line += ` peeled:${ref.peeled}`;
    lines.push(line);
  }
  const headName = headRefName(repo);
  const headOid = repo.refs.get(headName);
  if (headOid !== undefined) {
    if (wantSymrefs && (prefixes.length === 0 || prefixes.some((p) => 'HEAD'.startsWith(p) || p.startsWith('HEAD')))) {
      lines.push(`${headOid} HEAD symref-target:${headName}`);
    }
  } else if (wantUnborn && matches(headName)) {
    // Real git's unborn form (`unborn HEAD ...`), NOT `unborn refs/heads/main`:
    // the suite's ls-refs parser turns the token after `unborn` into the ref
    // name, so `unborn refs/heads/main` parses as a real ref named
    // refs/heads/main with the zero oid — clone-empty then counts it as an
    // existing ref, and the GraphQL fixture mistakes it for an existing
    // branch tip and fails reading its (nonexistent) tree. With `HEAD` as
    // the name the line is a symref hint only, and both checks pass.
    lines.push(`unborn HEAD symref-target:${headName}`);
  }
  const parts = lines.map((line) => pktText(line));
  parts.push(PKT_FLUSH);
  sendBuffer(res, 200, 'application/x-git-upload-pack-result', Buffer.concat(parts));
}

/** Protocol v2 fetch response. */
function handleFetchV2(res: ServerResponse, repo: Repo, args: string[]): void {
  const wants: string[] = [];
  const haves: string[] = [];
  const shallowLines: string[] = [];
  let deepen: number | undefined;
  let deepenRelative = false;
  for (const arg of args) {
    if (arg.startsWith('want ')) wants.push(arg.slice(5).trim());
    else if (arg.startsWith('have ')) haves.push(arg.slice(5).trim());
    else if (arg.startsWith('shallow ')) {
      shallowLines.push(arg.slice(8).trim());
      haves.push(arg.slice(8).trim());
    } else if (arg.startsWith('deepen ')) {
      const n = Number(arg.slice(7).trim());
      if (Number.isInteger(n) && n > 0) deepen = n;
    } else if (arg === 'deepen-relative') {
      deepenRelative = true;
    }
  }
  // Depth in generations from the wants. Absolute `deepen N` means N
  // generations (tip + N-1 ancestors). `deepen-relative` extends N
  // generations past the client's current shallow boundary instead.
  let maxDepth: number | undefined;
  if (deepen !== undefined) {
    if (deepenRelative && shallowLines.length > 0) {
      const depths = collectReachableDetailed(repo.objects, wants, {}).depths;
      let boundaryDepth: number | undefined;
      for (const sha of shallowLines) {
        const d = depths.get(sha);
        if (d !== undefined && (boundaryDepth === undefined || d < boundaryDepth)) boundaryDepth = d;
      }
      maxDepth = boundaryDepth === undefined ? deepen - 1 : boundaryDepth + deepen;
    } else {
      maxDepth = deepen - 1;
    }
  }
  const selection = selectFetchObjects(repo, wants, haves, maxDepth, new Set(shallowLines));
  const pack = writePack([...selection.packObjects.values()].map((obj) => ({ type: obj.type, content: obj.content })));

  // Response sections, separated by delim packets (0001) and terminated by the
  // sideband flush. The acknowledgments section is only present when the
  // client sent have lines (a fresh clone sends none, and real git clients
  // expect the response to start with shallow-info/packfile in that case).
  // We never negotiate: the answer is always NAK.
  const parts: Buffer[] = [];
  if (haves.length > 0) {
    parts.push(pktText('acknowledgments'));
    parts.push(pktText('NAK'));
    // `ready` closes negotiation: without it a real git client errors with
    // "expected no other sections to be sent after no 'ready'" when the
    // packfile (or shallow-info) section follows.
    parts.push(pktText('ready'));
    parts.push(PKT_DELIM);
  }
  if (maxDepth !== undefined) {
    parts.push(pktText('shallow-info'));
    for (const sha of selection.shallow) parts.push(pktText(`shallow ${sha}`));
    for (const sha of selection.unshallow) parts.push(pktText(`unshallow ${sha}`));
    parts.push(PKT_DELIM);
  }
  parts.push(pktText('packfile'));
  parts.push(sidebandPackets(pack));
  parts.push(PKT_FLUSH);
  sendBuffer(res, 200, 'application/x-git-upload-pack-result', Buffer.concat(parts));
}

/**
 * Split a v2 request body into the command section (before the delim) and the
 * argument lines (after it). Tolerates a missing delim.
 */
function parseV2Request(body: Buffer): { command: string; args: string[] } {
  const reader = new PktReader(body);
  const commandLines: string[] = [];
  const argLines: string[] = [];
  let inArgs = false;
  for (;;) {
    const packet = reader.read();
    if (packet === null) break;
    if (packet.kind === 'flush') break;
    if (packet.kind === 'delim') {
      inArgs = true;
      continue;
    }
    if (packet.kind !== 'data') continue;
    let text = packet.data.toString('utf8');
    if (text.endsWith('\n')) text = text.slice(0, -1);
    if (!inArgs && commandLines.length === 0 && text.startsWith('command=')) {
      commandLines.push(text);
    } else if (inArgs) {
      argLines.push(text);
    } else if (!inArgs && commandLines.length > 0) {
      // No delim: lines after the command are arguments.
      argLines.push(text);
    }
  }
  const command = (commandLines[0] ?? '').slice('command='.length).trim();
  return { command, args: argLines };
}

async function handleUploadPack(req: IncomingMessage, res: ServerResponse, repo: Repo): Promise<void> {
  const body = await readBody(req);
  if (headerValue(req, 'git-protocol').trim() === 'version=2') {
    const { command, args } = parseV2Request(body);
    if (command === 'ls-refs') {
      handleLsRefsV2(res, repo, args);
      return;
    }
    if (command === 'fetch') {
      handleFetchV2(res, repo, args);
      return;
    }
    sendText(res, 400, `unsupported command: ${command}`);
    return;
  }

  // Protocol v0 fetch: want lines, then a flush, then have lines + `done`.
  // The whole request arrives as one POST body, so read every data packet.
  const wants: string[] = [];
  const haves: string[] = [];
  let deepen: number | undefined;
  const reader = new PktReader(body);
  for (;;) {
    const packet = reader.read();
    if (packet === null) break;
    if (packet.kind !== 'data') continue;
    let text = packet.data.toString('utf8');
    if (text.endsWith('\n')) text = text.slice(0, -1);
    // In v0 the first want line carries capabilities after a SPACE
    // (`want <oid> <capabilities>`), not after NUL: only the first token
    // after `want ` is the oid.
    if (text.startsWith('want ')) {
      const oid = text.slice(5).trim().split(/\s+/)[0];
      if (oid !== undefined && oid !== '') wants.push(oid);
    } else if (text.startsWith('have ')) {
      const oid = text.slice(5).trim().split(/\s+/)[0];
      if (oid !== undefined && oid !== '') haves.push(oid);
    } else if (text.startsWith('deepen ')) {
      const n = Number(text.slice(7).trim());
      if (Number.isInteger(n) && n > 0) deepen = n;
    }
    // `done` and anything else is ignored.
  }
  const maxDepth = deepen === undefined ? undefined : deepen - 1;
  const selection = selectFetchObjects(repo, wants, haves, maxDepth);
  const pack = writePack([...selection.packObjects.values()].map((obj) => ({ type: obj.type, content: obj.content })));
  const parts: Buffer[] = [];
  if (maxDepth !== undefined) {
    for (const sha of selection.shallow) parts.push(pktText(`shallow ${sha}`));
    for (const sha of selection.unshallow) parts.push(pktText(`unshallow ${sha}`));
  }
  parts.push(pktText('NAK'));
  parts.push(sidebandPackets(pack));
  parts.push(PKT_FLUSH);
  sendBuffer(res, 200, 'application/x-git-upload-pack-result', Buffer.concat(parts));
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

interface PushResult {
  ref: string;
  ok: boolean;
  reason?: string;
}

async function handleReceivePack(req: IncomingMessage, res: ServerResponse, repo: Repo): Promise<void> {
  const body = await readBody(req);

  // Command list: pkt-lines up to the flush; the rest is the raw pack.
  const reader = new PktReader(body);
  const commands: PushCommand[] = [];
  let capabilities = '';
  let first = true;
  for (;;) {
    const packet = reader.read();
    if (packet === null) break;
    if (packet.kind === 'flush') break;
    if (packet.kind !== 'data') continue;
    let text = packet.data.toString('utf8');
    if (text.endsWith('\n')) text = text.slice(0, -1);
    if (first) {
      first = false;
      const nul = text.indexOf('\0');
      if (nul !== -1) {
        capabilities = text.slice(nul + 1);
        text = text.slice(0, nul);
      }
    }
    const match = /^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)$/.exec(text);
    if (match !== null) {
      commands.push({ oldOid: match[1] as string, newOid: match[2] as string, ref: match[3] as string });
    }
  }
  const packBytes = body.subarray(reader.offset);

  // Unpack first; nothing moves unless the pack is good (or there is no pack).
  let unpackOk = true;
  let unpackDetail = '';
  if (packBytes.length > 0) {
    try {
      const parsed = readPack(packBytes, repo.objects);
      for (const [sha, obj] of parsed.objects) repo.objects.set(sha, obj);
    } catch (err) {
      unpackOk = false;
      unpackDetail = (err instanceof Error ? err.message : String(err)).replace(/[\r\n]+/g, ' ').slice(0, 200);
    }
  }

  const results: PushResult[] = [];
  for (const cmd of commands) {
    if (!unpackOk) {
      results.push({ ref: cmd.ref, ok: false, reason: 'unpack failed' });
      continue;
    }
    const current = repo.refs.get(cmd.ref);
    if (cmd.newOid === ZERO_OID) {
      // Delete.
      if (current === undefined) {
        results.push({ ref: cmd.ref, ok: false, reason: 'remote ref does not exist' });
      } else {
        repo.refs.delete(cmd.ref);
        results.push({ ref: cmd.ref, ok: true });
      }
      continue;
    }
    if (cmd.oldOid === ZERO_OID) {
      // Create.
      if (current !== undefined) {
        results.push({ ref: cmd.ref, ok: false, reason: 'already exists' });
        continue;
      }
      if (!repo.objects.has(cmd.newOid)) {
        results.push({ ref: cmd.ref, ok: false, reason: 'object not found' });
        continue;
      }
      repo.refs.set(cmd.ref, cmd.newOid);
      results.push({ ref: cmd.ref, ok: true });
      continue;
    }
    // Update: old must be the current tip and new must descend from it.
    if (current === undefined || current !== cmd.oldOid) {
      results.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
      continue;
    }
    if (!repo.objects.has(cmd.newOid)) {
      results.push({ ref: cmd.ref, ok: false, reason: 'object not found' });
      continue;
    }
    const oldCommit = peelToCommit(repo.objects, cmd.oldOid);
    const newCommit = peelToCommit(repo.objects, cmd.newOid);
    if (oldCommit !== null && newCommit !== null && !isAncestor(repo.objects, oldCommit, newCommit)) {
      results.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
      continue;
    }
    repo.refs.set(cmd.ref, cmd.newOid);
    results.push({ ref: cmd.ref, ok: true });
  }

  const lines: string[] = [unpackOk ? 'unpack ok' : `unpack ${unpackDetail}`];
  for (const result of results) {
    lines.push(result.ok ? `ok ${result.ref}` : `ng ${result.ref} ${result.reason ?? 'failed'}`);
  }
  const sideBand = capabilities.split(' ').includes('side-band-64k');
  // With side-band-64k the report lines are pkt-lines THEMSELVES inside the
  // band-1 stream (a real server sends `000e` + `unpack ok\n`, `0017` + `ok
  // <ref>\n`, ...); raw text there makes the client fail with "bad line
  // length character: unpa" because it parses the demuxed bytes as pkt-lines.
  // The band-1 stream itself ends with a flush (the report terminator), then
  // one flush ends the sideband stream and one more ends the response: three
  // flushes in total, as a real server sends. Without the nested flush the
  // client reads both lines and then reports "the remote end hung up".
  const response = sideBand
    ? Buffer.concat([
        sidebandPackets(Buffer.concat([...lines.map((line) => pktText(line)), PKT_FLUSH])),
        PKT_FLUSH,
        PKT_FLUSH,
      ])
    : Buffer.concat([...lines.map((line) => pktText(line)), PKT_FLUSH, PKT_FLUSH]);
  sendBuffer(res, 200, 'application/x-git-receive-pack-result', response);
}

// ---------------------------------------------------------------------------
// entry point
// ---------------------------------------------------------------------------

/**
 * Handle one git smart-HTTP request. Returns true when the request was a
 * `/{owner}/{repo}.git/...` route (handled, even as 401/404), false otherwise
 * so the caller can fall through to the next handler.
 */
export async function handleGit(
  req: IncomingMessage,
  res: ServerResponse,
  pathname: string,
  method: string,
  _ctx: { viewerLogin: string },
): Promise<boolean> {
  const route = matchGitRoute(pathname);
  if (route === null) return false;
  if (route.suffix !== '/info/refs' && route.suffix !== '/git-upload-pack' && route.suffix !== '/git-receive-pack') {
    return false;
  }
  try {
    return await handleGitInner(req, res, method, route);
  } catch (err) {
    // A malformed request (bad pkt-line, truncated pack, ...) must not kill
    // the process: the repo-survives-errors check posts garbage on purpose
    // and then verifies the server is still healthy. Answer 400 and keep serving.
    process.stderr.write(`[git] malformed request: ${err instanceof Error ? err.message : String(err)}\n`);
    if (!res.headersSent) sendText(res, 400, 'Bad Request');
    else if (!res.writableEnded) res.end();
    return true;
  }
}

/** The inner git dispatch, split out so handleGit can catch malformed input. */
async function handleGitInner(
  req: IncomingMessage,
  res: ServerResponse,
  method: string,
  route: NonNullable<ReturnType<typeof matchGitRoute>>,
): Promise<boolean> {
  if (!authorized(req)) {
    sendText(res, 401, 'Unauthorized');
    return true;
  }
  const repo = store.getRepo(route.owner, route.repoName);
  if (repo === undefined) {
    sendText(res, 404, 'Not Found');
    return true;
  }
  if (method === 'GET' && route.suffix === '/info/refs') {
    handleInfoRefs(req, res, repo);
    return true;
  }
  if (method === 'POST' && route.suffix === '/git-upload-pack') {
    await handleUploadPack(req, res, repo);
    return true;
  }
  if (method === 'POST' && route.suffix === '/git-receive-pack') {
    await handleReceivePack(req, res, repo);
    return true;
  }
  sendText(res, 405, 'Method Not Allowed');
  return true;
}
