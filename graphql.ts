// GraphQL v4 surface for the git forge (POST /api/graphql).
//
// A small, dependency-free GraphQL parser + executor over the shared store.
// Handles exactly the documents the suite sends: query/mutation operations,
// variables, inline arguments (string/int/enum/list/input-object/null),
// field aliases, inline fragments, fragment spreads, __type introspection,
// and batched request bodies. No other file is touched by this module; the
// coordinator wires handleGraphql into app.ts.
//
// Auth: POST /api/graphql requires `Authorization: Bearer <MAKEFUN_SHOP_TOKEN>`.
// Only an auth failure is a 401; every other outcome (including field errors
// and execution errors) is HTTP 200 with an errors array. A malformed JSON
// body is HTTP 400.

import { randomUUID } from 'node:crypto';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { gitObjectId, globalId, parseGlobalId, store } from './store.ts';
import type { Issue, Pull, PullFile, Repo } from './store.ts';

// ---------------------------------------------------------------------------
// Introspection tables (what __type reports)
// ---------------------------------------------------------------------------

const TYPE_FIELDS: Record<string, string[]> = {
  Issue: ['id', 'number', 'title', 'state', 'body', 'author', 'comments', 'createdAt', 'url'],
  PullRequest: ['id', 'number', 'title', 'body', 'headRefName', 'baseRefName', 'mergeable', 'state', 'merged', 'files'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest'],
  User: ['id', 'login'],
  Query: ['viewer', 'repository', 'node', '__type', '__schema'],
  Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'],
};

const TYPE_ENUM_VALUES: Record<string, string[]> = {
  SearchType: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION'],
  IssueState: ['OPEN', 'CLOSED'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
  OrderDirection: ['ASC', 'DESC'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
};

// ---------------------------------------------------------------------------
// Lexer
// ---------------------------------------------------------------------------

type TokenKind = 'name' | 'int' | 'float' | 'string' | 'punct' | 'spread' | 'eof';

interface Token {
  kind: TokenKind;
  value: string;
}

const PUNCT = new Set(['!', '$', '(', ')', ':', '=', '@', '[', ']', '{', '}', '|', '&']);

function lex(src: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  const n = src.length;
  while (i < n) {
    const c = src[i] as string;
    if (c === ' ' || c === '\t' || c === '\n' || c === '\r' || c === ',' || c === '﻿') {
      i += 1;
      continue;
    }
    if (c === '#') {
      while (i < n && src[i] !== '\n') i += 1;
      continue;
    }
    if (c === '.' && src.slice(i, i + 3) === '...') {
      tokens.push({ kind: 'spread', value: '...' });
      i += 3;
      continue;
    }
    if (PUNCT.has(c)) {
      tokens.push({ kind: 'punct', value: c });
      i += 1;
      continue;
    }
    if (c === '"') {
      // Block string """...""" or normal "..." with escapes.
      if (src.slice(i, i + 3) === '"""') {
        const end = src.indexOf('"""', i + 3);
        if (end === -1) throw new Error('Unterminated block string');
        tokens.push({ kind: 'string', value: src.slice(i + 3, end).replace(/\\"""/g, '"""') });
        i = end + 3;
        continue;
      }
      let j = i + 1;
      let out = '';
      while (j < n && src[j] !== '"') {
        if (src[j] === '\\' && j + 1 < n) {
          const e = src[j + 1] as string;
          if (e === 'u' && j + 5 < n) {
            out += String.fromCharCode(parseInt(src.slice(j + 2, j + 6), 16));
            j += 6;
          } else {
            const map: Record<string, string> = { n: '\n', t: '\t', r: '\r', b: '\b', f: '\f', '"': '"', '\\': '\\', '/': '/' };
            out += map[e] ?? e;
            j += 2;
          }
        } else {
          out += src[j];
          j += 1;
        }
      }
      if (j >= n) throw new Error('Unterminated string');
      tokens.push({ kind: 'string', value: out });
      i = j + 1;
      continue;
    }
    if (c === '-' || (c >= '0' && c <= '9')) {
      let j = i;
      if (src[j] === '-') j += 1;
      while (j < n && src[j] >= '0' && src[j] <= '9') j += 1;
      let isFloat = false;
      if (j < n && src[j] === '.') {
        isFloat = true;
        j += 1;
        while (j < n && src[j] >= '0' && src[j] <= '9') j += 1;
      }
      if (j < n && (src[j] === 'e' || src[j] === 'E')) {
        isFloat = true;
        j += 1;
        if (j < n && (src[j] === '+' || src[j] === '-')) j += 1;
        while (j < n && src[j] >= '0' && src[j] <= '9') j += 1;
      }
      tokens.push({ kind: isFloat ? 'float' : 'int', value: src.slice(i, j) });
      i = j;
      continue;
    }
    if ((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c === '_') {
      let j = i;
      while (j < n && /[_0-9A-Za-z]/.test(src[j] as string)) j += 1;
      tokens.push({ kind: 'name', value: src.slice(i, j) });
      i = j;
      continue;
    }
    throw new Error(`Unexpected character ${JSON.stringify(c)} in GraphQL document`);
  }
  tokens.push({ kind: 'eof', value: '' });
  return tokens;
}

// ---------------------------------------------------------------------------
// Parser (AST)
// ---------------------------------------------------------------------------

type ValueNode =
  | { kind: 'Variable'; name: string }
  | { kind: 'String'; value: string }
  | { kind: 'Int'; value: number }
  | { kind: 'Float'; value: number }
  | { kind: 'Boolean'; value: boolean }
  | { kind: 'Enum'; value: string }
  | { kind: 'Null' }
  | { kind: 'List'; values: ValueNode[] }
  | { kind: 'Object'; fields: { name: string; value: ValueNode }[] };

interface ArgumentNode {
  name: string;
  value: ValueNode;
}

type SelectionNode =
  | { kind: 'Field'; alias: string | null; name: string; args: ArgumentNode[]; selectionSet: SelectionNode[] | null }
  | { kind: 'FragmentSpread'; name: string }
  | { kind: 'InlineFragment'; typeCondition: string | null; selectionSet: SelectionNode[] };

interface OperationDefinition {
  kind: 'Operation';
  operation: 'query' | 'mutation' | 'subscription';
  name: string | null;
  variableDefinitions: { name: string; defaultValue: ValueNode | null }[];
  selectionSet: SelectionNode[];
}

interface FragmentDefinition {
  kind: 'Fragment';
  name: string;
  typeCondition: string;
  selectionSet: SelectionNode[];
}

type DefinitionNode = OperationDefinition | FragmentDefinition;

class Parser {
  private pos = 0;
  private readonly tokens: Token[];
  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token {
    return this.tokens[this.pos] as Token;
  }

  private next(): Token {
    const t = this.tokens[this.pos] as Token;
    this.pos += 1;
    return t;
  }

  private expectPunct(v: string): void {
    const t = this.next();
    if (t.kind !== 'punct' || t.value !== v) throw new Error(`Expected '${v}' but got '${t.value}'`);
  }

  private expectName(value?: string): string {
    const t = this.next();
    if (t.kind !== 'name') throw new Error(`Expected a name but got '${t.value}'`);
    if (value !== undefined && t.value !== value) throw new Error(`Expected '${value}' but got '${t.value}'`);
    return t.value;
  }

  private isPunct(v: string): boolean {
    const t = this.peek();
    return t.kind === 'punct' && t.value === v;
  }

  private skipDirectives(): void {
    while (this.isPunct('@')) {
      this.next();
      this.expectName();
      if (this.isPunct('(')) this.parseArguments();
    }
  }

  parseDocument(): DefinitionNode[] {
    const defs: DefinitionNode[] = [];
    while (this.peek().kind !== 'eof') {
      defs.push(this.parseDefinition());
    }
    return defs;
  }

  private parseDefinition(): DefinitionNode {
    const t = this.peek();
    if (t.kind === 'punct' && t.value === '{') {
      return { kind: 'Operation', operation: 'query', name: null, variableDefinitions: [], selectionSet: this.parseSelectionSet() };
    }
    if (t.kind === 'name' && (t.value === 'query' || t.value === 'mutation' || t.value === 'subscription')) {
      const operation = this.next().value as 'query' | 'mutation' | 'subscription';
      let name: string | null = null;
      if (this.peek().kind === 'name') name = this.next().value;
      const variableDefinitions = this.parseVariableDefinitions();
      this.skipDirectives();
      return { kind: 'Operation', operation, name, variableDefinitions, selectionSet: this.parseSelectionSet() };
    }
    if (t.kind === 'name' && t.value === 'fragment') {
      this.next();
      const name = this.expectName();
      this.expectName('on');
      const typeCondition = this.expectName();
      this.skipDirectives();
      return { kind: 'Fragment', name, typeCondition, selectionSet: this.parseSelectionSet() };
    }
    throw new Error(`Unexpected token '${t.value}' at start of definition`);
  }

  private parseVariableDefinitions(): { name: string; defaultValue: ValueNode | null }[] {
    const defs: { name: string; defaultValue: ValueNode | null }[] = [];
    if (!this.isPunct('(')) return defs;
    this.next();
    while (!this.isPunct(')')) {
      this.expectPunct('$');
      const name = this.expectName();
      this.expectPunct(':');
      this.parseType();
      let defaultValue: ValueNode | null = null;
      if (this.isPunct('=')) {
        this.next();
        defaultValue = this.parseValue(true);
      }
      this.skipDirectives();
      defs.push({ name, defaultValue });
    }
    this.expectPunct(')');
    return defs;
  }

  private parseType(): void {
    if (this.isPunct('[')) {
      this.next();
      this.parseType();
      this.expectPunct(']');
    } else {
      this.expectName();
    }
    if (this.isPunct('!')) this.next();
  }

  private parseSelectionSet(): SelectionNode[] {
    this.expectPunct('{');
    const sels: SelectionNode[] = [];
    while (!this.isPunct('}')) {
      if (this.peek().kind === 'eof') throw new Error('Unterminated selection set');
      sels.push(this.parseSelection());
    }
    this.expectPunct('}');
    return sels;
  }

  private parseSelection(): SelectionNode {
    if (this.peek().kind === 'spread') {
      this.next();
      if (this.peek().kind === 'name' && this.peek().value === 'on') {
        this.next();
        const typeCondition = this.expectName();
        this.skipDirectives();
        return { kind: 'InlineFragment', typeCondition, selectionSet: this.parseSelectionSet() };
      }
      if (this.peek().kind === 'name') {
        const name = this.next().value;
        this.skipDirectives();
        return { kind: 'FragmentSpread', name };
      }
      // Anonymous inline fragment: ... { ... }
      this.skipDirectives();
      return { kind: 'InlineFragment', typeCondition: null, selectionSet: this.parseSelectionSet() };
    }
    const first = this.expectName();
    let alias: string | null = null;
    let name = first;
    if (this.isPunct(':')) {
      this.next();
      alias = first;
      name = this.expectName();
    }
    const args = this.parseArguments();
    this.skipDirectives();
    let selectionSet: SelectionNode[] | null = null;
    if (this.isPunct('{')) selectionSet = this.parseSelectionSet();
    return { kind: 'Field', alias, name, args, selectionSet };
  }

  private parseArguments(): ArgumentNode[] {
    const args: ArgumentNode[] = [];
    if (!this.isPunct('(')) return args;
    this.next();
    while (!this.isPunct(')')) {
      const name = this.expectName();
      this.expectPunct(':');
      args.push({ name, value: this.parseValue(false) });
    }
    this.expectPunct(')');
    return args;
  }

  private parseValue(isConst: boolean): ValueNode {
    const t = this.next();
    if (t.kind === 'punct' && t.value === '$') {
      if (isConst) throw new Error('Variable in constant value');
      return { kind: 'Variable', name: this.expectName() };
    }
    if (t.kind === 'punct' && t.value === '[') {
      const values: ValueNode[] = [];
      while (!(this.peek().kind === 'punct' && this.peek().value === ']')) {
        values.push(this.parseValue(isConst));
      }
      this.next();
      return { kind: 'List', values };
    }
    if (t.kind === 'punct' && t.value === '{') {
      const fields: { name: string; value: ValueNode }[] = [];
      while (!(this.peek().kind === 'punct' && this.peek().value === '}')) {
        const name = this.expectName();
        this.expectPunct(':');
        fields.push({ name, value: this.parseValue(isConst) });
      }
      this.next();
      return { kind: 'Object', fields };
    }
    if (t.kind === 'string') return { kind: 'String', value: t.value };
    if (t.kind === 'int') return { kind: 'Int', value: parseInt(t.value, 10) };
    if (t.kind === 'float') return { kind: 'Float', value: parseFloat(t.value) };
    if (t.kind === 'name') {
      if (t.value === 'true') return { kind: 'Boolean', value: true };
      if (t.value === 'false') return { kind: 'Boolean', value: false };
      if (t.value === 'null') return { kind: 'Null' };
      return { kind: 'Enum', value: t.value };
    }
    throw new Error(`Unexpected value token '${t.value}'`);
  }
}

// ---------------------------------------------------------------------------
// Value resolution (AST value + variables -> JS value)
// ---------------------------------------------------------------------------

type Variables = Record<string, unknown>;

function valueFromAst(node: ValueNode, variables: Variables): unknown {
  switch (node.kind) {
    case 'Variable':
      return variables[node.name];
    case 'String':
    case 'Int':
    case 'Float':
    case 'Boolean':
    case 'Enum':
      return node.value;
    case 'Null':
      return null;
    case 'List':
      return node.values.map((v) => valueFromAst(v, variables));
    case 'Object': {
      const out: Record<string, unknown> = {};
      for (const f of node.fields) out[f.name] = valueFromAst(f.value, variables);
      return out;
    }
  }
}

function argsMap(args: ArgumentNode[], variables: Variables): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const a of args) out[a.name] = valueFromAst(a.value, variables);
  return out;
}

// ---------------------------------------------------------------------------
// Git object helpers (for PR diffs and squash merges)
// ---------------------------------------------------------------------------

function parseCommitTreeSha(content: Buffer): string | null {
  const text = content.toString('utf8');
  const m = /^tree ([0-9a-f]{40})/m.exec(text);
  return m ? (m[1] as string) : null;
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
    const mode = content.toString('utf8', i, space);
    const nul = content.indexOf(0x00, space + 1);
    if (nul === -1) break;
    const name = content.toString('utf8', space + 1, nul);
    const sha = content.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, sha });
    i = nul + 21;
  }
  return entries;
}

function flattenTree(repo: Repo, treeSha: string, prefix: string, out: Map<string, string>): void {
  const obj = repo.objects.get(treeSha);
  if (!obj || obj.type !== 'tree') return;
  for (const entry of parseTree(obj.content)) {
    const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
    if (entry.mode === '40000') {
      flattenTree(repo, entry.sha, path, out);
    } else {
      out.set(path, entry.sha);
    }
  }
}

function treeOfCommit(repo: Repo, commitSha: string): Map<string, string> {
  const out = new Map<string, string>();
  if (!commitSha) return out;
  const obj = repo.objects.get(commitSha);
  if (!obj || obj.type !== 'commit') return out;
  const treeSha = parseCommitTreeSha(obj.content);
  if (!treeSha) return out;
  flattenTree(repo, treeSha, '', out);
  return out;
}

function diffFiles(repo: Repo, baseSha: string, headSha: string): PullFile[] {
  const base = treeOfCommit(repo, baseSha);
  const head = treeOfCommit(repo, headSha);
  const paths = new Set([...base.keys(), ...head.keys()]);
  const files: PullFile[] = [];
  for (const path of [...paths].sort()) {
    const b = base.get(path);
    const h = head.get(path);
    if (b === h) continue;
    if (b === undefined) {
      files.push({ filename: path, status: 'added', additions: 1, deletions: 0, changes: 1 });
    } else if (h === undefined) {
      files.push({ filename: path, status: 'removed', additions: 0, deletions: 1, changes: 1 });
    } else {
      files.push({ filename: path, status: 'modified', additions: 1, deletions: 1, changes: 1 });
    }
  }
  return files;
}

// ---------------------------------------------------------------------------
// Store lookups
// ---------------------------------------------------------------------------

function findRepoByNumericId(id: number): Repo | undefined {
  for (const repo of store.repos.values()) {
    if (repo.id === id) return repo;
  }
  return undefined;
}

function findIssueByGlobalId(id: string): { repo: Repo; issue: Issue } | null {
  const parsed = parseGlobalId(id);
  if (!parsed || parsed.kind !== 'Issue') return null;
  for (const repo of store.repos.values()) {
    const issue = repo.issues.find((x) => x.id === parsed.numericId);
    if (issue) return { repo, issue };
  }
  return null;
}

function findPullByGlobalId(id: string): { repo: Repo; pull: Pull } | null {
  const parsed = parseGlobalId(id);
  if (!parsed || parsed.kind !== 'PullRequest') return null;
  for (const repo of store.repos.values()) {
    const pull = repo.pulls.find((x) => x.id === parsed.numericId);
    if (pull) return { repo, pull };
  }
  return null;
}

function findRepoByGlobalId(id: string): Repo | undefined {
  const parsed = parseGlobalId(id);
  if (!parsed || parsed.kind !== 'Repository') return undefined;
  return findRepoByNumericId(parsed.numericId);
}

function stateOut(state: 'open' | 'closed'): string {
  return state === 'open' ? 'OPEN' : 'CLOSED';
}

// ---------------------------------------------------------------------------
// Cursors + issue connection
// ---------------------------------------------------------------------------

function encodeCursor(number: number): string {
  return Buffer.from(`cursor:${number}`, 'utf8').toString('base64');
}

function decodeCursor(after: unknown): number | null {
  if (typeof after !== 'string' || after === '') return null;
  try {
    const text = Buffer.from(after, 'base64').toString('utf8');
    const m = /^cursor:(\d+)$/.exec(text);
    return m ? Number(m[1]) : null;
  } catch {
    return null;
  }
}

interface IssueConnection {
  __connection: 'issues';
  nodes: Issue[];
  pageInfo: { hasNextPage: boolean; endCursor: string | null; hasPreviousPage: boolean; startCursor: string | null };
}

function buildIssueConnection(repo: Repo, args: Record<string, unknown>): IssueConnection {
  let list = [...repo.issues];
  const states = args['states'];
  if (Array.isArray(states) && states.length > 0) {
    const wanted = new Set(states.map((s) => String(s).toLowerCase()));
    list = list.filter((i) => wanted.has(i.state));
  }
  const orderBy = args['orderBy'] as { field?: unknown; direction?: unknown } | undefined;
  const direction = orderBy && typeof orderBy === 'object' ? String(orderBy.direction ?? 'ASC').toUpperCase() : 'ASC';
  list.sort((a, b) => (direction === 'DESC' ? b.createdAt - a.createdAt : a.createdAt - b.createdAt));
  const afterNum = decodeCursor(args['after']);
  if (afterNum !== null) {
    const idx = list.findIndex((i) => i.number === afterNum);
    if (idx >= 0) list = list.slice(idx + 1);
  }
  const first = typeof args['first'] === 'number' ? args['first'] : undefined;
  const hasNextPage = first !== undefined ? list.length > first : false;
  const page = first !== undefined ? list.slice(0, first) : list;
  const endCursor = page.length > 0 ? encodeCursor((page[page.length - 1] as Issue).number) : null;
  const startCursor = page.length > 0 ? encodeCursor((page[0] as Issue).number) : null;
  return {
    __connection: 'issues',
    nodes: page,
    pageInfo: { hasNextPage, endCursor, hasPreviousPage: afterNum !== null, startCursor },
  };
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

class FieldError extends Error {
  path: (string | number)[];
  constructor(message: string, path: (string | number)[]) {
    super(message);
    this.path = path;
  }
}

interface ExecContext {
  viewerLogin: string;
  variables: Variables;
  fragments: Map<string, FragmentDefinition>;
  errors: { message: string; path?: (string | number)[] }[];
}

interface Resolved {
  value: unknown;
  type: string | null; // GraphQL type of an object value; null for scalars/lists handled by caller
}

function unknownField(typeName: string, field: string, path: (string | number)[]): FieldError {
  return new FieldError(`Cannot query field "${field}" on type "${typeName}".`, [...path, field]);
}

/** Resolve one field on a parent object. Returns the raw value + its object type. */
function resolveField(
  ctx: ExecContext,
  parentType: string,
  parent: unknown,
  field: Extract<SelectionNode, { kind: 'Field' }>,
  args: Record<string, unknown>,
  path: (string | number)[],
): Resolved {
  const name = field.name;

  // Meta fields available on every type.
  if (name === '__typename') return { value: parentType, type: null };

  switch (parentType) {
    case 'Query': {
      if (name === 'viewer') {
        return { value: { login: ctx.viewerLogin }, type: 'User' };
      }
      if (name === 'repository') {
        const repo = store.getRepo(String(args['owner'] ?? ''), String(args['name'] ?? ''));
        return { value: repo ?? null, type: 'Repository' };
      }
      if (name === 'node') {
        const parsed = parseGlobalId(String(args['id'] ?? ''));
        if (!parsed) return { value: null, type: null };
        if (parsed.kind === 'Repository') {
          return { value: findRepoByNumericId(parsed.numericId) ?? null, type: 'Repository' };
        }
        if (parsed.kind === 'Issue') {
          const found = findIssueByGlobalId(String(args['id'] ?? ''));
          return { value: found ? found.issue : null, type: 'Issue' };
        }
        if (parsed.kind === 'PullRequest') {
          const found = findPullByGlobalId(String(args['id'] ?? ''));
          return { value: found ? found.pull : null, type: 'PullRequest' };
        }
        return { value: null, type: null };
      }
      if (name === '__type') {
        const typeName = String(args['name'] ?? '');
        const fields = TYPE_FIELDS[typeName];
        const enums = TYPE_ENUM_VALUES[typeName];
        return {
          value: {
            name: typeName,
            kind: fields ? 'OBJECT' : enums ? 'ENUM' : 'OBJECT',
            fields: fields ? fields.map((f) => ({ name: f })) : null,
            enumValues: enums ? enums.map((e) => ({ name: e })) : null,
          },
          type: '__Type',
        };
      }
      if (name === '__schema') {
        return {
          value: { queryType: { name: 'Query' }, mutationType: { name: 'Mutation' } },
          type: '__Schema',
        };
      }
      throw unknownField('Query', name, path);
    }

    case 'Mutation': {
      if (name === 'createIssue') {
        const input = (args['input'] ?? {}) as Record<string, unknown>;
        const repo = findRepoByGlobalId(String(input['repositoryId'] ?? ''));
        if (!repo) throw new FieldError(`Repository not found for id "${String(input['repositoryId'] ?? '')}".`, [...path, name]);
        const issue: Issue = {
          id: store.nextIssueId++,
          number: ++repo.issueSeq,
          title: String(input['title'] ?? ''),
          body: typeof input['body'] === 'string' ? input['body'] : '',
          state: 'open',
          labels: [],
          createdAt: store.tick(),
          comments: [],
        };
        repo.issues.push(issue);
        return { value: { issue }, type: 'CreateIssuePayload' };
      }
      if (name === 'closeIssue') {
        const input = (args['input'] ?? {}) as Record<string, unknown>;
        const found = findIssueByGlobalId(String(input['issueId'] ?? ''));
        if (!found) throw new FieldError(`Issue not found for id "${String(input['issueId'] ?? '')}".`, [...path, name]);
        found.issue.state = 'closed';
        return { value: { issue: found.issue }, type: 'CloseIssuePayload' };
      }
      if (name === 'createPullRequest') {
        const input = (args['input'] ?? {}) as Record<string, unknown>;
        const repo = findRepoByGlobalId(String(input['repositoryId'] ?? ''));
        if (!repo) throw new FieldError(`Repository not found for id "${String(input['repositoryId'] ?? '')}".`, [...path, name]);
        const headRef = String(input['headRefName'] ?? '');
        const baseRef = String(input['baseRefName'] ?? '');
        const headSha = repo.refs.get(`refs/heads/${headRef}`) ?? '';
        const baseSha = repo.refs.get(`refs/heads/${baseRef}`) ?? '';
        const pull: Pull = {
          id: store.nextPullId++,
          number: ++repo.pullSeq,
          title: String(input['title'] ?? ''),
          body: typeof input['body'] === 'string' ? input['body'] : '',
          state: 'open',
          merged: false,
          mergedAt: null,
          mergeCommitSha: null,
          head: { ref: headRef, sha: headSha },
          base: { ref: baseRef, sha: baseSha },
          files: diffFiles(repo, baseSha, headSha),
          createdAt: store.tick(),
        };
        repo.pulls.push(pull);
        return { value: { pullRequest: pull }, type: 'CreatePullRequestPayload' };
      }
      if (name === 'mergePullRequest') {
        const input = (args['input'] ?? {}) as Record<string, unknown>;
        const found = findPullByGlobalId(String(input['pullRequestId'] ?? ''));
        if (!found) throw new FieldError(`Pull request not found for id "${String(input['pullRequestId'] ?? '')}".`, [...path, name]);
        const { repo, pull } = found;
        if (!pull.merged) {
          // Squash merge: one new commit on the base branch carrying the head's tree.
          const headTree = treeOfCommit(repo, pull.head.sha);
          const baseSha = pull.base.sha;
          let newSha = baseSha;
          if (headTree.size > 0 || baseSha) {
            const headObj = repo.objects.get(pull.head.sha);
            const headTreeSha = headObj && headObj.type === 'commit' ? parseCommitTreeSha(headObj.content) : null;
            if (headTreeSha) {
              const message = `Merge pull request #${pull.number}\n\n${pull.title}\n`;
              const lines = [`tree ${headTreeSha}`];
              if (baseSha) lines.push(`parent ${baseSha}`);
              lines.push(`author ${ctx.viewerLogin} <${ctx.viewerLogin}@example.com> 0 +0000`);
              lines.push(`committer ${ctx.viewerLogin} <${ctx.viewerLogin}@example.com> 0 +0000`);
              lines.push('', message);
              const content = Buffer.from(lines.join('\n'), 'utf8');
              newSha = gitObjectId('commit', content);
              repo.objects.set(newSha, { type: 'commit', content });
              repo.refs.set(`refs/heads/${pull.base.ref}`, newSha);
            }
          }
          pull.merged = true;
          pull.mergedAt = '2026-01-01T00:00:00.000Z';
          pull.mergeCommitSha = newSha;
          pull.state = 'closed';
        }
        return { value: { pullRequest: pull }, type: 'MergePullRequestPayload' };
      }
      if (name === 'addComment') {
        const input = (args['input'] ?? {}) as Record<string, unknown>;
        const found = findIssueByGlobalId(String(input['subjectId'] ?? ''));
        if (!found) throw new FieldError(`Issue not found for id "${String(input['subjectId'] ?? '')}".`, [...path, name]);
        const commentId = store.nextCommentId++;
        const comment = { id: commentId, body: String(input['body'] ?? ''), createdAt: store.tick() };
        found.issue.comments.push(comment);
        return {
          value: { commentEdge: { node: { id: globalId('Comment', commentId), body: comment.body } } },
          type: 'AddCommentPayload',
        };
      }
      throw unknownField('Mutation', name, path);
    }

    case 'Repository': {
      const repo = parent as Repo;
      switch (name) {
        case 'id': return { value: globalId('Repository', repo.id), type: null };
        case 'databaseId': return { value: repo.id, type: null };
        case 'name': return { value: repo.name, type: null };
        case 'nameWithOwner': return { value: repo.fullName, type: null };
        case 'owner': return { value: { login: repo.owner }, type: 'User' };
        case 'defaultBranchRef': return { value: { name: repo.defaultBranch, __repo: repo }, type: 'Ref' };
        case 'viewerPermission': return { value: 'ADMIN', type: null };
        case 'hasIssuesEnabled': return { value: true, type: null };
        case 'issue': {
          const issue = repo.issues.find((i) => i.number === Number(args['number']));
          return { value: issue ?? null, type: 'Issue' };
        }
        case 'issues': return { value: buildIssueConnection(repo, args), type: 'IssueConnection' };
        case 'pullRequests': {
          const first = typeof args['first'] === 'number' ? args['first'] : undefined;
          const nodes = first !== undefined ? repo.pulls.slice(0, first) : [...repo.pulls];
          return {
            value: {
              nodes,
              pageInfo: { hasNextPage: first !== undefined && repo.pulls.length > first, endCursor: null, hasPreviousPage: false, startCursor: null },
            },
            type: 'PullRequestConnection',
          };
        }
        case 'pullRequest': {
          const pull = repo.pulls.find((p) => p.number === Number(args['number']));
          return { value: pull ?? null, type: 'PullRequest' };
        }
        default: throw unknownField('Repository', name, path);
      }
    }

    case 'Issue': {
      const issue = parent as Issue;
      switch (name) {
        case 'id': return { value: globalId('Issue', issue.id), type: null };
        case 'number': return { value: issue.number, type: null };
        case 'title': return { value: issue.title, type: null };
        case 'state': return { value: stateOut(issue.state), type: null };
        case 'body': return { value: issue.body, type: null };
        case 'author': return { value: { login: ctx.viewerLogin }, type: 'User' };
        case 'createdAt': return { value: String(issue.createdAt), type: null };
        case 'url': return { value: '', type: null };
        case 'comments': {
          const first = typeof args['first'] === 'number' ? args['first'] : undefined;
          const nodes = first !== undefined ? issue.comments.slice(0, first) : [...issue.comments];
          return {
            value: {
              nodes: nodes.map((c) => ({ id: globalId('Comment', c.id), body: c.body })),
              pageInfo: { hasNextPage: false, endCursor: null, hasPreviousPage: false, startCursor: null },
            },
            type: 'CommentConnection',
          };
        }
        default: throw unknownField('Issue', name, path);
      }
    }

    case 'PullRequest': {
      const pull = parent as Pull;
      switch (name) {
        case 'id': return { value: globalId('PullRequest', pull.id), type: null };
        case 'number': return { value: pull.number, type: null };
        case 'title': return { value: pull.title, type: null };
        case 'body': return { value: pull.body, type: null };
        case 'state': return { value: stateOut(pull.state), type: null };
        case 'headRefName': return { value: pull.head.ref, type: null };
        case 'baseRefName': return { value: pull.base.ref, type: null };
        case 'mergeable': return { value: 'MERGEABLE', type: null };
        case 'merged': return { value: pull.merged, type: null };
        case 'files': {
          const first = typeof args['first'] === 'number' ? args['first'] : undefined;
          const nodes = (first !== undefined ? pull.files.slice(0, first) : [...pull.files]).map((f) => ({
            path: f.filename,
            additions: f.additions,
            deletions: f.deletions,
            changes: f.changes,
            status: f.status,
          }));
          return {
            value: {
              nodes,
              pageInfo: { hasNextPage: false, endCursor: null, hasPreviousPage: false, startCursor: null },
            },
            type: 'PullRequestFileConnection',
          };
        }
        default: throw unknownField('PullRequest', name, path);
      }
    }

    case 'User': {
      const user = parent as { login: string };
      if (name === 'login') return { value: user.login, type: null };
      if (name === 'id') return { value: globalId('User', 0), type: null };
      throw unknownField('User', name, path);
    }

    case 'Ref': {
      const ref = parent as { name: string; __repo: Repo };
      if (name === 'name') return { value: ref.name, type: null };
      if (name === 'target') {
        const sha = ref.__repo.refs.get(`refs/heads/${ref.name}`) ?? '';
        return { value: { oid: sha }, type: 'GitObject' };
      }
      throw unknownField('Ref', name, path);
    }

    case 'GitObject': {
      const obj = parent as { oid: string };
      if (name === 'oid') return { value: obj.oid, type: null };
      throw unknownField('GitObject', name, path);
    }

    case 'IssueConnection':
    case 'PullRequestConnection':
    case 'CommentConnection':
    case 'PullRequestFileConnection': {
      const conn = parent as { nodes: unknown[]; pageInfo: unknown };
      if (name === 'nodes') {
        const itemType =
          parentType === 'IssueConnection' ? 'Issue'
          : parentType === 'PullRequestConnection' ? 'PullRequest'
          : parentType === 'CommentConnection' ? 'Comment'
          : 'PullRequestFile';
        return { value: conn.nodes, type: `List:${itemType}` };
      }
      if (name === 'edges') {
        const itemType =
          parentType === 'IssueConnection' ? 'Issue'
          : parentType === 'PullRequestConnection' ? 'PullRequest'
          : parentType === 'CommentConnection' ? 'Comment'
          : 'PullRequestFile';
        const nodes = conn.nodes as { number?: number }[];
        return {
          value: nodes.map((n, i) => ({ cursor: encodeCursor(typeof n.number === 'number' ? n.number : i + 1), node: n })),
          type: `List:Edge:${itemType}`,
        };
      }
      if (name === 'pageInfo') return { value: conn.pageInfo, type: 'PageInfo' };
      if (name === 'totalCount') return { value: conn.nodes.length, type: null };
      throw unknownField(parentType, name, path);
    }

    case 'PageInfo': {
      const info = parent as { hasNextPage: boolean; endCursor: string | null; hasPreviousPage: boolean; startCursor: string | null };
      if (name === 'hasNextPage') return { value: info.hasNextPage, type: null };
      if (name === 'endCursor') return { value: info.endCursor, type: null };
      if (name === 'hasPreviousPage') return { value: info.hasPreviousPage, type: null };
      if (name === 'startCursor') return { value: info.startCursor, type: null };
      throw unknownField('PageInfo', name, path);
    }

    case 'CreateIssuePayload':
    case 'CloseIssuePayload': {
      const payload = parent as { issue: Issue };
      if (name === 'issue') return { value: payload.issue, type: 'Issue' };
      throw unknownField(parentType, name, path);
    }

    case 'CreatePullRequestPayload':
    case 'MergePullRequestPayload': {
      const payload = parent as { pullRequest: Pull };
      if (name === 'pullRequest') return { value: payload.pullRequest, type: 'PullRequest' };
      throw unknownField(parentType, name, path);
    }

    case 'AddCommentPayload': {
      const payload = parent as { commentEdge: { node: { id: string; body: string } } };
      if (name === 'commentEdge') return { value: payload.commentEdge, type: 'IssueCommentEdge' };
      throw unknownField('AddCommentPayload', name, path);
    }

    case 'IssueCommentEdge': {
      const edge = parent as { node: { id: string; body: string } };
      if (name === 'node') return { value: edge.node, type: 'Comment' };
      if (name === 'cursor') return { value: '', type: null };
      throw unknownField('IssueCommentEdge', name, path);
    }

    case 'Comment': {
      const comment = parent as { id: string; body: string };
      if (name === 'id') return { value: comment.id, type: null };
      if (name === 'body') return { value: comment.body, type: null };
      throw unknownField('Comment', name, path);
    }

    case 'PullRequestFile': {
      const file = parent as { path: string; additions: number; deletions: number; changes: number; status: string };
      if (name === 'path') return { value: file.path, type: null };
      if (name === 'additions') return { value: file.additions, type: null };
      if (name === 'deletions') return { value: file.deletions, type: null };
      if (name === 'changes') return { value: file.changes, type: null };
      if (name === 'status') return { value: file.status, type: null };
      throw unknownField('PullRequestFile', name, path);
    }

    case '__Type': {
      const t = parent as { name: string; kind: string; fields: { name: string }[] | null; enumValues: { name: string }[] | null };
      if (name === 'name') return { value: t.name, type: null };
      if (name === 'kind') return { value: t.kind, type: null };
      if (name === 'fields') return { value: t.fields, type: t.fields ? 'List:__Field' : null };
      if (name === 'enumValues') return { value: t.enumValues, type: t.enumValues ? 'List:__EnumValue' : null };
      throw unknownField('__Type', name, path);
    }

    case '__Field':
    case '__EnumValue': {
      const f = parent as { name: string };
      if (name === 'name') return { value: f.name, type: null };
      throw unknownField(parentType, name, path);
    }

    case '__Schema': {
      const s = parent as { queryType: { name: string }; mutationType: { name: string } };
      if (name === 'queryType') return { value: s.queryType, type: '__TypeRef' };
      if (name === 'mutationType') return { value: s.mutationType, type: '__TypeRef' };
      throw unknownField('__Schema', name, path);
    }

    case '__TypeRef': {
      const r = parent as { name: string };
      if (name === 'name') return { value: r.name, type: null };
      throw unknownField('__TypeRef', name, path);
    }

    default: {
      // Generic fallback: plain objects resolve their own keys (covers edge nodes).
      const obj = parent as Record<string, unknown>;
      if (obj !== null && typeof obj === 'object' && name in obj) {
        const v = obj[name];
        if (name === 'node' && v !== null && typeof v === 'object') {
          return { value: v, type: 'Comment' };
        }
        return { value: v, type: null };
      }
      throw unknownField(parentType, name, path);
    }
  }
}

/** Resolve a selection set against a parent value of the given type. */
function executeSelectionSet(
  ctx: ExecContext,
  parentType: string,
  parent: unknown,
  selections: SelectionNode[],
  path: (string | number)[],
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const sel of selections) {
    if (sel.kind === 'FragmentSpread') {
      const frag = ctx.fragments.get(sel.name);
      if (!frag) throw new FieldError(`Unknown fragment "${sel.name}".`, path);
      if (frag.typeCondition !== parentType && !(frag.typeCondition === 'Node' && ['Repository', 'Issue', 'PullRequest'].includes(parentType))) {
        continue;
      }
      Object.assign(out, executeSelectionSet(ctx, parentType, parent, frag.selectionSet, path));
      continue;
    }
    if (sel.kind === 'InlineFragment') {
      if (sel.typeCondition !== null && sel.typeCondition !== parentType) continue;
      Object.assign(out, executeSelectionSet(ctx, parentType, parent, sel.selectionSet, path));
      continue;
    }
    const key = sel.alias ?? sel.name;
    const args = argsMap(sel.args, ctx.variables);
    const resolved = resolveField(ctx, parentType, parent, sel, args, path);
    out[key] = completeValue(ctx, resolved, sel.selectionSet, [...path, key]);
  }
  return out;
}

/** Complete a resolved value: apply a sub-selection, or return the scalar as-is. */
function completeValue(
  ctx: ExecContext,
  resolved: Resolved,
  selectionSet: SelectionNode[] | null,
  path: (string | number)[],
): unknown {
  const { value, type } = resolved;
  if (value === null || value === undefined) {
    if (selectionSet) return null;
    return value ?? null;
  }
  if (selectionSet === null) {
    if (value !== null && typeof value === 'object') {
      // Scalar field returned an object (should not happen); serialise defensively.
      return value;
    }
    return value;
  }
  if (Array.isArray(value)) {
    const itemType = type !== null && type.startsWith('List:') ? type.slice('List:'.length) : null;
    if (itemType !== null && itemType.startsWith('Edge:')) {
      const inner = itemType.slice('Edge:'.length);
      return value.map((item, i) => executeSelectionSetWithEdges(ctx, 'Edge', { ...(item as Record<string, unknown>), __innerType: inner }, selectionSet, [...path, i]));
    }
    if (itemType === null) return value;
    return value.map((item, i) => executeSelectionSet(ctx, itemType, item, selectionSet, [...path, i]));
  }
  if (type === null) {
    // Object value with no declared type (e.g. __type fields list items handled via List:).
    throw new FieldError(`Field at ${path.join('.')} has a sub-selection but no object type.`, path);
  }
  return executeSelectionSet(ctx, type, value, selectionSet, path);
}

function executeEdgeSelectionSet(
  ctx: ExecContext,
  parent: { node: unknown; __innerType: string },
  selections: SelectionNode[],
  path: (string | number)[],
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const sel of selections) {
    if (sel.kind !== 'Field') continue;
    const key = sel.alias ?? sel.name;
    if (sel.name === 'node') {
      out[key] = sel.selectionSet
        ? executeSelectionSet(ctx, parent.__innerType, parent.node, sel.selectionSet, [...path, key])
        : parent.node;
    } else if (sel.name === 'cursor') {
      out[key] = (parent as Record<string, unknown>)['cursor'] ?? '';
    } else {
      out[key] = (parent as Record<string, unknown>)[sel.name] ?? null;
    }
  }
  return out;
}

// Edge-aware entry point: executeSelectionSet handles every other parent;
// 'Edge' parents go through executeEdgeSelectionSet so `node` resolves to
// the wrapped item's type instead of a fixed one.
function executeSelectionSetWithEdges(
  ctx: ExecContext,
  parentType: string,
  parent: unknown,
  selections: SelectionNode[],
  path: (string | number)[],
): Record<string, unknown> {
  if (parentType === 'Edge') {
    return executeEdgeSelectionSet(ctx, parent as { node: unknown; __innerType: string }, selections, path);
  }
  return executeSelectionSet(ctx, parentType, parent, selections, path);
}

// ---------------------------------------------------------------------------
// One GraphQL document -> {data, errors}
// ---------------------------------------------------------------------------

interface GraphqlResult {
  data: Record<string, unknown> | null;
  errors?: { message: string; path?: (string | number)[] }[];
}

function executeDocument(entry: { query?: unknown; variables?: unknown; operationName?: unknown }, viewerLogin: string): GraphqlResult {
  const query = entry.query;
  if (typeof query !== 'string' || query.trim() === '') {
    return { data: null, errors: [{ message: 'Must provide query string.' }] };
  }
  let variables: Variables = {};
  if (entry.variables !== undefined && entry.variables !== null) {
    if (typeof entry.variables === 'string') {
      try {
        variables = JSON.parse(entry.variables) as Variables;
      } catch {
        return { data: null, errors: [{ message: 'Variables are invalid JSON.' }] };
      }
    } else if (typeof entry.variables === 'object') {
      variables = entry.variables as Variables;
    }
  }
  const operationName = typeof entry.operationName === 'string' ? entry.operationName : null;

  let definitions: DefinitionNode[];
  try {
    definitions = new Parser(lex(query)).parseDocument();
  } catch (err) {
    return { data: null, errors: [{ message: err instanceof Error ? err.message : String(err) }] };
  }

  const fragments = new Map<string, FragmentDefinition>();
  const operations: OperationDefinition[] = [];
  for (const def of definitions) {
    if (def.kind === 'Fragment') fragments.set(def.name, def);
    else operations.push(def);
  }
  if (operations.length === 0) {
    return { data: null, errors: [{ message: 'No operation found in document.' }] };
  }
  let operation: OperationDefinition | undefined;
  if (operationName !== null) {
    operation = operations.find((o) => o.name === operationName);
    if (!operation) return { data: null, errors: [{ message: `Unknown operation named "${operationName}".` }] };
  } else {
    operation = operations[0];
  }

  // Apply variable defaults.
  const vars: Variables = { ...variables };
  for (const def of operation.variableDefinitions) {
    if (vars[def.name] === undefined && def.defaultValue !== null) {
      vars[def.name] = valueFromAst(def.defaultValue, vars);
    }
  }

  const ctx: ExecContext = { viewerLogin, variables: vars, fragments, errors: [] };
  const rootType = operation.operation === 'mutation' ? 'Mutation' : 'Query';
  try {
    // Edge-aware dispatch: executeSelectionSet handles 'Edge' parents via the
    // patched wrapper below (see completeValue edge branch).
    const data = executeSelectionSetWithEdges(ctx, rootType, {}, operation.selectionSet, []);
    return { data, errors: [] };
  } catch (err) {
    if (err instanceof FieldError) {
      return { data: null, errors: [{ message: err.message, path: err.path }] };
    }
    return { data: null, errors: [{ message: err instanceof Error ? err.message : String(err) }] };
  }
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

function readBody(req: IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on('data', (chunk: Buffer | string) => {
      chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
    });
    req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    req.on('error', reject);
  });
}

function sendJson(res: ServerResponse, status: number, body: unknown): void {
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, {
    'content-type': 'application/json',
    'content-length': Buffer.byteLength(text),
    'x-github-request-id': `req-${randomUUID()}`,
  });
  res.end(text);
}

export async function handleGraphql(
  req: IncomingMessage,
  res: ServerResponse,
  pathname: string,
  method: string,
  ctx: { viewerLogin: string },
): Promise<boolean> {
  if (pathname !== '/api/graphql' && pathname !== '/api/graphql/') return false;
  if (method !== 'POST') return false;

  const expected = process.env.MAKEFUN_SHOP_TOKEN ?? '';
  const auth = req.headers['authorization'] ?? '';
  if (expected !== '' && auth !== `Bearer ${expected}`) {
    sendJson(res, 401, { message: 'Bad credentials' });
    return true;
  }

  const raw = await readBody(req);
  let body: unknown;
  try {
    body = JSON.parse(raw);
  } catch {
    sendJson(res, 400, { message: 'Problems parsing JSON' });
    return true;
  }

  const viewerLogin = ctx.viewerLogin || store.viewerLogin;
  if (Array.isArray(body)) {
    const results = body.map((entry) =>
      entry !== null && typeof entry === 'object'
        ? executeDocument(entry as { query?: unknown; variables?: unknown; operationName?: unknown }, viewerLogin)
        : { data: null, errors: [{ message: 'Batch entry must be an object.' }] },
    );
    sendJson(res, 200, results);
    return true;
  }
  if (body === null || typeof body !== 'object') {
    sendJson(res, 400, { message: 'Problems parsing JSON' });
    return true;
  }
  sendJson(res, 200, executeDocument(body as { query?: unknown; variables?: unknown; operationName?: unknown }, viewerLogin));
  return true;
}
