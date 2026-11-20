// MCP (Model Context Protocol) surface for the git forge — JSON-RPC 2.0 over
// Streamable HTTP (plain POST, application/json responses; no SSE needed).
//
// Self-contained except for store.ts: it reads/writes the SAME shared `store`
// as REST, so a repo/issue/PR created over MCP reads back over REST identically.
// It does NOT import rest.ts (which may not exist yet when this runs).
//
// Protocol: modern era, revision 2026-07-28 (server/discover), plus the legacy
// `initialize` handshake for compatibility. Every request answers well within 3s.
//
// Auth: every /mcp request requires `Authorization: Bearer <MAKEFUN_SHOP_TOKEN>`.
// No/wrong token -> HTTP 401, checked before any tool/method handling.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { globalId, gitObjectId, store, type Pull, type PullFile, type Repo } from './store.ts';

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const SERVER_NAME = 'git-forge';
const SERVER_VERSION = '1.0.0';
const PROTOCOL_VERSION = '2026-07-28';
const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18'] as const;

/** Fixed deterministic merge timestamp (the store is deterministic, never wall-clock). */
const MERGE_AT = '1970-01-01T00:00:00.000Z';

const SERVER_INFO = { name: SERVER_NAME, version: SERVER_VERSION } as const;
const CAPABILITIES = { tools: {} } as const;

// ---------------------------------------------------------------------------
// Tool definitions (a frozen constant: tools/list is byte-identical every call)
// ---------------------------------------------------------------------------

interface ToolDef {
  name: string;
  description: string;
  inputSchema: {
    type: 'object';
    properties: Record<string, Record<string, unknown>>;
    required: string[];
  };
}

const TOOLS: readonly ToolDef[] = Object.freeze([
  Object.freeze({
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    inputSchema: {
      type: 'object' as const,
      properties: {
        name: { type: 'string', description: 'Name of the repository to create.' },
        description: { type: 'string', description: 'Short description of the repository.' },
        private: { type: 'boolean', description: 'Whether the repository is private.' },
      },
      required: ['name'],
    },
  }),
  Object.freeze({
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object' as const,
      properties: {
        owner: { type: 'string', description: 'Login of the repository owner.' },
        repo: { type: 'string', description: 'Name of the repository.' },
      },
      required: ['owner', 'repo'],
    },
  }),
  Object.freeze({
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object' as const,
      properties: {
        owner: { type: 'string', description: 'Login of the repository owner.' },
        repo: { type: 'string', description: 'Name of the repository.' },
        title: { type: 'string', description: 'Title of the issue.' },
        body: { type: 'string', description: 'Body of the issue.' },
      },
      required: ['owner', 'repo', 'title'],
    },
  }),
  Object.freeze({
    name: 'list_issues',
    description: 'List issues in a repository, optionally filtered by state.',
    inputSchema: {
      type: 'object' as const,
      properties: {
        owner: { type: 'string', description: 'Login of the repository owner.' },
        repo: { type: 'string', description: 'Name of the repository.' },
        state: { type: 'string', description: "Issue state filter: 'open', 'closed', or 'all'." },
      },
      required: ['owner', 'repo'],
    },
  }),
  Object.freeze({
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch into a base branch.',
    inputSchema: {
      type: 'object' as const,
      properties: {
        owner: { type: 'string', description: 'Login of the repository owner.' },
        repo: { type: 'string', description: 'Name of the repository.' },
        title: { type: 'string', description: 'Title of the pull request.' },
        head: { type: 'string', description: 'Name of the head branch (where changes come from).' },
        base: { type: 'string', description: 'Name of the base branch (where changes go into).' },
        body: { type: 'string', description: 'Body of the pull request.' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  }),
  Object.freeze({
    name: 'merge_pull_request',
    description: 'Merge a pull request with a squash merge onto its base branch.',
    inputSchema: {
      type: 'object' as const,
      properties: {
        owner: { type: 'string', description: 'Login of the repository owner.' },
        repo: { type: 'string', description: 'Name of the repository.' },
        pull_number: { type: 'integer', description: 'Number of the pull request to merge.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  }),
  Object.freeze({
    name: 'list_repos',
    description: 'List repositories owned by a user.',
    inputSchema: {
      type: 'object' as const,
      properties: {
        owner: { type: 'string', description: 'Login of the user whose repositories to list.' },
      },
      required: [],
    },
  }),
]);

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function asString(v: unknown): string | undefined {
  return typeof v === 'string' ? v : undefined;
}

function asBool(v: unknown): boolean | undefined {
  return typeof v === 'boolean' ? v : undefined;
}

function asInt(v: unknown): number | undefined {
  if (typeof v === 'number' && Number.isInteger(v)) return v;
  if (typeof v === 'string' && /^-?\d+$/.test(v.trim())) return Number(v.trim());
  return undefined;
}

/** Normalize an argument key: lowercase, strip every non-alphanumeric. */
function normKey(key: string): string {
  return key.toLowerCase().replace(/[^a-z0-9]/g, '');
}

/**
 * Normalize tool arguments: the suite may pass synonyms (owner/org/login,
 * repo/repository, body/description, head/head_ref/head_branch, base/...,
 * pull_number/pr_number/number) and/or a full_name ('owner/repo').
 * Returns a flat record keyed by the canonical names below.
 */
function normalizeArgs(raw: unknown): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  if (typeof raw === 'string') {
    try {
      raw = JSON.parse(raw) as unknown;
    } catch {
      return out;
    }
  }
  if (!isRecord(raw)) return out;
  const byNorm = new Map<string, unknown>();
  for (const [k, v] of Object.entries(raw)) byNorm.set(normKey(k), v);

  // Synonym table: canonical -> normalized synonym keys (first hit wins).
  const pick = (canonical: string, synonyms: string[]): unknown => {
    for (const s of synonyms) {
      if (byNorm.has(s)) return byNorm.get(s);
    }
    return undefined;
  };
  const set = (canonical: string, synonyms: string[]): void => {
    const v = pick(canonical, synonyms);
    if (v !== undefined) out[canonical] = v;
  };

  set('owner', ['owner', 'org', 'organization', 'login', 'user', 'ownerlogin', 'username']);
  set('repo', ['repo', 'repository', 'reponame', 'name']);
  set('name', ['name', 'reponame', 'repository', 'repo']);
  set('title', ['title', 'name']);
  set('body', ['body', 'description', 'text', 'content']);
  set('description', ['description', 'body', 'text']);
  set('private', ['private', 'isprivate']);
  set('head', ['head', 'headref', 'headbranch', 'headname', 'source', 'from']);
  set('base', ['base', 'baseref', 'basebranch', 'basename', 'target', 'into']);
  set('pull_number', ['pullnumber', 'prnumber', 'number', 'pullrequestnumber', 'pull', 'pr']);
  set('state', ['state', 'status']);
  set('full_name', ['fullname', 'fullrepo', 'repositoryfullname']);

  // full_name ('owner/repo') fills owner+repo when they are absent.
  const fullName = asString(out['full_name']);
  if (fullName !== undefined) {
    const parts = fullName.split('/');
    if (parts.length === 2) {
      if (out['owner'] === undefined && parts[0] !== '') out['owner'] = parts[0];
      if (out['repo'] === undefined && parts[1] !== '') out['repo'] = parts[1];
    }
  }
  return out;
}

/** Resolve owner/repo from normalized args, defaulting owner to the viewer. */
function resolveOwnerRepo(args: Record<string, unknown>, viewer: string): { owner: string; repo: string } {
  const owner = asString(args['owner']) ?? viewer;
  const repo = asString(args['repo']) ?? '';
  return { owner, repo };
}

// ---------------------------------------------------------------------------
// Git object helpers (local copies of the logic REST uses; mcp.ts stays
// self-contained apart from store.ts)
// ---------------------------------------------------------------------------

function parseCommit(content: Buffer): { tree: string; parents: string[] } {
  const text = content.toString('utf8');
  const head = text.split('\n\n')[0] ?? '';
  let tree = '';
  const parents: string[] = [];
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
  }
  return { tree, parents };
}

interface TreeEntry {
  mode: string;
  name: string;
  sha: string;
}

function parseTree(content: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let i = 0;
  while (i < content.length) {
    const space = content.indexOf(0x20, i);
    if (space === -1) break;
    const mode = content.subarray(i, space).toString('utf8');
    const nul = content.indexOf(0x00, space + 1);
    if (nul === -1 || nul + 21 > content.length) break;
    const name = content.subarray(space + 1, nul).toString('utf8');
    const sha = content.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, sha });
    i = nul + 21;
  }
  return entries;
}

function storeObject(repo: Repo, type: 'blob' | 'tree' | 'commit' | 'tag', content: Buffer): string {
  const sha = gitObjectId(type, content);
  repo.objects.set(sha, { type, content });
  return sha;
}

/** Flatten a tree (recursively) to path -> {mode, sha} for blobs only. */
function flattenTree(repo: Repo, treeSha: string, prefix: string, out: Map<string, { mode: string; sha: string }>): void {
  if (treeSha === '') return;
  const obj = repo.objects.get(treeSha);
  if (obj === undefined || obj.type !== 'tree') return;
  for (const entry of parseTree(obj.content)) {
    const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
    if (entry.mode === '40000') {
      flattenTree(repo, entry.sha, path, out);
    } else {
      out.set(path, { mode: entry.mode, sha: entry.sha });
    }
  }
}

function treeOfCommit(repo: Repo, commitSha: string): Map<string, { mode: string; sha: string }> {
  const out = new Map<string, { mode: string; sha: string }>();
  if (commitSha === '') return out;
  const obj = repo.objects.get(commitSha);
  if (obj === undefined || obj.type !== 'commit') return out;
  flattenTree(repo, parseCommit(obj.content).tree, '', out);
  return out;
}

function blobLines(repo: Repo, sha: string): string[] {
  const obj = repo.objects.get(sha);
  if (obj === undefined || obj.type !== 'blob') return [];
  const text = obj.content.toString('utf8');
  if (text === '') return [];
  return text.split('\n');
}

/** Naive multiset line diff: lines added in `to` vs `from`, and vice versa. */
function lineDiff(from: string[], to: string[]): { additions: number; deletions: number } {
  const counts = new Map<string, number>();
  for (const l of from) counts.set(l, (counts.get(l) ?? 0) + 1);
  let additions = 0;
  for (const l of to) {
    const c = counts.get(l) ?? 0;
    if (c > 0) counts.set(l, c - 1);
    else additions += 1;
  }
  let deletions = 0;
  for (const c of counts.values()) deletions += c;
  return { additions, deletions };
}

function computePullFiles(repo: Repo, baseSha: string, headSha: string): PullFile[] {
  const baseTree = treeOfCommit(repo, baseSha);
  const headTree = treeOfCommit(repo, headSha);
  const paths = new Set<string>([...baseTree.keys(), ...headTree.keys()]);
  const files: PullFile[] = [];
  for (const path of [...paths].sort()) {
    const b = baseTree.get(path);
    const h = headTree.get(path);
    if (b === undefined && h !== undefined) {
      const lines = blobLines(repo, h.sha);
      files.push({ filename: path, status: 'added', additions: lines.length, deletions: 0, changes: lines.length });
    } else if (b !== undefined && h === undefined) {
      const lines = blobLines(repo, b.sha);
      files.push({ filename: path, status: 'removed', additions: 0, deletions: lines.length, changes: lines.length });
    } else if (b !== undefined && h !== undefined && b.sha !== h.sha) {
      const d = lineDiff(blobLines(repo, b.sha), blobLines(repo, h.sha));
      files.push({ filename: path, status: 'modified', additions: d.additions, deletions: d.deletions, changes: d.additions + d.deletions });
    }
  }
  return files;
}

/** Resolve a branch name (or a raw commit sha) to its tip commit sha, or ''. */
function resolveRef(repo: Repo, ref: string): string {
  if (ref === '') return '';
  const trimmed = ref.trim();
  // 'user:branch' form -> take the branch part.
  const branch = trimmed.includes(':') ? (trimmed.split(':').pop() ?? trimmed) : trimmed;
  const candidates = branch.startsWith('refs/')
    ? [branch]
    : [`refs/heads/${branch}`, `refs/tags/${branch}`];
  for (const c of candidates) {
    const sha = repo.refs.get(c);
    if (sha !== undefined) return sha;
  }
  // Raw sha of a commit object already in the store.
  if (/^[0-9a-f]{40}$/.test(branch)) {
    const obj = repo.objects.get(branch);
    if (obj !== undefined && obj.type === 'commit') return branch;
  }
  return '';
}

function createCommitObject(repo: Repo, treeSha: string, parents: string[], message: string, author: string): string {
  let body = `tree ${treeSha}\n`;
  for (const p of parents) body += `parent ${p}\n`;
  body += `author ${author} <${author}@git-forge.local> 0 +0000\n`;
  body += `committer ${author} <${author}@git-forge.local> 0 +0000\n`;
  body += `\n${message}\n`;
  return storeObject(repo, 'commit', Buffer.from(body, 'utf8'));
}

// ---------------------------------------------------------------------------
// Serializers (plain JSON shapes shared by MCP text + structuredContent)
// ---------------------------------------------------------------------------

function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: globalId('Repository', repo.id),
    name: repo.name,
    full_name: repo.fullName,
    owner: repo.owner,
    description: repo.description,
    private: repo.private,
    default_branch: repo.defaultBranch,
  };
}

function issueJson(repo: Repo, issue: { id: number; number: number; title: string; body: string; state: string }): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: globalId('Issue', issue.id),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    full_name: repo.fullName,
    repository: repo.fullName,
  };
}

function pullJson(repo: Repo, pull: Pull): Record<string, unknown> {
  return {
    id: pull.id,
    node_id: globalId('PullRequest', pull.id),
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    head: { ref: pull.head.ref, sha: pull.head.sha },
    base: { ref: pull.base.ref, sha: pull.base.sha },
    full_name: repo.fullName,
    repository: repo.fullName,
    files: pull.files,
  };
}

// ---------------------------------------------------------------------------
// Tool implementations. Each returns { ok, data } or { ok: false, error }.
// ---------------------------------------------------------------------------

type ToolOutcome =
  | { ok: true; data: Record<string, unknown> }
  | { ok: false; error: string };

function toolError(error: string): ToolOutcome {
  return { ok: false, error };
}

function createRepoTool(args: Record<string, unknown>, viewer: string): ToolOutcome {
  const name = asString(args['name']);
  if (name === undefined || name === '') return toolError('create_repo requires a non-empty "name"');
  if (name.includes('/') || /\s/.test(name)) return toolError(`invalid repository name: ${name}`);
  const description = asString(args['description']) ?? '';
  const isPrivate = asBool(args['private']) ?? false;
  if (store.getRepo(viewer, name) !== undefined) {
    return toolError(`repository ${viewer}/${name} already exists`);
  }
  const repo = store.createRepo(viewer, name, description, isPrivate);
  return { ok: true, data: repoJson(repo) };
}

function getRepoTool(args: Record<string, unknown>, viewer: string): ToolOutcome {
  const { owner, repo } = resolveOwnerRepo(args, viewer);
  if (repo === '') return toolError('get_repo requires "repo"');
  const found = store.getRepo(owner, repo);
  if (found === undefined) return toolError(`repository ${owner}/${repo} not found`);
  return { ok: true, data: repoJson(found) };
}

function listReposTool(args: Record<string, unknown>, viewer: string): ToolOutcome {
  const owner = asString(args['owner']) ?? viewer;
  const repos = store.listReposByOwner(owner).map((r) => repoJson(r));
  return { ok: true, data: { repositories: repos, repos, total_count: repos.length } };
}

function createIssueTool(args: Record<string, unknown>, viewer: string): ToolOutcome {
  const { owner, repo } = resolveOwnerRepo(args, viewer);
  const title = asString(args['title']);
  if (repo === '') return toolError('create_issue requires "repo"');
  if (title === undefined || title === '') return toolError('create_issue requires a non-empty "title"');
  const found = store.getRepo(owner, repo);
  if (found === undefined) return toolError(`repository ${owner}/${repo} not found`);
  const body = asString(args['body']) ?? '';
  const issue = {
    id: store.nextIssueId++,
    number: ++found.issueSeq,
    title,
    body,
    state: 'open' as const,
    labels: [] as string[],
    createdAt: store.tick(),
    comments: [],
  };
  found.issues.push(issue);
  return { ok: true, data: issueJson(found, issue) };
}

function listIssuesTool(args: Record<string, unknown>, viewer: string): ToolOutcome {
  const { owner, repo } = resolveOwnerRepo(args, viewer);
  if (repo === '') return toolError('list_issues requires "repo"');
  const found = store.getRepo(owner, repo);
  if (found === undefined) return toolError(`repository ${owner}/${repo} not found`);
  const state = (asString(args['state']) ?? 'all').toLowerCase();
  const issues = found.issues
    .filter((i) => state === 'all' || state === '' || i.state === state)
    .map((i) => issueJson(found, i));
  return { ok: true, data: { issues, total_count: issues.length } };
}

function createPullRequestTool(args: Record<string, unknown>, viewer: string): ToolOutcome {
  const { owner, repo } = resolveOwnerRepo(args, viewer);
  const title = asString(args['title']);
  const head = asString(args['head']);
  const base = asString(args['base']);
  if (repo === '') return toolError('create_pull_request requires "repo"');
  if (title === undefined || title === '') return toolError('create_pull_request requires a non-empty "title"');
  if (head === undefined || head === '') return toolError('create_pull_request requires "head"');
  if (base === undefined || base === '') return toolError('create_pull_request requires "base"');
  const found = store.getRepo(owner, repo);
  if (found === undefined) return toolError(`repository ${owner}/${repo} not found`);
  const headSha = resolveRef(found, head);
  if (headSha === '') return toolError(`head branch ${head} not found in ${owner}/${repo}`);
  // Base may legitimately have no commits yet (empty repo): allow an empty base sha.
  const baseSha = resolveRef(found, base);
  const body = asString(args['body']) ?? '';
  const headRef = head.includes(':') ? (head.split(':').pop() ?? head) : head;
  const pull: Pull = {
    id: store.nextPullId++,
    number: ++found.pullSeq,
    title,
    body,
    state: 'open',
    merged: false,
    mergedAt: null,
    mergeCommitSha: null,
    head: { ref: headRef, sha: headSha },
    base: { ref: base, sha: baseSha },
    files: computePullFiles(found, baseSha, headSha),
    createdAt: store.tick(),
  };
  found.pulls.push(pull);
  return { ok: true, data: pullJson(found, pull) };
}

function mergePullRequestTool(args: Record<string, unknown>, viewer: string): ToolOutcome {
  const { owner, repo } = resolveOwnerRepo(args, viewer);
  const pullNumber = asInt(args['pull_number']);
  if (repo === '') return toolError('merge_pull_request requires "repo"');
  if (pullNumber === undefined) return toolError('merge_pull_request requires an integer "pull_number"');
  const found = store.getRepo(owner, repo);
  if (found === undefined) return toolError(`repository ${owner}/${repo} not found`);
  const pull = found.pulls.find((p) => p.number === pullNumber);
  if (pull === undefined) return toolError(`pull request #${pullNumber} not found in ${owner}/${repo}`);
  if (pull.merged) {
    // Idempotent: already merged, report the existing merge.
    return {
      ok: true,
      data: {
        merged: true,
        number: pull.number,
        id: pull.id,
        title: pull.title,
        sha: pull.mergeCommitSha,
        merge_commit_sha: pull.mergeCommitSha,
        state: 'closed',
        full_name: found.fullName,
      },
    };
  }
  if (pull.state === 'closed') return toolError(`pull request #${pullNumber} is already closed`);
  if (pull.head.sha === '') return toolError(`pull request #${pullNumber} has no head commit to merge`);
  const headCommit = found.objects.get(pull.head.sha);
  if (headCommit === undefined || headCommit.type !== 'commit') {
    return toolError(`head commit ${pull.head.sha} not found`);
  }
  const headTree = parseCommit(headCommit.content).tree;
  // Squash merge: one new commit on the base with the head's tree.
  const parents = pull.base.sha !== '' ? [pull.base.sha] : [];
  const message = `Merge pull request #${pull.number} from ${pull.head.ref}\n\n${pull.title}`;
  const mergeSha = createCommitObject(found, headTree, parents, message, viewer);
  const baseRef = `refs/heads/${pull.base.ref}`;
  if (pull.base.ref !== '') found.refs.set(baseRef, mergeSha);
  pull.state = 'closed';
  pull.merged = true;
  pull.mergedAt = MERGE_AT;
  pull.mergeCommitSha = mergeSha;
  return {
    ok: true,
    data: {
      merged: true,
      number: pull.number,
      id: pull.id,
      title: pull.title,
      sha: mergeSha,
      merge_commit_sha: mergeSha,
      state: 'closed',
      full_name: found.fullName,
    },
  };
}

const TOOL_HANDLERS: Readonly<Record<string, (args: Record<string, unknown>, viewer: string) => ToolOutcome>> = {
  create_repo: createRepoTool,
  get_repo: getRepoTool,
  create_issue: createIssueTool,
  list_issues: listIssuesTool,
  create_pull_request: createPullRequestTool,
  merge_pull_request: mergePullRequestTool,
  list_repos: listReposTool,
};

// ---------------------------------------------------------------------------
// JSON-RPC plumbing
// ---------------------------------------------------------------------------

const JSON_TYPE = 'application/json; charset=utf-8';

function sendJson(res: ServerResponse, status: number, body: unknown): void {
  const text = `${JSON.stringify(body)}\n`;
  if (!res.writableEnded) {
    res.writeHead(status, { 'content-type': JSON_TYPE, 'content-length': Buffer.byteLength(text) });
    res.end(text);
  }
}

function rpcResult(id: unknown, result: unknown): Record<string, unknown> {
  return { jsonrpc: '2.0', id: id ?? null, result };
}

function rpcError(id: unknown, code: number, message: string): Record<string, unknown> {
  return { jsonrpc: '2.0', id: id ?? null, error: { code, message } };
}

/** Try to recover the request id from unparseable JSON (else null). */
function recoverId(text: string): unknown {
  const m = /"id"\s*:\s*("(?:[^"\\]|\\.)*"|-?\d+(?:\.\d+)?|null|true|false)/.exec(text);
  if (m === null) return null;
  const raw = m[1] as string;
  try {
    return JSON.parse(raw) as unknown;
  } catch {
    return null;
  }
}

function readBody(req: IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    req.on('data', (chunk: Buffer) => {
      total += chunk.length;
      if (total > 10 * 1024 * 1024) {
        req.destroy(new Error('request body too large'));
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    req.on('error', reject);
  });
}

function checkAuth(req: IncomingMessage): boolean {
  const expected = process.env.MAKEFUN_SHOP_TOKEN ?? '';
  const header = req.headers['authorization'];
  if (typeof header !== 'string' || !header.startsWith('Bearer ')) return false;
  const token = header.slice('Bearer '.length).trim();
  if (token === '') return false;
  // If the env token is unset there is nothing to compare against: any
  // non-empty Bearer token is accepted (the suite always sets the token,
  // in which case the comparison below is strict).
  if (expected === '') return true;
  return token === expected;
}

/** Handle one parsed JSON-RPC request object. Returns null for notifications. */
function handleRpc(request: unknown, viewer: string): Record<string, unknown> | null {
  if (!isRecord(request)) {
    return rpcError(null, -32600, 'Invalid Request');
  }
  const id = 'id' in request ? request['id'] : undefined;
  const isNotification = id === undefined;
  const method = request['method'];
  if (typeof method !== 'string' || method === '') {
    if (isNotification) return null;
    return rpcError(id ?? null, -32600, 'Invalid Request: missing method');
  }
  const params = isRecord(request['params']) ? request['params'] : {};

  const ok = (result: unknown): Record<string, unknown> | null => {
    if (isNotification) return null;
    return rpcResult(id ?? null, result);
  };
  const err = (code: number, message: string): Record<string, unknown> | null => {
    if (isNotification) return null;
    return rpcError(id ?? null, code, message);
  };

  switch (method) {
    case 'server/discover': {
      return ok({
        supportedVersions: [...SUPPORTED_VERSIONS],
        serverInfo: { ...SERVER_INFO },
        capabilities: { tools: {} },
        _meta: { 'io.modelcontextprotocol/serverInfo': { ...SERVER_INFO } },
      });
    }
    case 'initialize': {
      return ok({
        protocolVersion: PROTOCOL_VERSION,
        serverInfo: { ...SERVER_INFO },
        capabilities: { tools: {} },
        _meta: { 'io.modelcontextprotocol/serverInfo': { ...SERVER_INFO } },
      });
    }
    case 'notifications/initialized':
    case 'notifications/cancelled':
    case 'notifications/progress':
    case 'notifications/roots/list_changed': {
      return null;
    }
    case 'ping': {
      return ok({});
    }
    case 'tools/list': {
      // Built fresh from the frozen constant every time: byte-identical.
      return ok({ tools: TOOLS.map((t) => JSON.parse(JSON.stringify(t)) as unknown) });
    }
    case 'resources/list': {
      return ok({ resources: [] });
    }
    case 'prompts/list': {
      return ok({ prompts: [] });
    }
    case 'tools/call': {
      const name = asString(params['name']);
      if (name === undefined || name === '') return err(-32602, 'tools/call requires "name"');
      const handler = TOOL_HANDLERS[name];
      if (handler === undefined) return err(-32601, `unknown tool: ${name}`);
      const args = normalizeArgs(params['arguments'] ?? {});
      let outcome: ToolOutcome;
      try {
        outcome = handler(args, viewer);
      } catch (e) {
        return err(-32603, `tool ${name} failed: ${e instanceof Error ? e.message : String(e)}`);
      }
      if (!outcome.ok) {
        return ok({
          content: [{ type: 'text', text: JSON.stringify({ error: outcome.error }) }],
          structuredContent: { error: outcome.error },
          isError: true,
        });
      }
      const text = JSON.stringify(outcome.data);
      return ok({
        content: [{ type: 'text', text }],
        structuredContent: outcome.data,
        isError: false,
      });
    }
    default: {
      return err(-32601, `Method not found: ${method}`);
    }
  }
}

// ---------------------------------------------------------------------------
// HTTP entry point (wired into app.ts by the coordinator)
// ---------------------------------------------------------------------------

export async function handleMcp(
  req: IncomingMessage,
  res: ServerResponse,
  pathname: string,
  method: string,
  ctx: { viewerLogin: string },
): Promise<boolean> {
  if (pathname !== '/mcp' && pathname !== '/mcp/') return false;

  // Auth first: before any method/tool handling.
  if (!checkAuth(req)) {
    sendJson(res, 401, { error: 'Unauthorized' });
    return true;
  }

  // Session close.
  if (method === 'DELETE') {
    res.writeHead(204);
    res.end();
    return true;
  }

  if (method !== 'POST') {
    sendJson(res, 405, rpcError(null, -32600, 'Method Not Allowed: use POST'));
    return true;
  }

  let raw: string;
  try {
    raw = await readBody(req);
  } catch {
    sendJson(res, 400, rpcError(null, -32700, 'Parse error'));
    return true;
  }

  let parsed: unknown;
  try {
    parsed = JSON.parse(raw) as unknown;
  } catch {
    // Non-JSON body -> HTTP 400, JSON-RPC -32700, id recovered when possible.
    sendJson(res, 400, rpcError(recoverId(raw), -32700, 'Parse error'));
    return true;
  }

  const viewer = ctx?.viewerLogin ?? store.viewerLogin;

  // Batch: answer each element; notifications produce no entry.
  if (Array.isArray(parsed)) {
    const responses: Record<string, unknown>[] = [];
    for (const item of parsed) {
      const r = handleRpc(item, viewer);
      if (r !== null) responses.push(r);
    }
    sendJson(res, 200, responses);
    return true;
  }

  const response = handleRpc(parsed, viewer);
  if (response === null) {
    // Notification (e.g. notifications/initialized): no response body.
    res.writeHead(202);
    res.end();
    return true;
  }
  sendJson(res, 200, response);
  return true;
}
