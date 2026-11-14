// GraphQL: a small document parser and executor for the subset the suite
// sends (the documents gh and github-mcp-server emit). It answers the
// query and mutation fields the checks use, with real data from the store,
// plus the __type introspection they probe and the invalid-field error the
// protocol group requires (HTTP 200 with an errors array naming the field).

import type { ServerResponse } from 'node:http';
import { authorized, jsonBody, type Ctx } from './ctx.ts';
import { sendError, sendJson, sendNotFound, sendUnauthorized } from './respond.ts';
import { VIEWER_ID, VIEWER_LOGIN, type FlatFile, type Issue, type Pull, type Repo, type Store } from './store.ts';
import { b64decode, b64encode } from './util.ts';

// ---------------------------------------------------------- parser

type ValueNode =
  | { kind: 'variable'; name: string }
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; values: ValueNode[] }
  | { kind: 'object'; fields: Record<string, ValueNode> };

interface DirectiveNode {
  name: string;
  args: Record<string, ValueNode>;
}

type SelectionNode =
  | { kind: 'field'; name: string; alias: string | null; args: Record<string, ValueNode>; directives: DirectiveNode[]; selections: SelectionNode[] }
  | { kind: 'fragmentSpread'; name: string; directives: DirectiveNode[] }
  | { kind: 'inlineFragment'; typeCondition: string | null; directives: DirectiveNode[]; selections: SelectionNode[] };

interface OperationNode {
  type: 'query' | 'mutation';
  name: string | null;
  variables: Array<{ name: string; defaultValue: ValueNode | null }>;
  selections: SelectionNode[];
}

interface DocumentNode {
  operations: OperationNode[];
  fragments: Map<string, { typeCondition: string; selections: SelectionNode[] }>;
}

interface Token {
  kind: 'name' | 'string' | 'int' | 'float' | 'punct' | 'spread';
  value: string;
}

function tokenize(source: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  const isNameStart = (c: string): boolean => /[_A-Za-z]/.test(c);
  const isNameChar = (c: string): boolean => /[_0-9A-Za-z]/.test(c);
  while (i < source.length) {
    const c = source[i] as string;
    if (c === ' ' || c === '\t' || c === '\n' || c === '\r' || c === ',') {
      i += 1;
      continue;
    }
    if (c === '#') {
      while (i < source.length && source[i] !== '\n') i += 1;
      continue;
    }
    if (c === '.' && source.slice(i, i + 3) === '...') {
      tokens.push({ kind: 'spread', value: '...' });
      i += 3;
      continue;
    }
    if ('{}()[]:!=$@'.includes(c)) {
      tokens.push({ kind: 'punct', value: c });
      i += 1;
      continue;
    }
    if (c === '"') {
      let value = '';
      i += 1;
      while (i < source.length && source[i] !== '"') {
        const ch = source[i] as string;
        if (ch === '\\') {
          const esc = source[i + 1] as string;
          i += 2;
          if (esc === 'n') value += '\n';
          else if (esc === 't') value += '\t';
          else if (esc === 'r') value += '\r';
          else if (esc === 'b') value += '\b';
          else if (esc === 'f') value += '\f';
          else if (esc === 'u') {
            value += String.fromCharCode(Number.parseInt(source.slice(i, i + 4), 16));
            i += 4;
          } else value += esc;
        } else {
          value += ch;
          i += 1;
        }
      }
      i += 1;
      tokens.push({ kind: 'string', value });
      continue;
    }
    if (c === '-' || /[0-9]/.test(c)) {
      let j = i;
      if (source[j] === '-') j += 1;
      while (j < source.length && /[0-9]/.test(source[j] as string)) j += 1;
      let isFloat = false;
      if (source[j] === '.') {
        isFloat = true;
        j += 1;
        while (j < source.length && /[0-9]/.test(source[j] as string)) j += 1;
      }
      if (source[j] === 'e' || source[j] === 'E') {
        isFloat = true;
        j += 1;
        if (source[j] === '+' || source[j] === '-') j += 1;
        while (j < source.length && /[0-9]/.test(source[j] as string)) j += 1;
      }
      tokens.push({ kind: isFloat ? 'float' : 'int', value: source.slice(i, j) });
      i = j;
      continue;
    }
    if (isNameStart(c)) {
      let j = i;
      while (j < source.length && isNameChar(source[j] as string)) j += 1;
      tokens.push({ kind: 'name', value: source.slice(i, j) });
      i = j;
      continue;
    }
    throw new Error(`unexpected character ${JSON.stringify(c)}`);
  }
  return tokens;
}

class Parser {
  private pos = 0;
  private readonly tokens: Token[];

  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token | undefined {
    return this.tokens[this.pos];
  }

  private next(): Token {
    const token = this.tokens[this.pos];
    if (token === undefined) throw new Error('unexpected end of document');
    this.pos += 1;
    return token;
  }

  private expectPunct(value: string): void {
    const token = this.next();
    if (token.kind !== 'punct' || token.value !== value) throw new Error(`expected ${value}, got ${token.value}`);
  }

  private expectName(value?: string): string {
    const token = this.next();
    if (token.kind !== 'name') throw new Error(`expected a name, got ${token.value}`);
    if (value !== undefined && token.value !== value) throw new Error(`expected ${value}, got ${token.value}`);
    return token.value;
  }

  private isPunct(value: string): boolean {
    const token = this.peek();
    return token !== undefined && token.kind === 'punct' && token.value === value;
  }

  parseDocument(): DocumentNode {
    const operations: OperationNode[] = [];
    const fragments = new Map<string, { typeCondition: string; selections: SelectionNode[] }>();
    while (this.peek() !== undefined) {
      const token = this.peek() as Token;
      if (token.kind === 'name' && token.value === 'fragment') {
        this.next();
        const name = this.expectName();
        this.expectName('on');
        const typeCondition = this.expectName();
        const selections = this.parseSelectionSet();
        fragments.set(name, { typeCondition, selections });
      } else if (token.kind === 'punct' && token.value === '{') {
        operations.push({ type: 'query', name: null, variables: [], selections: this.parseSelectionSet() });
      } else {
        const type = this.expectName();
        if (type !== 'query' && type !== 'mutation') throw new Error(`unsupported operation ${type}`);
        let name: string | null = null;
        const next = this.peek();
        if (next !== undefined && next.kind === 'name') name = this.next().value;
        const variables = this.isPunct('(') ? this.parseVariableDefinitions() : [];
        const selections = this.parseSelectionSet();
        operations.push({ type, name, variables, selections });
      }
    }
    if (operations.length === 0) throw new Error('document has no operations');
    return { operations, fragments };
  }

  private parseVariableDefinitions(): Array<{ name: string; defaultValue: ValueNode | null }> {
    this.expectPunct('(');
    const out: Array<{ name: string; defaultValue: ValueNode | null }> = [];
    while (!this.isPunct(')')) {
      this.expectPunct('$');
      const name = this.expectName();
      this.expectPunct(':');
      this.parseType();
      let defaultValue: ValueNode | null = null;
      if (this.isPunct('=')) {
        this.next();
        defaultValue = this.parseValue();
      }
      out.push({ name, defaultValue });
    }
    this.expectPunct(')');
    return out;
  }

  /** Skip a type reference (named, list, non-null). */
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
    const selections: SelectionNode[] = [];
    while (!this.isPunct('}')) {
      selections.push(this.parseSelection());
    }
    this.expectPunct('}');
    return selections;
  }

  private parseSelection(): SelectionNode {
    const token = this.peek() as Token;
    if (token.kind === 'spread') {
      this.next();
      const next = this.peek();
      if (next !== undefined && next.kind === 'name' && next.value === 'on') {
        this.next();
        const typeCondition = this.expectName();
        const directives = this.parseDirectives();
        return { kind: 'inlineFragment', typeCondition, directives, selections: this.parseSelectionSet() };
      }
      if (next !== undefined && next.kind === 'name') {
        const name = this.next().value;
        const directives = this.parseDirectives();
        return { kind: 'fragmentSpread', name, directives };
      }
      const directives = this.parseDirectives();
      return { kind: 'inlineFragment', typeCondition: null, directives, selections: this.parseSelectionSet() };
    }
    const first = this.expectName();
    let alias: string | null = null;
    let name = first;
    if (this.isPunct(':')) {
      this.next();
      alias = first;
      name = this.expectName();
    }
    const args = this.isPunct('(') ? this.parseArguments() : {};
    const directives = this.parseDirectives();
    const selections = this.isPunct('{') ? this.parseSelectionSet() : [];
    return { kind: 'field', name, alias, args, directives, selections };
  }

  private parseDirectives(): DirectiveNode[] {
    const out: DirectiveNode[] = [];
    while (this.isPunct('@')) {
      this.next();
      const name = this.expectName();
      const args = this.isPunct('(') ? this.parseArguments() : {};
      out.push({ name, args });
    }
    return out;
  }

  private parseArguments(): Record<string, ValueNode> {
    this.expectPunct('(');
    const out: Record<string, ValueNode> = {};
    while (!this.isPunct(')')) {
      const name = this.expectName();
      this.expectPunct(':');
      out[name] = this.parseValue();
    }
    this.expectPunct(')');
    return out;
  }

  private parseValue(): ValueNode {
    const token = this.next();
    if (token.kind === 'punct' && token.value === '$') return { kind: 'variable', name: this.expectName() };
    if (token.kind === 'string') return { kind: 'string', value: token.value };
    if (token.kind === 'int') return { kind: 'int', value: Number(token.value) };
    if (token.kind === 'float') return { kind: 'float', value: Number(token.value) };
    if (token.kind === 'name') {
      if (token.value === 'true') return { kind: 'boolean', value: true };
      if (token.value === 'false') return { kind: 'boolean', value: false };
      if (token.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: token.value };
    }
    if (token.kind === 'punct' && token.value === '[') {
      const values: ValueNode[] = [];
      while (!(this.peek() !== undefined && this.peek()?.kind === 'punct' && this.peek()?.value === ']')) {
        values.push(this.parseValue());
      }
      this.expectPunct(']');
      return { kind: 'list', values };
    }
    if (token.kind === 'punct' && token.value === '{') {
      const fields: Record<string, ValueNode> = {};
      while (!(this.peek() !== undefined && this.peek()?.kind === 'punct' && this.peek()?.value === '}')) {
        const name = this.expectName();
        this.expectPunct(':');
        fields[name] = this.parseValue();
      }
      this.expectPunct('}');
      return { kind: 'object', fields };
    }
    throw new Error(`unexpected value token ${token.value}`);
  }
}

/** Resolve a value node against the operation's variables. */
function resolveValue(node: ValueNode, variables: Record<string, unknown>): unknown {
  switch (node.kind) {
    case 'variable':
      return variables[node.name] ?? null;
    case 'string':
    case 'int':
    case 'float':
    case 'boolean':
      return node.value;
    case 'null':
      return null;
    case 'enum':
      return node.value;
    case 'list':
      return node.values.map((value) => resolveValue(value, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const [key, value] of Object.entries(node.fields)) out[key] = resolveValue(value, variables);
      return out;
    }
  }
}

/** Whether a selection survives its @skip/@include directives. */
function directivesAllow(directives: DirectiveNode[], variables: Record<string, unknown>): boolean {
  for (const directive of directives) {
    const raw = directive.args.if;
    const value = raw === undefined ? true : resolveValue(raw, variables);
    if (directive.name === 'skip' && value === true) return false;
    if (directive.name === 'include' && value !== true) return false;
  }
  return true;
}

// ---------------------------------------------------------- values and schema

/** A value the executor can resolve fields against. */
interface GqlValue {
  __typename: string;
  [key: string]: unknown;
}

interface ExecCtx {
  store: Store;
  errors: Array<{ message: string }>;
}

/** The fields each type answers (anything else is an invalid-field error). */
const SCHEMA: Record<string, Set<string>> = {
  Query: new Set(['viewer', 'repository', 'node', '__type', '__schema']),
  Mutation: new Set(['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment']),
  Repository: new Set(['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'createdAt', 'updatedAt', 'description', 'url', 'isPrivate', 'fork']),
  Issue: new Set(['id', 'databaseId', 'number', 'title', 'state', 'body', 'createdAt', 'updatedAt', 'author', 'labels', 'comments', 'url', 'resourcePath', 'repository']),
  PullRequest: new Set(['id', 'databaseId', 'number', 'title', 'state', 'body', 'merged', 'mergeable', 'headRefName', 'baseRefName', 'headRef', 'baseRef', 'createdAt', 'updatedAt', 'author', 'files', 'url', 'resourcePath', 'repository', 'mergedAt', 'mergeCommitSha']),
  User: new Set(['id', 'login', 'name', 'email', 'url']),
  Ref: new Set(['name', 'target']),
  Label: new Set(['id', 'name', 'color', 'description']),
  LabelConnection: new Set(['nodes', 'edges', 'pageInfo', 'totalCount']),
  IssueConnection: new Set(['nodes', 'edges', 'pageInfo', 'totalCount']),
  PullRequestConnection: new Set(['nodes', 'edges', 'pageInfo', 'totalCount']),
  PullRequestChangedFileConnection: new Set(['nodes', 'edges', 'pageInfo', 'totalCount']),
  IssueCommentConnection: new Set(['nodes', 'edges', 'pageInfo', 'totalCount']),
  PullRequestChangedFile: new Set(['path', 'additions', 'deletions', 'changeType']),
  IssueComment: new Set(['id', 'databaseId', 'body', 'author', 'createdAt', 'updatedAt', 'url']),
  IssueCommentEdge: new Set(['node', 'cursor']),
  LabelEdge: new Set(['node', 'cursor']),
  IssueEdge: new Set(['node', 'cursor']),
  PullRequestEdge: new Set(['node', 'cursor']),
  PullRequestChangedFileEdge: new Set(['node', 'cursor']),
  PageInfo: new Set(['hasNextPage', 'hasPreviousPage', 'startCursor', 'endCursor']),
  __Type: new Set(['name', 'kind', 'fields', 'enumValues', 'description']),
  __Field: new Set(['name', 'description', 'type']),
  __EnumValue: new Set(['name', 'description']),
  __Schema: new Set(['types', 'queryType', 'mutationType']),
  CreateIssuePayload: new Set(['issue', 'clientMutationId']),
  CloseIssuePayload: new Set(['issue', 'clientMutationId']),
  CreatePullRequestPayload: new Set(['pullRequest', 'clientMutationId']),
  MergePullRequestPayload: new Set(['pullRequest', 'clientMutationId']),
  AddCommentPayload: new Set(['commentEdge', 'clientMutationId']),
};

const SEARCH_TYPE_VALUES = ['CODE', 'COMMIT', 'DISCUSSION', 'ISSUE', 'LABEL', 'MILESTONE', 'PULL_REQUEST', 'REPOSITORY', 'USER'];

function viewerValue(): GqlValue {
  return { __typename: 'User', login: VIEWER_LOGIN, id: VIEWER_ID };
}

function repoValue(store: Store, repo: Repo): GqlValue {
  return { __typename: 'Repository', store, repo };
}

function issueValue(store: Store, repo: Repo, issue: Issue): GqlValue {
  return { __typename: 'Issue', store, repo, issue };
}

function pullValue(store: Store, repo: Repo, pr: Pull): GqlValue {
  return { __typename: 'PullRequest', store, repo, pr };
}

function cursorOf(kind: string, number: number): string {
  return b64encode(`${kind}:${number}`);
}

function pageInfoOf<T>(nodes: T[], cursorOfNode: (node: T) => string, hasMore: boolean): GqlValue {
  return {
    __typename: 'PageInfo',
    hasNextPage: hasMore,
    hasPreviousPage: false,
    startCursor: nodes.length === 0 ? null : cursorOfNode(nodes[0] as T),
    endCursor: nodes.length === 0 ? null : cursorOfNode(nodes[nodes.length - 1] as T),
  };
}

/** Order issues by an orderBy argument, then paginate by first/after. */
function issueConnection(store: Store, repo: Repo, args: Record<string, unknown>): GqlValue {
  let issues = [...repo.issues];
  const states = args.states as string[] | undefined;
  if (Array.isArray(states) && states.length > 0) {
    issues = issues.filter((issue) => states.includes(issue.state.toUpperCase()));
  }
  const orderBy = (args.orderBy as { field?: string; direction?: string } | undefined) ?? {};
  const direction = orderBy.direction === 'ASC' ? 1 : -1;
  const keyOf = (issue: Issue): number => {
    if (orderBy.field === 'UPDATED_AT') return issue.updatedSeq;
    if (orderBy.field === 'COMMENTS') return issue.comments.length;
    return issue.createdSeq;
  };
  issues.sort((a, b) => {
    const diff = keyOf(a) - keyOf(b);
    if (diff !== 0) return diff * direction;
    return (a.number - b.number) * direction;
  });
  const first = typeof args.first === 'number' ? Math.max(0, Math.floor(args.first)) : issues.length;
  const after = typeof args.after === 'string' ? args.after : null;
  let start = 0;
  if (after !== null) {
    const decoded = b64decode(after).toString('utf8');
    const match = /^I:(\d+)$/.exec(decoded);
    if (match !== null) {
      const index = issues.findIndex((issue) => issue.number === Number(match[1]));
      if (index >= 0) start = index + 1;
    }
  }
  const page = issues.slice(start, start + first);
  const nodes = page.map((issue) => issueValue(store, repo, issue));
  return {
    __typename: 'IssueConnection',
    nodes,
    totalCount: issues.length,
    pageInfo: pageInfoOf(nodes, (node) => cursorOf('I', (node.issue as Issue).number), start + first < issues.length),
  };
}

function pullConnection(store: Store, repo: Repo, args: Record<string, unknown>): GqlValue {
  const pulls = [...repo.pulls].sort((a, b) => b.number - a.number);
  const first = typeof args.first === 'number' ? Math.max(0, Math.floor(args.first)) : pulls.length;
  const page = pulls.slice(0, first);
  const nodes = page.map((pr) => pullValue(store, repo, pr));
  return {
    __typename: 'PullRequestConnection',
    nodes,
    totalCount: pulls.length,
    pageInfo: pageInfoOf(nodes, (node) => cursorOf('P', (node.pr as Pull).number), first < pulls.length),
  };
}

function commentConnection(store: Store, repo: Repo, issue: Issue): GqlValue {
  const nodes = issue.comments.map((comment) => ({
    __typename: 'IssueComment',
    store,
    repo,
    issue,
    comment,
    id: store.commentGlobalId(repo, issue, comment),
  }));
  return {
    __typename: 'IssueCommentConnection',
    nodes,
    totalCount: nodes.length,
    pageInfo: pageInfoOf(nodes, (node) => cursorOf('C', (node.comment as { id: number }).id), false),
  };
}

function labelConnection(store: Store, repo: Repo, issue: Issue): GqlValue {
  const nodes = issue.labels
    .map((name) => repo.labels.get(name))
    .filter((label): label is NonNullable<typeof label> => label !== undefined)
    .map((label) => ({ __typename: 'Label', id: b64encode(`L:${repo.owner}/${repo.name}:${label.name}`), ...label }));
  return {
    __typename: 'LabelConnection',
    nodes,
    totalCount: nodes.length,
    pageInfo: pageInfoOf(nodes, (node) => b64encode(`L:${node.name as string}`), false),
  };
}

function filesConnection(store: Store, repo: Repo, pr: Pull, args: Record<string, unknown>): GqlValue {
  const files = store.pullFiles(repo, pr);
  const first = typeof args.first === 'number' ? Math.max(0, Math.floor(args.first)) : files.length;
  const page = files.slice(0, first);
  const nodes = page.map((file) => ({
    __typename: 'PullRequestChangedFile',
    path: file.filename,
    additions: file.additions,
    deletions: file.deletions,
    changeType: file.status.toUpperCase(),
  }));
  return {
    __typename: 'PullRequestChangedFileConnection',
    nodes,
    totalCount: files.length,
    pageInfo: pageInfoOf(nodes, (node) => b64encode(`F:${node.path as string}`), first < files.length),
  };
}

// ---------------------------------------------------------- resolvers

type Resolver = (parent: GqlValue, args: Record<string, unknown>, ctx: ExecCtx) => unknown;

function repoUrl(repo: Repo): string {
  return `https://github.com/${repo.owner}/${repo.name}`;
}

function doSquashMerge(ctx: ExecCtx, repo: Repo, pr: Pull): boolean {
  const headSha = repo.refs.get(`refs/heads/${pr.headRef}`);
  const baseSha = repo.refs.get(`refs/heads/${pr.baseRef}`);
  if (headSha === undefined || baseSha === undefined) return false;
  const merged = ctx.store.threeWayMerge(repo, baseSha, headSha);
  if (merged.conflict) return false;
  const treeOid = ctx.store.buildTreeFromFlat(repo, merged.flat);
  const mergeSha = ctx.store.createCommit(repo, treeOid, [baseSha], pr.title);
  repo.refs.set(`refs/heads/${pr.baseRef}`, mergeSha);
  pr.state = 'closed';
  pr.merged = true;
  pr.mergedAt = ctx.store.isoSec(ctx.store.nowSec());
  pr.mergeCommitSha = mergeSha;
  pr.baseSha = mergeSha;
  return true;
}

const RESOLVERS: Record<string, Record<string, Resolver>> = {
  Query: {
    viewer: () => viewerValue(),
    repository: (parent, args) => {
      const store = parent.store as Store;
      const owner = args.owner as string | undefined;
      const name = args.name as string | undefined;
      if (owner !== VIEWER_LOGIN || typeof name !== 'string') return null;
      const repo = store.getRepo(owner, name);
      return repo === undefined ? null : repoValue(store, repo);
    },
    node: (parent, args) => {
      const store = parent.store as Store;
      const id = args.id as string | undefined;
      if (typeof id !== 'string') return null;
      const resolved = store.resolveGlobalId(id);
      if (resolved === null) return null;
      if (resolved.kind === 'repo') return repoValue(store, resolved.repo);
      if (resolved.kind === 'issue') return issueValue(store, resolved.repo, resolved.issue);
      return pullValue(store, resolved.repo, resolved.pull);
    },
    __type: (_parent, args) => {
      const name = args.name as string | undefined;
      if (name === 'SearchType') {
        return {
          __typename: '__Type',
          name: 'SearchType',
          kind: 'ENUM',
          fields: null,
          enumValues: SEARCH_TYPE_VALUES.map((value) => ({ __typename: '__EnumValue', name: value, description: null })),
        };
      }
      // Every object type in the schema answers its field names; an unknown
      // name gets an empty list rather than an error.
      const fields = name !== undefined ? SCHEMA[name] : undefined;
      if (fields !== undefined) {
        return {
          __typename: '__Type',
          name,
          kind: 'OBJECT',
          fields: [...fields].map((field) => ({ __typename: '__Field', name: field, description: null })),
          enumValues: null,
        };
      }
      return { __typename: '__Type', name: name ?? null, kind: 'OBJECT', fields: [], enumValues: null };
    },
    __schema: () => ({ __typename: '__Schema', types: [], queryType: { __typename: '__Type', name: 'Query' }, mutationType: { __typename: '__Type', name: 'Mutation' } }),
  },
  Mutation: {
    createIssue: (parent, args) => {
      const store = parent.store as Store;
      const input = (args.input as Record<string, unknown> | undefined) ?? {};
      const resolved = typeof input.repositoryId === 'string' ? store.resolveGlobalId(input.repositoryId) : null;
      if (resolved === null || resolved.kind !== 'repo') throw new Error('Repository not found');
      const title = typeof input.title === 'string' ? input.title : '';
      if (title === '') throw new Error('title is required');
      const issue = store.createIssue(resolved.repo, title, typeof input.body === 'string' ? input.body : '', []);
      return {
        __typename: 'CreateIssuePayload',
        issue: issueValue(store, resolved.repo, issue),
        clientMutationId: typeof input.clientMutationId === 'string' ? input.clientMutationId : null,
      };
    },
    closeIssue: (parent, args) => {
      const store = parent.store as Store;
      const input = (args.input as Record<string, unknown> | undefined) ?? {};
      const resolved = typeof input.issueId === 'string' ? store.resolveGlobalId(input.issueId) : null;
      if (resolved === null || resolved.kind !== 'issue') throw new Error('Issue not found');
      resolved.issue.state = 'closed';
      store.touchIssue(resolved.repo, resolved.issue);
      return {
        __typename: 'CloseIssuePayload',
        issue: issueValue(store, resolved.repo, resolved.issue),
        clientMutationId: typeof input.clientMutationId === 'string' ? input.clientMutationId : null,
      };
    },
    createPullRequest: (parent, args) => {
      const store = parent.store as Store;
      const input = (args.input as Record<string, unknown> | undefined) ?? {};
      const resolved = typeof input.repositoryId === 'string' ? store.resolveGlobalId(input.repositoryId) : null;
      if (resolved === null || resolved.kind !== 'repo') throw new Error('Repository not found');
      const head = typeof input.headRefName === 'string' ? input.headRefName : '';
      const base = typeof input.baseRefName === 'string' ? input.baseRefName : '';
      const headSha = resolved.repo.refs.get(`refs/heads/${head}`);
      const baseSha = resolved.repo.refs.get(`refs/heads/${base}`);
      if (headSha === undefined || baseSha === undefined) throw new Error('head or base branch does not exist');
      const title = typeof input.title === 'string' ? input.title : '';
      const pr = store.createPull(resolved.repo, title, typeof input.body === 'string' ? input.body : '', head, headSha, base, baseSha);
      return {
        __typename: 'CreatePullRequestPayload',
        pullRequest: pullValue(store, resolved.repo, pr),
        clientMutationId: typeof input.clientMutationId === 'string' ? input.clientMutationId : null,
      };
    },
    mergePullRequest: (parent, args, ctx) => {
      const store = parent.store as Store;
      const input = (args.input as Record<string, unknown> | undefined) ?? {};
      const resolved = typeof input.pullRequestId === 'string' ? store.resolveGlobalId(input.pullRequestId) : null;
      if (resolved === null || resolved.kind !== 'pull') throw new Error('Pull request not found');
      if (resolved.pull.merged) throw new Error('Pull request already merged');
      if (resolved.pull.state !== 'open') throw new Error('Pull request is closed');
      if (!doSquashMerge(ctx, resolved.repo, resolved.pull)) throw new Error('Pull request has conflicts');
      return {
        __typename: 'MergePullRequestPayload',
        pullRequest: pullValue(store, resolved.repo, resolved.pull),
        clientMutationId: typeof input.clientMutationId === 'string' ? input.clientMutationId : null,
      };
    },
    addComment: (parent, args) => {
      const store = parent.store as Store;
      const input = (args.input as Record<string, unknown> | undefined) ?? {};
      const resolved = typeof input.subjectId === 'string' ? store.resolveGlobalId(input.subjectId) : null;
      if (resolved === null || resolved.kind !== 'issue') throw new Error('Subject not found');
      const body = typeof input.body === 'string' ? input.body : '';
      const comment = store.addComment(resolved.repo, resolved.issue, body);
      const node = {
        __typename: 'IssueComment',
        store,
        repo: resolved.repo,
        issue: resolved.issue,
        comment,
        id: store.commentGlobalId(resolved.repo, resolved.issue, comment),
      };
      return {
        __typename: 'AddCommentPayload',
        commentEdge: { __typename: 'IssueCommentEdge', node, cursor: cursorOf('C', comment.id) },
        clientMutationId: typeof input.clientMutationId === 'string' ? input.clientMutationId : null,
      };
    },
  },
  Repository: {
    id: (parent) => (parent.store as Store).repoGlobalId(parent.repo as Repo),
    databaseId: (parent) => (parent.repo as Repo).id,
    name: (parent) => (parent.repo as Repo).name,
    nameWithOwner: (parent) => `${(parent.repo as Repo).owner}/${(parent.repo as Repo).name}`,
    owner: () => viewerValue(),
    defaultBranchRef: (parent) => {
      const repo = parent.repo as Repo;
      const head = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
      return head === undefined ? null : { __typename: 'Ref', name: repo.defaultBranch, target: head };
    },
    viewerPermission: () => 'ADMIN',
    hasIssuesEnabled: () => true,
    issue: (parent, args) => {
      const store = parent.store as Store;
      const repo = parent.repo as Repo;
      const number = args.number as number | undefined;
      if (typeof number !== 'number') return null;
      const issue = store.findIssue(repo, number);
      return issue === undefined ? null : issueValue(store, repo, issue);
    },
    issues: (parent, args) => issueConnection(parent.store as Store, parent.repo as Repo, args),
    pullRequests: (parent, args) => pullConnection(parent.store as Store, parent.repo as Repo, args),
    pullRequest: (parent, args) => {
      const store = parent.store as Store;
      const repo = parent.repo as Repo;
      const number = args.number as number | undefined;
      if (typeof number !== 'number') return null;
      const pr = store.findPull(repo, number);
      return pr === undefined ? null : pullValue(store, repo, pr);
    },
    createdAt: (parent) => (parent.store as Store).isoFor((parent.repo as Repo).createdSeq),
    updatedAt: (parent) => (parent.store as Store).isoFor((parent.repo as Repo).createdSeq),
    description: (parent) => (parent.repo as Repo).description,
    url: (parent) => repoUrl(parent.repo as Repo),
    isPrivate: (parent) => (parent.repo as Repo).private,
    fork: () => false,
  },
  Issue: {
    id: (parent) => (parent.store as Store).issueGlobalId(parent.repo as Repo, parent.issue as Issue),
    databaseId: (parent) => (parent.issue as Issue).id,
    number: (parent) => (parent.issue as Issue).number,
    title: (parent) => (parent.issue as Issue).title,
    state: (parent) => (parent.issue as Issue).state.toUpperCase(),
    body: (parent) => (parent.issue as Issue).body,
    createdAt: (parent) => (parent.store as Store).isoFor((parent.issue as Issue).createdSeq),
    updatedAt: (parent) => (parent.store as Store).isoFor((parent.issue as Issue).updatedSeq),
    author: () => viewerValue(),
    labels: (parent) => labelConnection(parent.store as Store, parent.repo as Repo, parent.issue as Issue),
    comments: (parent) => commentConnection(parent.store as Store, parent.repo as Repo, parent.issue as Issue),
    url: (parent) => `${repoUrl(parent.repo as Repo)}/issues/${(parent.issue as Issue).number}`,
    resourcePath: (parent) => `/${(parent.repo as Repo).owner}/${(parent.repo as Repo).name}/issues/${(parent.issue as Issue).number}`,
    repository: (parent) => repoValue(parent.store as Store, parent.repo as Repo),
  },
  PullRequest: {
    id: (parent) => (parent.store as Store).pullGlobalId(parent.repo as Repo, parent.pr as Pull),
    databaseId: (parent) => (parent.pr as Pull).id,
    number: (parent) => (parent.pr as Pull).number,
    title: (parent) => (parent.pr as Pull).title,
    state: (parent) => {
      const pr = parent.pr as Pull;
      return pr.merged ? 'MERGED' : pr.state.toUpperCase();
    },
    body: (parent) => (parent.pr as Pull).body,
    merged: (parent) => (parent.pr as Pull).merged,
    mergeable: (parent) => {
      const store = parent.store as Store;
      const repo = parent.repo as Repo;
      const pr = parent.pr as Pull;
      return store.mergeable(repo, pr) ? 'MERGEABLE' : 'CONFLICTING';
    },
    headRefName: (parent) => (parent.pr as Pull).headRef,
    baseRefName: (parent) => (parent.pr as Pull).baseRef,
    headRef: (parent) => ({ __typename: 'Ref', name: (parent.pr as Pull).headRef, target: (parent.pr as Pull).headSha }),
    baseRef: (parent) => ({ __typename: 'Ref', name: (parent.pr as Pull).baseRef, target: (parent.pr as Pull).baseSha }),
    createdAt: (parent) => (parent.store as Store).isoFor((parent.pr as Pull).createdSeq),
    updatedAt: (parent) => (parent.store as Store).isoFor((parent.pr as Pull).createdSeq),
    author: () => viewerValue(),
    files: (parent, args) => filesConnection(parent.store as Store, parent.repo as Repo, parent.pr as Pull, args),
    url: (parent) => `${repoUrl(parent.repo as Repo)}/pull/${(parent.pr as Pull).number}`,
    resourcePath: (parent) => `/${(parent.repo as Repo).owner}/${(parent.repo as Repo).name}/pull/${(parent.pr as Pull).number}`,
    repository: (parent) => repoValue(parent.store as Store, parent.repo as Repo),
    mergedAt: (parent) => (parent.pr as Pull).mergedAt,
    mergeCommitSha: (parent) => (parent.pr as Pull).mergeCommitSha,
  },
  User: {
    id: (parent) => parent.id as number,
    login: (parent) => parent.login as string,
    name: () => 'ada iversen',
    email: () => 'adaiversen@example.invalid',
    url: (parent) => `https://github.com/${parent.login as string}`,
  },
  Ref: {
    name: (parent) => parent.name as string,
    target: (parent) => parent.target as string | undefined ?? null,
  },
  Label: {
    id: (parent) => parent.id as string,
    name: (parent) => parent.name as string,
    color: (parent) => parent.color as string,
    description: (parent) => parent.description as string,
  },
  IssueConnection: {
    nodes: (parent) => parent.nodes as unknown[],
    edges: (parent) => (parent.nodes as GqlValue[]).map((node) => ({ __typename: 'IssueEdge', node, cursor: cursorOf('I', (node.issue as Issue).number) })),
    pageInfo: (parent) => parent.pageInfo as GqlValue,
    totalCount: (parent) => parent.totalCount as number,
  },
  PullRequestConnection: {
    nodes: (parent) => parent.nodes as unknown[],
    edges: (parent) => (parent.nodes as GqlValue[]).map((node) => ({ __typename: 'PullRequestEdge', node, cursor: cursorOf('P', (node.pr as Pull).number) })),
    pageInfo: (parent) => parent.pageInfo as GqlValue,
    totalCount: (parent) => parent.totalCount as number,
  },
  PullRequestChangedFileConnection: {
    nodes: (parent) => parent.nodes as unknown[],
    edges: (parent) => (parent.nodes as GqlValue[]).map((node) => ({ __typename: 'PullRequestChangedFileEdge', node, cursor: b64encode(`F:${node.path as string}`) })),
    pageInfo: (parent) => parent.pageInfo as GqlValue,
    totalCount: (parent) => parent.totalCount as number,
  },
  IssueCommentConnection: {
    nodes: (parent) => parent.nodes as unknown[],
    edges: (parent) => (parent.nodes as GqlValue[]).map((node) => ({ __typename: 'IssueCommentEdge', node, cursor: cursorOf('C', (node.comment as { id: number }).id) })),
    pageInfo: (parent) => parent.pageInfo as GqlValue,
    totalCount: (parent) => parent.totalCount as number,
  },
  LabelConnection: {
    nodes: (parent) => parent.nodes as unknown[],
    edges: (parent) => (parent.nodes as GqlValue[]).map((node) => ({ __typename: 'LabelEdge', node, cursor: b64encode(`L:${node.name as string}`) })),
    pageInfo: (parent) => parent.pageInfo as GqlValue,
    totalCount: (parent) => parent.totalCount as number,
  },
  PullRequestChangedFile: {
    path: (parent) => parent.path as string,
    additions: (parent) => parent.additions as number,
    deletions: (parent) => parent.deletions as number,
    changeType: (parent) => parent.changeType as string,
  },
  IssueComment: {
    id: (parent) => parent.id as string,
    databaseId: (parent) => (parent.comment as { id: number }).id,
    body: (parent) => (parent.comment as { body: string }).body,
    author: () => viewerValue(),
    createdAt: (parent) => (parent.store as Store).isoFor((parent.comment as { createdSeq: number }).createdSeq),
    updatedAt: (parent) => (parent.store as Store).isoFor((parent.comment as { createdSeq: number }).createdSeq),
    url: (parent) => `${repoUrl(parent.repo as Repo)}/issues/${(parent.issue as Issue).number}#issuecomment-${(parent.comment as { id: number }).id}`,
  },
  IssueCommentEdge: {
    node: (parent) => parent.node as GqlValue,
    cursor: (parent) => parent.cursor as string,
  },
  LabelEdge: {
    node: (parent) => parent.node as GqlValue,
    cursor: (parent) => parent.cursor as string,
  },
  IssueEdge: {
    node: (parent) => parent.node as GqlValue,
    cursor: (parent) => parent.cursor as string,
  },
  PullRequestEdge: {
    node: (parent) => parent.node as GqlValue,
    cursor: (parent) => parent.cursor as string,
  },
  PullRequestChangedFileEdge: {
    node: (parent) => parent.node as GqlValue,
    cursor: (parent) => parent.cursor as string,
  },
  PageInfo: {
    hasNextPage: (parent) => parent.hasNextPage as boolean,
    hasPreviousPage: (parent) => parent.hasPreviousPage as boolean,
    startCursor: (parent) => parent.startCursor as string | null,
    endCursor: (parent) => parent.endCursor as string | null,
  },
  __Type: {
    name: (parent) => parent.name as string | null,
    kind: (parent) => parent.kind as string,
    fields: (parent) => (parent.fields as unknown[] | null) ?? null,
    enumValues: (parent) => (parent.enumValues as unknown[] | null) ?? null,
    description: () => null,
  },
  __Field: {
    name: (parent) => parent.name as string,
    description: (parent) => (parent.description as string | null) ?? null,
    type: () => ({ __typename: '__Type', name: 'String', kind: 'SCALAR', fields: null, enumValues: null }),
  },
  __EnumValue: {
    name: (parent) => parent.name as string,
    description: (parent) => (parent.description as string | null) ?? null,
  },
  __Schema: {
    types: (parent) => parent.types as unknown[],
    queryType: (parent) => parent.queryType as GqlValue,
    mutationType: (parent) => parent.mutationType as GqlValue,
  },
  CreateIssuePayload: {
    issue: (parent) => parent.issue as GqlValue,
    clientMutationId: (parent) => parent.clientMutationId as string | null,
  },
  CloseIssuePayload: {
    issue: (parent) => parent.issue as GqlValue,
    clientMutationId: (parent) => parent.clientMutationId as string | null,
  },
  CreatePullRequestPayload: {
    pullRequest: (parent) => parent.pullRequest as GqlValue,
    clientMutationId: (parent) => parent.clientMutationId as string | null,
  },
  MergePullRequestPayload: {
    pullRequest: (parent) => parent.pullRequest as GqlValue,
    clientMutationId: (parent) => parent.clientMutationId as string | null,
  },
  AddCommentPayload: {
    commentEdge: (parent) => parent.commentEdge as GqlValue,
    clientMutationId: (parent) => parent.clientMutationId as string | null,
  },
};

// ---------------------------------------------------------- executor

/** Execute a selection set against a value, collecting field results. */
function executeSelections(
  parent: GqlValue,
  selections: SelectionNode[],
  variables: Record<string, unknown>,
  fragments: Map<string, { typeCondition: string; selections: SelectionNode[] }>,
  ctx: ExecCtx,
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const selection of selections) {
    if (!directivesAllow(selection.directives, variables)) continue;
    if (selection.kind === 'fragmentSpread') {
      const fragment = fragments.get(selection.name);
      if (fragment === undefined) {
        ctx.errors.push({ message: `Unknown fragment "${selection.name}".` });
        continue;
      }
      if (fragment.typeCondition !== parent.__typename) continue;
      Object.assign(out, executeSelections(parent, fragment.selections, variables, fragments, ctx));
      continue;
    }
    if (selection.kind === 'inlineFragment') {
      if (selection.typeCondition !== null && selection.typeCondition !== parent.__typename) continue;
      Object.assign(out, executeSelections(parent, selection.selections, variables, fragments, ctx));
      continue;
    }
    const key = selection.alias ?? selection.name;
    if (selection.name === '__typename') {
      out[key] = parent.__typename;
      continue;
    }
    const allowed = SCHEMA[parent.__typename];
    if (allowed === undefined || !allowed.has(selection.name)) {
      ctx.errors.push({ message: `Cannot query field "${selection.name}" on type "${parent.__typename}".` });
      out[key] = null;
      continue;
    }
    const resolver = RESOLVERS[parent.__typename]?.[selection.name];
    if (resolver === undefined) {
      ctx.errors.push({ message: `Cannot query field "${selection.name}" on type "${parent.__typename}".` });
      out[key] = null;
      continue;
    }
    const args: Record<string, unknown> = {};
    for (const [argName, argNode] of Object.entries(selection.args)) args[argName] = resolveValue(argNode, variables);
    let value: unknown;
    try {
      value = resolver(parent, args, ctx);
    } catch (err) {
      ctx.errors.push({ message: err instanceof Error ? err.message : 'resolver failed' });
      out[key] = null;
      continue;
    }
    if (selection.selections.length === 0) {
      out[key] = value;
      continue;
    }
    if (value === null || value === undefined) {
      out[key] = null;
      continue;
    }
    if (Array.isArray(value)) {
      out[key] = value.map((item) =>
        item !== null && typeof item === 'object' && '__typename' in (item as Record<string, unknown>)
          ? executeSelections(item as GqlValue, selection.selections, variables, fragments, ctx)
          : null,
      );
      continue;
    }
    if (typeof value === 'object' && '__typename' in (value as Record<string, unknown>)) {
      out[key] = executeSelections(value as GqlValue, selection.selections, variables, fragments, ctx);
      continue;
    }
    ctx.errors.push({ message: `Field "${selection.name}" must not have a selection since type "${typeof value}" has no subfields.` });
    out[key] = null;
  }
  return out;
}

/** Execute one operation document against the store. */
function executeOperation(
  store: Store,
  document: DocumentNode,
  operationName: string | undefined,
  requestVariables: Record<string, unknown>,
): { data: Record<string, unknown> | null; errors?: Array<{ message: string }> } {
  const errors: Array<{ message: string }> = [];
  let operation = document.operations[0] as OperationNode | undefined;
  if (operationName !== undefined) {
    operation = document.operations.find((op) => op.name === operationName);
    if (operation === undefined) {
      return { data: null, errors: [{ message: `Unknown operation named "${operationName}".` }] };
    }
  } else if (document.operations.length > 1) {
    return { data: null, errors: [{ message: 'Must provide operation name if query contains multiple operations.' }] };
  }
  if (operation === undefined) return { data: null, errors: [{ message: 'No operation found.' }] };
  const variables: Record<string, unknown> = {};
  for (const definition of operation.variables) {
    if (definition.name in requestVariables) variables[definition.name] = requestVariables[definition.name];
    else if (definition.defaultValue !== null) variables[definition.name] = resolveValue(definition.defaultValue, variables);
    else variables[definition.name] = null;
  }
  const ctx: ExecCtx = { store, errors };
  const root: GqlValue =
    operation.type === 'mutation'
      ? { __typename: 'Mutation', store }
      : { __typename: 'Query', store };
  const data = executeSelections(root, operation.selections, variables, document.fragments, ctx);
  return errors.length > 0 ? { data, errors } : { data };
}

/** Execute one request body (a single operation, or a batch array). */
function executeBody(store: Store, body: unknown): unknown {
  if (Array.isArray(body)) {
    return body.map((item) => executeOne(store, item));
  }
  return executeOne(store, body);
}

function executeOne(store: Store, body: unknown): unknown {
  if (body === null || typeof body !== 'object' || Array.isArray(body)) {
    return { data: null, errors: [{ message: 'Request body must be a JSON object with a query string.' }] };
  }
  const record = body as Record<string, unknown>;
  const query = record.query;
  if (typeof query !== 'string' || query.trim() === '') {
    return { data: null, errors: [{ message: 'Must provide query string.' }] };
  }
  let document: DocumentNode;
  try {
    document = new Parser(tokenize(query)).parseDocument();
  } catch (err) {
    return { data: null, errors: [{ message: err instanceof Error ? err.message : 'Failed to parse query.' }] };
  }
  const operationName = typeof record.operationName === 'string' ? record.operationName : undefined;
  const variables =
    record.variables !== null && typeof record.variables === 'object' && !Array.isArray(record.variables)
      ? (record.variables as Record<string, unknown>)
      : {};
  return executeOperation(store, document, operationName, variables);
}

// ---------------------------------------------------------- handler

/** Handle POST /api/graphql (and a GET with ?query=, for completeness). */
export function handleGraphql(ctx: Ctx, res: ServerResponse): boolean {
  if (ctx.path !== '/api/graphql') return false;
  if (!authorized(ctx)) {
    sendUnauthorized(res);
    return true;
  }
  if (ctx.method === 'GET') {
    const query = ctx.query.get('query');
    if (query === null) {
      sendError(res, 400, 'Must provide query string.');
      return true;
    }
    const result = executeBody(ctx.store, { query, variables: {} });
    sendJson(res, 200, result);
    return true;
  }
  if (ctx.method !== 'POST') {
    sendNotFound(res);
    return true;
  }
  const body = jsonBody(ctx);
  if (body === undefined) {
    sendError(res, 400, 'Invalid JSON body');
    return true;
  }
  const result = executeBody(ctx.store, body);
  sendJson(res, 200, result);
  return true;
}
