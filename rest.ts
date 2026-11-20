// REST v3 surface for the git forge (github-core-clone-01).
//
// Owns every route under /api/v3. All state lives in the shared store
// (store.ts), so the git smart-HTTP, GraphQL and MCP surfaces see exactly
// the same repos, objects, issues and pulls this handler creates.
//
// Contract with the coordinator (app.ts):
//   handleRest(req, res, pathname, method, { viewerLogin }) -> true when this
//   handler answered the request, false when the path is not a REST route it
//   owns (so the coordinator can try other handlers / fall through to 404).
//   `pathname` is percent-decoded already; contents paths keep '/' inside.
//
// Invariants:
//   - Every response is application/json and carries a non-empty
//     X-GitHub-Request-Id header (req-<counter>, unique per request).
//   - Every error body is { message: <non-empty string> }.
//   - Every /api/v3 route requires `Authorization: Bearer <token>` where the
//     token is process.env.MAKEFUN_SHOP_TOKEN (read at request time).
//     Missing/wrong token -> 401 { message }. (/healthz is not a REST route
//     and is not handled here at all.)
//   - A repo belongs to the login the token names (ctx.viewerLogin); the same
//     repo name under another owner -> 404, never 403.
//   - Deterministic: commit epochs derive from store.tick(), merged_at is a
//     fixed ISO string, ids derive from counters. No wall clock, no random.

import type { IncomingMessage, ServerResponse } from 'node:http';
import {
  store,
  gitObjectId,
  globalId,
  type GitObject,
  type Issue,
  type Label,
  type Pull,
  type PullFile,
  type Repo,
} from './store.ts';

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

let requestCounter = 0;

const JSON_TYPE = 'application/json; charset=utf-8';
const MERGED_AT = '2025-01-01T00:00:00.000Z';
const COMMIT_EPOCH_BASE = 1_700_000_000;

function nextRequestId(): string {
  requestCounter += 1;
  return `req-${requestCounter}`;
}

function sendJson(res: ServerResponse, status: number, body: unknown, extraHeaders: Readonly<Record<string, string>> = {}): void {
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': Buffer.byteLength(text),
    'x-github-request-id': nextRequestId(),
    ...extraHeaders,
  });
  res.end(text);
}

function sendNoContent(res: ServerResponse): void {
  res.writeHead(204, {
    'content-type': JSON_TYPE,
    'x-github-request-id': nextRequestId(),
  });
  res.end();
}

function sendError(res: ServerResponse, status: number, message: string): void {
  sendJson(res, status, { message: message === '' ? 'Error' : message });
}

/** Read the request body. Empty body -> {}. Malformed JSON -> ok:false. */
async function readJsonBody(req: IncomingMessage): Promise<{ ok: true; value: Record<string, unknown> } | { ok: false }> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) {
    chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk as string));
  }
  const text = Buffer.concat(chunks).toString('utf8').trim();
  if (text === '') return { ok: true, value: {} };
  try {
    const parsed: unknown = JSON.parse(text);
    if (parsed !== null && typeof parsed === 'object' && !Array.isArray(parsed)) {
      return { ok: true, value: parsed as Record<string, unknown> };
    }
    // A JSON array / scalar is not a valid object body for these routes.
    return { ok: false };
  } catch {
    return { ok: false };
  }
}

function parseQuery(req: IncomingMessage): URLSearchParams {
  try {
    return new URL(req.url ?? '/', 'http://entry').searchParams;
  } catch {
    return new URLSearchParams();
  }
}

function originOf(req: IncomingMessage): string {
  const host = req.headers.host ?? 'localhost';
  return `http://${host}`;
}

function intParam(q: URLSearchParams, name: string, fallback: number): number {
  const raw = q.get(name);
  if (raw === null) return fallback;
  const n = Number.parseInt(raw, 10);
  if (!Number.isFinite(n) || n < 1) return fallback;
  return n;
}

/** Pagination: returns the page slice and a Link header value ('' when no next page). */
function paginate<T>(items: T[], q: URLSearchParams, req: IncomingMessage): { page: T[]; link: string } {
  const perPage = Math.min(100, intParam(q, 'per_page', 30));
  const pageNum = intParam(q, 'page', 1);
  const start = (pageNum - 1) * perPage;
  const page = items.slice(start, start + perPage);
  let link = '';
  if (start + perPage < items.length) {
    const url = new URL(req.url ?? '/', originOf(req));
    url.searchParams.set('per_page', String(perPage));
    url.searchParams.set('page', String(pageNum + 1));
    link = `<${url.toString()}>; rel="next"`;
  }
  return { page, link };
}

function withLink(link: string): Record<string, string> {
  return link === '' ? {} : { link };
}

// ---------------------------------------------------------------------------
// Git object helpers (exported: the git smart-HTTP surface may reuse them)
// ---------------------------------------------------------------------------

export interface TreeEntry {
  mode: string; // '100644' file | '40000' dir
  name: string;
  sha: string; // hex sha1
}

/** Git's tree sort order: directory names sort as if they ended with '/'. */
function treeSortKey(e: TreeEntry): string {
  return e.mode === '40000' ? `${e.name}/` : e.name;
}

export function parseTree(content: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let i = 0;
  while (i < content.length) {
    const space = content.indexOf(0x20, i);
    if (space < 0) break;
    const nul = content.indexOf(0x00, space + 1);
    if (nul < 0 || nul + 21 > content.length) break;
    const mode = content.subarray(i, space).toString('utf8');
    const name = content.subarray(space + 1, nul).toString('utf8');
    const sha = content.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, sha });
    i = nul + 21;
  }
  return entries;
}

export function serializeTree(entries: TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => (treeSortKey(a) < treeSortKey(b) ? -1 : treeSortKey(a) > treeSortKey(b) ? 1 : 0));
  const parts: Buffer[] = [];
  for (const e of sorted) {
    parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'));
    parts.push(Buffer.from(e.sha, 'hex'));
  }
  return Buffer.concat(parts);
}

function storeObject(repo: Repo, type: GitObject['type'], content: Buffer): string {
  const sha = gitObjectId(type, content);
  repo.objects.set(sha, { type, content });
  return sha;
}

function getTree(repo: Repo, treeSha: string | null): TreeEntry[] {
  if (treeSha === null) return [];
  const obj = repo.objects.get(treeSha);
  if (obj === undefined || obj.type !== 'tree') return [];
  return parseTree(obj.content);
}

/**
 * Return the new tree sha after setting `path` -> `blobSha` in the tree
 * rooted at `parentTreeSha` (null = empty tree), creating intermediate
 * trees as needed. `blobSha` null removes the path instead.
 */
export function buildTree(repo: Repo, parentTreeSha: string | null, path: string, blobSha: string | null): string {
  const segments = path.split('/').filter((s) => s !== '');
  const setIn = (treeSha: string | null, idx: number): string => {
    const entries = getTree(repo, treeSha);
    const name = segments[idx] as string;
    const last = idx === segments.length - 1;
    const existing = entries.findIndex((e) => e.name === name);
    if (last) {
      if (blobSha === null) {
        if (existing >= 0) entries.splice(existing, 1);
      } else if (existing >= 0) {
        entries[existing] = { mode: '100644', name, sha: blobSha };
      } else {
        entries.push({ mode: '100644', name, sha: blobSha });
      }
    } else {
      const childSha = existing >= 0 && entries[existing]?.mode === '40000' ? (entries[existing] as TreeEntry).sha : null;
      const newChildSha = setIn(childSha, idx + 1);
      if (existing >= 0) {
        entries[existing] = { mode: '40000', name, sha: newChildSha };
      } else {
        entries.push({ mode: '40000', name, sha: newChildSha });
      }
    }
    return storeObject(repo, 'tree', serializeTree(entries));
  };
  return setIn(parentTreeSha, 0);
}

/** Build a commit object; returns { sha, content }. Epoch is deterministic. */
export function commitObject(
  treeSha: string,
  parentSha: string | null,
  message: string,
  authorLogin: string,
  epoch: number,
): { sha: string; content: Buffer } {
  const ident = `${authorLogin} <${authorLogin}@example.invalid> ${epoch} +0000`;
  const lines = [`tree ${treeSha}`];
  if (parentSha !== null) lines.push(`parent ${parentSha}`);
  lines.push(`author ${ident}`, `committer ${ident}`, '', message.endsWith('\n') ? message : `${message}\n`);
  const content = Buffer.from(lines.join('\n'), 'utf8');
  return { sha: gitObjectId('commit', content), content };
}

export interface ParsedCommit {
  tree: string | null;
  parents: string[];
  authorName: string;
  authorEmail: string;
  authorEpoch: number;
  message: string;
}

export function parseCommit(content: Buffer): ParsedCommit {
  const text = content.toString('utf8');
  const headEnd = text.indexOf('\n\n');
  const head = headEnd >= 0 ? text.slice(0, headEnd) : text;
  const message = (headEnd >= 0 ? text.slice(headEnd + 2) : '').replace(/\n+$/, '');
  let tree: string | null = null;
  const parents: string[] = [];
  let authorName = '';
  let authorEmail = '';
  let authorEpoch = 0;
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) {
      const m = /^author (.+) <([^>]*)> (\d+) [+-]\d{4}$/.exec(line);
      if (m) {
        authorName = m[1] as string;
        authorEmail = m[2] as string;
        authorEpoch = Number(m[3]);
      }
    }
  }
  return { tree, parents, authorName, authorEmail, authorEpoch, message };
}

/** Blob sha for `path` in the tree rooted at `treeSha`, or null. */
export function getFileAtTree(repo: Repo, treeSha: string | null, path: string): string | null {
  if (treeSha === null) return null;
  const segments = path.split('/').filter((s) => s !== '');
  let current: string | null = treeSha;
  for (let i = 0; i < segments.length; i += 1) {
    if (current === null) return null;
    const entries = getTree(repo, current);
    const entry = entries.find((e) => e.name === segments[i]);
    if (entry === undefined) return null;
    if (i === segments.length - 1) {
      return entry.mode === '100644' ? entry.sha : null;
    }
    if (entry.mode !== '40000') return null;
    current = entry.sha;
  }
  return null;
}

/** Immediate children of `path` ('' = root) in the tree rooted at `treeSha`. */
export function listDirAtTree(
  repo: Repo,
  treeSha: string | null,
  path: string,
): { name: string; path: string; sha: string; type: 'file' | 'dir' }[] {
  if (treeSha === null) return [];
  const segments = path.split('/').filter((s) => s !== '');
  let current: string | null = treeSha;
  for (const seg of segments) {
    if (current === null) return [];
    const entry: TreeEntry | undefined = getTree(repo, current).find((e) => e.name === seg);
    if (entry === undefined || entry.mode !== '40000') return [];
    current = entry.sha;
  }
  if (current === null) return [];
  return getTree(repo, current).map((e) => ({
    name: e.name,
    path: segments.length === 0 ? e.name : `${segments.join('/')}/${e.name}`,
    sha: e.sha,
    type: e.mode === '40000' ? ('dir' as const) : ('file' as const),
  }));
}

/** Commits reachable from `tipSha`, newest first (depth-first, parents in order). */
export function listCommits(repo: Repo, tipSha: string | null): { sha: string; commit: ParsedCommit }[] {
  const out: { sha: string; commit: ParsedCommit }[] = [];
  const seen = new Set<string>();
  const visit = (sha: string | null): void => {
    if (sha === null || seen.has(sha)) return;
    seen.add(sha);
    const obj = repo.objects.get(sha);
    if (obj === undefined || obj.type !== 'commit') return;
    const parsed = parseCommit(obj.content);
    out.push({ sha, commit: parsed });
    for (const parent of parsed.parents) visit(parent);
  };
  visit(tipSha);
  return out;
}

/** Flatten a tree to path -> blob sha (files only). */
function flattenTree(repo: Repo, treeSha: string | null, prefix: string, out: Map<string, string>): void {
  if (treeSha === null) return;
  for (const e of getTree(repo, treeSha)) {
    const p = prefix === '' ? e.name : `${prefix}/${e.name}`;
    if (e.mode === '40000') flattenTree(repo, e.sha, p, out);
    else out.set(p, e.sha);
  }
}

function countLines(buf: Buffer): number {
  if (buf.length === 0) return 0;
  const parts = buf.toString('utf8').split('\n');
  if (parts.length > 0 && parts[parts.length - 1] === '') parts.pop();
  return parts.length;
}

function diffTrees(repo: Repo, baseTreeSha: string | null, headTreeSha: string | null): PullFile[] {
  const base = new Map<string, string>();
  const head = new Map<string, string>();
  flattenTree(repo, baseTreeSha, '', base);
  flattenTree(repo, headTreeSha, '', head);
  const files: PullFile[] = [];
  const paths = new Set([...base.keys(), ...head.keys()]);
  for (const path of [...paths].sort()) {
    const b = base.get(path);
    const h = head.get(path);
    if (b === h) continue;
    if (b === undefined && h !== undefined) {
      const additions = countLines(repo.objects.get(h)?.content ?? Buffer.alloc(0));
      files.push({ filename: path, status: 'added', additions, deletions: 0, changes: additions });
    } else if (h === undefined && b !== undefined) {
      const deletions = countLines(repo.objects.get(b)?.content ?? Buffer.alloc(0));
      files.push({ filename: path, status: 'removed', additions: 0, deletions, changes: deletions });
    } else if (b !== undefined && h !== undefined) {
      const additions = countLines(repo.objects.get(h)?.content ?? Buffer.alloc(0));
      const deletions = countLines(repo.objects.get(b)?.content ?? Buffer.alloc(0));
      files.push({ filename: path, status: 'modified', additions, deletions, changes: additions + deletions });
    }
  }
  return files;
}

/** Resolve a ref-ish (branch name, full ref, or commit sha) to a commit sha. */
function resolveRef(repo: Repo, ref: string): string | null {
  if (repo.refs.has(ref)) return repo.refs.get(ref) as string;
  const full = `refs/heads/${ref}`;
  if (repo.refs.has(full)) return repo.refs.get(full) as string;
  const obj = repo.objects.get(ref);
  if (obj !== undefined && obj.type === 'commit') return ref;
  return null;
}

function tipOfBranch(repo: Repo, branch: string): string | null {
  return repo.refs.get(`refs/heads/${branch}`) ?? null;
}

// ---------------------------------------------------------------------------
// Serializers
// ---------------------------------------------------------------------------

function isoFromCounter(counter: number): string {
  return new Date((COMMIT_EPOCH_BASE + counter) * 1000).toISOString();
}

function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: globalId('Repository', repo.id),
    name: repo.name,
    full_name: repo.fullName,
    owner: { login: repo.owner, id: 1, type: 'User' },
    private: repo.private,
    description: repo.description,
    fork: false,
    default_branch: repo.defaultBranch,
    created_at: isoFromCounter(repo.id),
    updated_at: isoFromCounter(repo.id),
    html_url: `/api/v3/repos/${repo.owner}/${repo.name}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}`,
  };
}

function labelJson(label: Label): Record<string, unknown> {
  return { name: label.name, color: label.color, description: '' };
}

function issueJson(issue: Issue, repo: Repo): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: globalId('Issue', issue.id),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => {
      const label = repo.labels.get(name);
      return label !== undefined ? labelJson(label) : { name, color: '000000' };
    }),
    user: { login: repo.owner, id: 1, type: 'User' },
    created_at: isoFromCounter(issue.createdAt),
    updated_at: isoFromCounter(issue.createdAt),
    comments: issue.comments.length,
    comments_url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}/comments`,
  };
}

function pullJson(pull: Pull, repo: Repo): Record<string, unknown> {
  return {
    id: pull.id,
    node_id: globalId('PullRequest', pull.id),
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    head: { ref: pull.head.ref, sha: pull.head.sha, repo: { full_name: repo.fullName } },
    base: { ref: pull.base.ref, sha: pull.base.sha, repo: { full_name: repo.fullName } },
    user: { login: repo.owner, id: 1, type: 'User' },
    created_at: isoFromCounter(pull.createdAt),
    updated_at: isoFromCounter(pull.createdAt),
  };
}

function commentJson(comment: { id: number; body: string; createdAt: number }, repo: Repo): Record<string, unknown> {
  return {
    id: comment.id,
    node_id: globalId('IssueComment', comment.id),
    body: comment.body,
    user: { login: repo.owner, id: 1, type: 'User' },
    created_at: isoFromCounter(comment.createdAt),
    updated_at: isoFromCounter(comment.createdAt),
  };
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

export async function handleRest(
  req: IncomingMessage,
  res: ServerResponse,
  pathname: string,
  method: string,
  ctx: { viewerLogin: string },
): Promise<boolean> {
  const segments = pathname.split('/').filter((s) => s !== '');
  // Not a REST route we own: /healthz and anything outside /api/v3.
  if (segments.length < 2 || segments[0] !== 'api' || segments[1] !== 'v3') return false;

  const viewerLogin = ctx.viewerLogin !== '' ? ctx.viewerLogin : store.viewerLogin;

  // Auth: every /api/v3 route requires the bearer token (read at request time).
  const token = process.env.MAKEFUN_SHOP_TOKEN ?? '';
  const auth = req.headers.authorization ?? '';
  if (token === '' || auth !== `Bearer ${token}`) {
    sendError(res, 401, 'Bad credentials');
    return true;
  }

  const rest = segments.slice(2); // segments after /api/v3
  const q = parseQuery(req);

  const bodyNeeded = method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE';
  let body: Record<string, unknown> = {};
  if (bodyNeeded) {
    const read = await readJsonBody(req);
    if (!read.ok) {
      sendError(res, 400, 'Problems parsing JSON');
      return true;
    }
    body = read.value;
  }

  const str = (v: unknown): string | null => (typeof v === 'string' ? v : null);
  const bool = (v: unknown, fallback: boolean): boolean => (typeof v === 'boolean' ? v : fallback);

  // ---- API root ------------------------------------------------------------
  if (rest.length === 0) {
    if (method !== 'GET') {
      sendError(res, 404, 'Not Found');
      return true;
    }
    sendJson(
      res,
      200,
      {
        current_user_url: '/api/v3/user',
        rate_limit_url: '/api/v3/rate_limit',
        user_url: '/api/v3/users/{user}',
        repository_url: '/api/v3/repos/{owner}/{repo}',
      },
      { 'x-oauth-scopes': 'repo, read:org' },
    );
    return true;
  }

  // ---- /meta ---------------------------------------------------------------
  if (rest.length === 1 && rest[0] === 'meta') {
    if (method !== 'GET') {
      sendError(res, 404, 'Not Found');
      return true;
    }
    sendJson(res, 200, { installed_version: '1.0.0' });
    return true;
  }

  // ---- /user ---------------------------------------------------------------
  if (rest.length === 1 && rest[0] === 'user' && method === 'GET') {
    sendJson(res, 200, { login: viewerLogin, id: 1, type: 'User' });
    return true;
  }

  // ---- POST /user/repos + GET /user/repos ----------------------------------
  if (rest.length === 2 && rest[0] === 'user' && rest[1] === 'repos') {
    if (method === 'POST') {
      const name = str(body.name);
      if (name === null || name === '' || name.includes('/')) {
        sendError(res, 422, 'Repository name is required');
        return true;
      }
      if (store.getRepo(viewerLogin, name) !== undefined) {
        sendError(res, 422, 'Repository already exists');
        return true;
      }
      const repo = store.createRepo(viewerLogin, name, str(body.description) ?? '', bool(body.private, false));
      sendJson(res, 201, repoJson(repo));
      return true;
    }
    if (method === 'GET') {
      const repos = store.listReposByOwner(viewerLogin);
      const { page, link } = paginate(repos, q, req);
      sendJson(res, 200, page.map((r) => repoJson(r)), withLink(link));
      return true;
    }
    sendError(res, 404, 'Not Found');
    return true;
  }

  // ---- /repos/{owner}/{repo}... --------------------------------------------
  if (rest.length >= 3 && rest[0] === 'repos') {
    const owner = rest[1] as string;
    const repoName = rest[2] as string;
    const tail = rest.slice(3);

    // The repo belongs to the viewer; any other owner -> 404, never 403.
    // store.getRepo enforces this: it adopts the placeholder-owned repo to
    // the requested owner on first access, and returns undefined when the
    // repo belongs to a different real login.
    const repo = store.getRepo(owner, repoName);
    if (repo === undefined) {
      sendError(res, 404, 'Not Found');
      return true;
    }

    // GET/PATCH/DELETE /repos/{owner}/{repo}
    if (tail.length === 0) {
      if (method === 'GET') {
        sendJson(res, 200, repoJson(repo));
        return true;
      }
      if (method === 'PATCH') {
        const description = str(body.description);
        if (description !== null) repo.description = description;
        sendJson(res, 200, repoJson(repo));
        return true;
      }
      if (method === 'DELETE') {
        store.deleteRepo(owner, repoName);
        sendNoContent(res);
        return true;
      }
      sendError(res, 404, 'Not Found');
      return true;
    }

    // ---- git/refs ----------------------------------------------------------
    if (tail[0] === 'git' && tail[1] === 'refs') {
      if (method === 'POST' && tail.length === 2) {
        const ref = str(body.ref);
        const sha = str(body.sha);
        if (ref === null || !ref.startsWith('refs/heads/') || ref === 'refs/heads/' || sha === null) {
          sendError(res, 422, 'Invalid ref or sha');
          return true;
        }
        const obj = repo.objects.get(sha);
        if (obj === undefined || obj.type !== 'commit') {
          sendError(res, 422, 'Object does not exist');
          return true;
        }
        if (repo.refs.has(ref)) {
          sendError(res, 422, 'Reference already exists');
          return true;
        }
        repo.refs.set(ref, sha);
        sendJson(res, 201, { ref, object: { sha } });
        return true;
      }
      if (method === 'GET' && tail.length >= 4 && tail[2] === 'heads') {
        const branch = tail.slice(3).join('/');
        const sha = repo.refs.get(`refs/heads/${branch}`);
        if (sha === undefined) {
          sendError(res, 404, 'Not Found');
          return true;
        }
        sendJson(res, 200, { ref: `refs/heads/${branch}`, object: { sha } });
        return true;
      }
      sendError(res, 404, 'Not Found');
      return true;
    }

    // ---- branches ----------------------------------------------------------
    if (tail[0] === 'branches') {
      const branchNames = [...repo.refs.keys()]
        .filter((r) => r.startsWith('refs/heads/'))
        .map((r) => r.slice('refs/heads/'.length))
        .sort();
      if (method === 'GET' && tail.length === 1) {
        sendJson(
          res,
          200,
          branchNames.map((name) => ({ name, commit: { sha: repo.refs.get(`refs/heads/${name}`) as string } })),
        );
        return true;
      }
      if (method === 'GET' && tail.length >= 2) {
        const branch = tail.slice(1).join('/');
        const sha = repo.refs.get(`refs/heads/${branch}`);
        if (sha === undefined) {
          sendError(res, 404, 'Not Found');
          return true;
        }
        sendJson(res, 200, { name: branch, commit: { sha } });
        return true;
      }
      sendError(res, 404, 'Not Found');
      return true;
    }

    // ---- commits -----------------------------------------------------------
    if (tail[0] === 'commits' && tail.length === 1 && method === 'GET') {
      const refParam = q.get('sha');
      const tip = refParam !== null ? resolveRef(repo, refParam) : tipOfBranch(repo, repo.defaultBranch);
      if (tip === null) {
        sendJson(res, 200, []);
        return true;
      }
      const commits = listCommits(repo, tip);
      sendJson(
        res,
        200,
        commits.map(({ sha, commit }) => ({
          sha,
          commit: {
            message: commit.message,
            author: { name: commit.authorName, email: commit.authorEmail, date: new Date(commit.authorEpoch * 1000).toISOString() },
            committer: { name: commit.authorName, email: commit.authorEmail, date: new Date(commit.authorEpoch * 1000).toISOString() },
          },
          parents: commit.parents.map((p) => ({ sha: p })),
        })),
      );
      return true;
    }

    // ---- contents ----------------------------------------------------------
    if (tail[0] === 'contents') {
      const contentPath = tail.slice(1).join('/');
      if (method === 'GET') {
        const refParam = q.get('ref');
        const tip = refParam !== null ? resolveRef(repo, refParam) : tipOfBranch(repo, repo.defaultBranch);
        if (tip === null) {
          sendError(res, 404, 'Not Found');
          return true;
        }
        const tipObj = repo.objects.get(tip);
        if (tipObj === undefined || tipObj.type !== 'commit') {
          sendError(res, 404, 'Not Found');
          return true;
        }
        const treeSha = parseCommit(tipObj.content).tree;
        if (contentPath === '') {
          const children = listDirAtTree(repo, treeSha, '');
          sendJson(
            res,
            200,
            children.map((c) => ({ type: c.type, name: c.name, path: c.path, sha: c.sha, size: 0 })),
          );
          return true;
        }
        const blobSha = getFileAtTree(repo, treeSha, contentPath);
        if (blobSha !== null) {
          const blob = repo.objects.get(blobSha);
          const bytes = blob !== undefined ? blob.content : Buffer.alloc(0);
          sendJson(res, 200, {
            type: 'file',
            encoding: 'base64',
            content: bytes.toString('base64'),
            sha: blobSha,
            size: bytes.length,
            name: contentPath.split('/').pop() as string,
            path: contentPath,
          });
          return true;
        }
        const children = listDirAtTree(repo, treeSha, contentPath);
        if (children.length > 0) {
          sendJson(
            res,
            200,
            children.map((c) => ({ type: c.type, name: c.name, path: c.path, sha: c.sha, size: 0 })),
          );
          return true;
        }
        sendError(res, 404, 'Not Found');
        return true;
      }

      if (method === 'PUT') {
        const message = str(body.message);
        const contentB64 = str(body.content);
        if (message === null || message === '' || contentB64 === null) {
          sendError(res, 422, 'message and content are required');
          return true;
        }
        let bytes: Buffer;
        try {
          bytes = Buffer.from(contentB64, 'base64');
        } catch {
          sendError(res, 400, 'content is not valid base64');
          return true;
        }
        const branch = str(body.branch) ?? repo.defaultBranch;
        const refName = `refs/heads/${branch}`;
        const tip = repo.refs.get(refName) ?? null;
        let parentTreeSha: string | null = null;
        if (tip !== null) {
          const tipObj = repo.objects.get(tip);
          if (tipObj !== undefined && tipObj.type === 'commit') parentTreeSha = parseCommit(tipObj.content).tree;
        }
        const currentBlob = getFileAtTree(repo, parentTreeSha, contentPath);
        const givenSha = str(body.sha);
        const isUpdate = currentBlob !== null;
        if (givenSha !== null) {
          if (currentBlob === null) {
            sendError(res, 404, 'Not Found');
            return true;
          }
          if (givenSha !== currentBlob) {
            sendError(res, 409, 'sha does not match the current file');
            return true;
          }
        } else if (isUpdate) {
          sendError(res, 422, 'sha is required to update an existing file');
          return true;
        }
        const blobSha = storeObject(repo, 'blob', bytes);
        const treeSha = buildTree(repo, parentTreeSha, contentPath, blobSha);
        const epoch = COMMIT_EPOCH_BASE + store.tick();
        const { sha: commitSha, content: commitContent } = commitObject(treeSha, tip, message, viewerLogin, epoch);
        repo.objects.set(commitSha, { type: 'commit', content: commitContent });
        repo.refs.set(refName, commitSha);
        sendJson(res, isUpdate ? 200 : 201, {
          content: {
            name: contentPath.split('/').pop() as string,
            path: contentPath,
            sha: blobSha,
            size: bytes.length,
            type: 'file',
            encoding: 'base64',
            content: contentB64,
          },
          commit: { sha: commitSha, message },
        });
        return true;
      }

      if (method === 'DELETE') {
        const message = str(body.message);
        const givenSha = str(body.sha);
        if (message === null || message === '' || givenSha === null) {
          sendError(res, 422, 'message and sha are required');
          return true;
        }
        const branch = str(body.branch) ?? repo.defaultBranch;
        const refName = `refs/heads/${branch}`;
        const tip = repo.refs.get(refName) ?? null;
        if (tip === null) {
          sendError(res, 404, 'Not Found');
          return true;
        }
        const tipObj = repo.objects.get(tip);
        const parentTreeSha = tipObj !== undefined && tipObj.type === 'commit' ? parseCommit(tipObj.content).tree : null;
        const currentBlob = getFileAtTree(repo, parentTreeSha, contentPath);
        if (currentBlob === null) {
          sendError(res, 404, 'Not Found');
          return true;
        }
        if (givenSha !== currentBlob) {
          sendError(res, 409, 'sha does not match the current file');
          return true;
        }
        const treeSha = buildTree(repo, parentTreeSha, contentPath, null);
        const epoch = COMMIT_EPOCH_BASE + store.tick();
        const { sha: commitSha, content: commitContent } = commitObject(treeSha, tip, message, viewerLogin, epoch);
        repo.objects.set(commitSha, { type: 'commit', content: commitContent });
        repo.refs.set(refName, commitSha);
        sendJson(res, 200, { content: null, commit: { sha: commitSha, message } });
        return true;
      }

      sendError(res, 404, 'Not Found');
      return true;
    }

    // ---- labels ------------------------------------------------------------
    if (tail[0] === 'labels' && tail.length === 1) {
      if (method === 'POST') {
        const name = str(body.name);
        if (name === null || name === '') {
          sendError(res, 422, 'Label name is required');
          return true;
        }
        const color = str(body.color) ?? '000000';
        const label: Label = { name, color };
        repo.labels.set(name, label);
        sendJson(res, 201, labelJson(label));
        return true;
      }
      if (method === 'GET') {
        sendJson(res, 200, [...repo.labels.values()].map((l) => labelJson(l)));
        return true;
      }
      sendError(res, 404, 'Not Found');
      return true;
    }

    // ---- issues ------------------------------------------------------------
    if (tail[0] === 'issues') {
      // POST /issues and GET /issues (list)
      if (tail.length === 1) {
        if (method === 'POST') {
          const title = str(body.title);
          if (title === null || title === '') {
            sendError(res, 422, 'Title is required');
            return true;
          }
          const labelNames: string[] = Array.isArray(body.labels)
            ? (body.labels as unknown[]).filter((l): l is string => typeof l === 'string')
            : [];
          for (const name of labelNames) {
            if (!repo.labels.has(name)) {
              sendError(res, 422, `Label does not exist: ${name}`);
              return true;
            }
          }
          const issue: Issue = {
            id: store.nextIssueId++,
            number: ++repo.issueSeq,
            title,
            body: str(body.body) ?? '',
            state: 'open',
            labels: labelNames,
            createdAt: store.tick(),
            comments: [],
          };
          repo.issues.push(issue);
          sendJson(res, 201, issueJson(issue, repo));
          return true;
        }
        if (method === 'GET') {
          const state = q.get('state') ?? 'open';
          let issues = repo.issues;
          if (state === 'open' || state === 'closed') issues = issues.filter((i) => i.state === state);
          const direction = q.get('direction') ?? 'desc';
          const sorted = [...issues].sort((a, b) => (direction === 'asc' ? a.createdAt - b.createdAt : b.createdAt - a.createdAt));
          const { page, link } = paginate(sorted, q, req);
          sendJson(res, 200, page.map((i) => issueJson(i, repo)), withLink(link));
          return true;
        }
        sendError(res, 404, 'Not Found');
        return true;
      }

      // /issues/{number}...
      const number = Number.parseInt(tail[1] as string, 10);
      if (!Number.isFinite(number)) {
        sendError(res, 404, 'Not Found');
        return true;
      }
      const issue = repo.issues.find((i) => i.number === number);
      if (issue === undefined) {
        sendError(res, 404, 'Not Found');
        return true;
      }
      if (tail.length === 2) {
        if (method === 'GET') {
          sendJson(res, 200, issueJson(issue, repo));
          return true;
        }
        if (method === 'PATCH') {
          const state = str(body.state);
          if (state === 'open' || state === 'closed') issue.state = state;
          const title = str(body.title);
          if (title !== null) issue.title = title;
          const bodyText = str(body.body);
          if (bodyText !== null) issue.body = bodyText;
          sendJson(res, 200, issueJson(issue, repo));
          return true;
        }
        sendError(res, 404, 'Not Found');
        return true;
      }
      if (tail.length === 3 && tail[2] === 'comments') {
        if (method === 'POST') {
          const text = str(body.body);
          if (text === null || text === '') {
            sendError(res, 422, 'Comment body is required');
            return true;
          }
          const comment = { id: store.nextCommentId++, body: text, createdAt: store.tick() };
          issue.comments.push(comment);
          sendJson(res, 201, commentJson(comment, repo));
          return true;
        }
        if (method === 'GET') {
          sendJson(res, 200, issue.comments.map((c) => commentJson(c, repo)));
          return true;
        }
        sendError(res, 404, 'Not Found');
        return true;
      }
      sendError(res, 404, 'Not Found');
      return true;
    }

    // ---- pulls -------------------------------------------------------------
    if (tail[0] === 'pulls') {
      if (tail.length === 1) {
        if (method === 'POST') {
          const title = str(body.title);
          const head = str(body.head);
          const base = str(body.base);
          if (title === null || title === '' || head === null || head === '' || base === null || base === '') {
            sendError(res, 422, 'title, head and base are required');
            return true;
          }
          const headSha = tipOfBranch(repo, head);
          const baseSha = tipOfBranch(repo, base);
          if (headSha === null || baseSha === null) {
            sendError(res, 422, 'head or base branch does not exist');
            return true;
          }
          const headTipObj = repo.objects.get(headSha);
          const baseTipObj = repo.objects.get(baseSha);
          const headTree = headTipObj !== undefined && headTipObj.type === 'commit' ? parseCommit(headTipObj.content).tree : null;
          const baseTree = baseTipObj !== undefined && baseTipObj.type === 'commit' ? parseCommit(baseTipObj.content).tree : null;
          const pull: Pull = {
            id: store.nextPullId++,
            number: ++repo.pullSeq,
            title,
            body: str(body.body) ?? '',
            state: 'open',
            merged: false,
            mergedAt: null,
            mergeCommitSha: null,
            head: { ref: head, sha: headSha },
            base: { ref: base, sha: baseSha },
            files: diffTrees(repo, baseTree, headTree),
            createdAt: store.tick(),
          };
          repo.pulls.push(pull);
          sendJson(res, 201, pullJson(pull, repo));
          return true;
        }
        if (method === 'GET') {
          const state = q.get('state') ?? 'open';
          let pulls = repo.pulls;
          if (state === 'open' || state === 'closed') pulls = pulls.filter((p) => p.state === state);
          const { page, link } = paginate(pulls, q, req);
          sendJson(res, 200, page.map((p) => pullJson(p, repo)), withLink(link));
          return true;
        }
        sendError(res, 404, 'Not Found');
        return true;
      }

      const number = Number.parseInt(tail[1] as string, 10);
      if (!Number.isFinite(number)) {
        sendError(res, 404, 'Not Found');
        return true;
      }
      const pull = repo.pulls.find((p) => p.number === number);
      if (pull === undefined) {
        sendError(res, 404, 'Not Found');
        return true;
      }
      if (tail.length === 2 && method === 'GET') {
        sendJson(res, 200, pullJson(pull, repo));
        return true;
      }
      if (tail.length === 3 && tail[2] === 'files' && method === 'GET') {
        sendJson(res, 200, pull.files);
        return true;
      }
      if (tail.length === 3 && tail[2] === 'merge' && method === 'PUT') {
        if (pull.merged) {
          sendJson(res, 200, { merged: true, sha: pull.mergeCommitSha, message: `Pull request #${pull.number} already merged` });
          return true;
        }
        if (pull.state !== 'open') {
          sendError(res, 405, 'Pull request is closed');
          return true;
        }
        const baseTip = tipOfBranch(repo, pull.base.ref);
        const headTip = tipOfBranch(repo, pull.head.ref);
        if (baseTip === null || headTip === null) {
          sendError(res, 422, 'head or base branch does not exist');
          return true;
        }
        // Conflict: base moved past the PR's base sha AND a touched file's
        // current base content differs from the PR head's content.
        if (baseTip !== pull.base.sha) {
          const baseTipObj = repo.objects.get(baseTip);
          const headTipObj = repo.objects.get(headTip);
          const baseTree = baseTipObj !== undefined && baseTipObj.type === 'commit' ? parseCommit(baseTipObj.content).tree : null;
          const headTree = headTipObj !== undefined && headTipObj.type === 'commit' ? parseCommit(headTipObj.content).tree : null;
          for (const file of pull.files) {
            const baseBlob = getFileAtTree(repo, baseTree, file.filename);
            const headBlob = getFileAtTree(repo, headTree, file.filename);
            if (baseBlob !== headBlob) {
              sendError(res, 405, 'Merge conflict');
              return true;
            }
          }
        }
        // Squash-merge: merge commit on base whose tree = head tip tree.
        const headTipObj = repo.objects.get(headTip);
        const headTree = headTipObj !== undefined && headTipObj.type === 'commit' ? parseCommit(headTipObj.content).tree : null;
        if (headTree === null) {
          sendError(res, 422, 'head branch has no commits');
          return true;
        }
        const message = `Merge pull request #${pull.number} from ${repo.owner}/${pull.head.ref}`;
        const epoch = COMMIT_EPOCH_BASE + store.tick();
        const { sha: mergeSha, content: mergeContent } = commitObject(headTree, baseTip, message, viewerLogin, epoch);
        repo.objects.set(mergeSha, { type: 'commit', content: mergeContent });
        repo.refs.set(`refs/heads/${pull.base.ref}`, mergeSha);
        pull.state = 'closed';
        pull.merged = true;
        pull.mergedAt = MERGED_AT;
        pull.mergeCommitSha = mergeSha;
        sendJson(res, 200, { merged: true, sha: mergeSha, message });
        return true;
      }
      sendError(res, 404, 'Not Found');
      return true;
    }

    // Unknown sub-route under an existing repo.
    sendError(res, 404, 'Not Found');
    return true;
  }

  // Unknown /api/v3 route.
  sendError(res, 404, 'Not Found');
  return true;
}
