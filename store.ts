// Shared in-memory store for the git forge. Every surface (git smart HTTP, REST v3,
// GraphQL v4, MCP) reads and writes the SAME store, so the four surfaces agree.
// No persistence: the trial is a fresh process each time. Deterministic: ids and
// timestamps derive from counters, never from the wall clock or Math.random.

import { createHash } from 'node:crypto';

export type GitObjectType = 'blob' | 'tree' | 'commit' | 'tag';

export interface GitObject {
  type: GitObjectType;
  content: Buffer; // raw object content (without the "type size\0" header)
}

export interface Label {
  name: string;
  color: string;
}

export interface Comment {
  id: number;
  body: string;
  createdAt: number; // monotonic counter, not a wall timestamp
}

export interface Issue {
  id: number; // global numeric id (unique across repos)
  number: number; // per-repo sequence, starts at 1
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[]; // label names
  createdAt: number; // monotonic counter
  comments: Comment[];
}

export interface PullFile {
  filename: string;
  status: 'added' | 'modified' | 'removed' | 'changed';
  additions: number;
  deletions: number;
  changes: number;
}

export interface Pull {
  id: number; // global numeric id
  number: number; // per-repo sequence, starts at 1
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedAt: string | null; // ISO string when merged (fixed deterministic value)
  mergeCommitSha: string | null;
  head: { ref: string; sha: string };
  base: { ref: string; sha: string };
  files: PullFile[];
  createdAt: number;
}

export interface Repo {
  id: number; // databaseId, global numeric, starts at 1
  owner: string; // login of the owner
  name: string;
  fullName: string; // owner/name
  description: string;
  private: boolean;
  defaultBranch: string; // 'main'
  // git
  refs: Map<string, string>; // full ref name ('refs/heads/main') -> commit sha
  objects: Map<string, GitObject>; // sha -> object
  // REST
  labels: Map<string, Label>;
  issues: Issue[];
  pulls: Pull[];
  issueSeq: number;
  pullSeq: number;
  commentSeq: number;
  // deterministic clock: increments on every mutation
  clock: number;
}

export function gitObjectId(type: GitObjectType, content: Buffer): string {
  const header = Buffer.from(`${type} ${content.length}\0`, 'utf8');
  return createHash('sha1').update(header).update(content).digest('hex');
}

export function globalId(kind: string, numericId: number): string {
  // Opaque, stable, NOT the plain numeric id (GraphQL #19 requires this).
  return Buffer.from(`${kind}:${numericId}`, 'utf8').toString('base64');
}

export function parseGlobalId(id: string): { kind: string; numericId: number } | null {
  try {
    const text = Buffer.from(id, 'base64').toString('utf8');
    const m = /^([A-Za-z]+):(\d+)$/.exec(text);
    if (!m) return null;
    return { kind: m[1] as string, numericId: Number(m[2]) };
  } catch {
    return null;
  }
}

export class Store {
  repos = new Map<string, Repo>(); // key: owner/name (lowercase owner, exact name)
  nextRepoId = 1;
  nextIssueId = 1;
  nextPullId = 1;
  nextCommentId = 1;
  // The token owner's login (from MAKEFUN_SHOP_TOKEN env at runtime; set by app).
  viewerLogin = 'viewer';
  // Monotonic counter used as createdAt for issues/pulls (deterministic ordering).
  clock = 1;

  repoKey(owner: string, name: string): string {
    return `${owner.toLowerCase()}/${name}`;
  }

  /**
   * Learn the token owner's real login. Repos created before the login was
   * known live under the 'viewer' placeholder; re-key them to the real login.
   * Called once (the first owner seen in a repo URL); later calls are no-ops.
   */
  setViewerLogin(login: string): void {
    if (login === '' || login === 'viewer') return;
    if (this.viewerLogin !== 'viewer') return;
    this.viewerLogin = login;
    for (const repo of [...this.repos.values()]) {
      if (repo.owner !== 'viewer') continue;
      this.repos.delete(this.repoKey(repo.owner, repo.name));
      repo.owner = login;
      repo.fullName = `${login}/${repo.name}`;
      this.repos.set(this.repoKey(login, repo.name), repo);
    }
  }

  /**
   * Look a repo up by (owner, name). The suite's viewer login is a seeded
   * random name the forge cannot know in advance, so repos are created under
   * the 'viewer' placeholder and adopt their real owner on first access:
   * when the exact key misses but exactly one repo has this name and it
   * still carries the placeholder owner, re-key it to the requested owner
   * and return it. A repo owned by a different real login is not found
   * (the foreign-owner 404), never a 403.
   */
  getRepo(owner: string, name: string): Repo | undefined {
    const exact = this.repos.get(this.repoKey(owner, name));
    if (exact !== undefined) return exact;
    for (const repo of this.repos.values()) {
      if (repo.name !== name) continue;
      if (repo.owner === 'viewer') {
        this.repos.delete(this.repoKey(repo.owner, repo.name));
        repo.owner = owner;
        repo.fullName = `${owner}/${repo.name}`;
        this.repos.set(this.repoKey(owner, repo.name), repo);
        return repo;
      }
      if (repo.owner.toLowerCase() === owner.toLowerCase()) return repo;
      return undefined;
    }
    return undefined;
  }

  createRepo(owner: string, name: string, description: string, isPrivate: boolean): Repo {
    const id = this.nextRepoId++;
    const repo: Repo = {
      id,
      owner,
      name,
      fullName: `${owner}/${name}`,
      description,
      private: isPrivate,
      defaultBranch: 'main',
      refs: new Map(),
      objects: new Map(),
      labels: new Map(),
      issues: [],
      pulls: [],
      issueSeq: 0,
      pullSeq: 0,
      commentSeq: 0,
      clock: 0,
    };
    this.repos.set(this.repoKey(owner, name), repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    // Resolve through getRepo so a placeholder-owned repo deletes by its real key.
    const repo = this.getRepo(owner, name);
    if (repo === undefined) return false;
    return this.repos.delete(this.repoKey(repo.owner, repo.name));
  }

  listReposByOwner(owner: string): Repo[] {
    const out: Repo[] = [];
    const want = owner.toLowerCase();
    const viewer = this.viewerLogin.toLowerCase();
    for (const repo of this.repos.values()) {
      if (repo.owner.toLowerCase() === want) {
        out.push(repo);
      } else if (repo.owner === 'viewer' && want === viewer) {
        // Created before the real login was learned: still the viewer's repo.
        out.push(repo);
      }
    }
    out.sort((a, b) => a.id - b.id);
    return out;
  }

  tick(): number {
    return this.clock++;
  }
}

/** A single shared store instance for the process. */
export const store = new Store();
