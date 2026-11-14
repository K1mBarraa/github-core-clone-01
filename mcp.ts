// MCP: JSON-RPC 2.0 over Streamable HTTP at /mcp. The modern 2026-07-28
// surface (server/discover, then tools/list and tools/call) and the legacy
// initialize handshake both work. The tools act on the same store as REST
// and GraphQL, so a repository created by one surface is visible to the
// others. A missing or wrong bearer token is 401; a body that is not JSON
// is HTTP 400 with a -32700 parse error.

import type { ServerResponse } from 'node:http';
import { authorized, type Ctx } from './ctx.ts';
import { baseHeaders, sendJson, sendNotFound, sendUnauthorized } from './respond.ts';
import { VIEWER_LOGIN, type Repo } from './store.ts';
import { jsonText } from './util.ts';

const SERVER_NAME = 'github-core-clone-forge';
const SERVER_VERSION = '1.0.0';
const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];

interface ToolDef {
  name: string;
  description: string;
  inputSchema: Record<string, unknown>;
  call: (ctx: Ctx, args: Record<string, unknown>) => unknown;
}

/** The first defined argument among several names (the client maps synonyms). */
function pickArg(args: Record<string, unknown>, ...names: string[]): unknown {
  for (const name of names) {
    const value = args[name];
    if (value !== undefined && value !== null) return value;
  }
  return undefined;
}

/** Split an owner/repo pair out of the arguments (full_name or owner+repo). */
function ownerRepo(args: Record<string, unknown>): { owner: string; repo: string } {
  const fullName = pickArg(args, 'full_name', 'fullName', 'repository') as string | undefined;
  if (typeof fullName === 'string' && fullName.includes('/')) {
    const [owner, repo] = fullName.split('/', 2) as [string, string];
    return { owner, repo };
  }
  return {
    owner: (pickArg(args, 'owner', 'owner_login', 'ownerLogin') as string | undefined) ?? '',
    repo: (pickArg(args, 'repo', 'repository', 'repo_name', 'repoName') as string | undefined) ?? '',
  };
}

/** The repository a tool call names, or an error message. */
function toolRepo(ctx: Ctx, args: Record<string, unknown>): { repo: Repo } | { error: string } {
  const { owner, repo } = ownerRepo(args);
  if (owner !== VIEWER_LOGIN) return { error: `Repository ${owner}/${repo} not found` };
  const found = ctx.store.getRepo(owner, repo);
  if (found === undefined) return { error: `Repository ${owner}/${repo} not found` };
  return { repo: found };
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'The name of the repository.' },
        description: { type: 'string', description: 'A short description of the repository.' },
        private: { type: 'boolean', description: 'Whether the repository is private.' },
      },
      required: ['name'],
    },
    call: (ctx, args) => {
      const name = pickArg(args, 'name', 'repo', 'repo_name') as string | undefined;
      if (typeof name !== 'string' || name.trim() === '') throw new Error('name is required');
      if (ctx.store.getRepo(VIEWER_LOGIN, name) !== undefined) throw new Error(`Repository ${name} already exists`);
      const repo = ctx.store.createRepo(
        VIEWER_LOGIN,
        name,
        (pickArg(args, 'description') as string | undefined) ?? '',
        pickArg(args, 'private') === true,
      );
      if (repo === null) throw new Error(`Repository ${name} already exists`);
      return ctx.store.repoJson(repo);
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
      },
      required: ['owner', 'repo'],
    },
    call: (ctx, args) => {
      const found = toolRepo(ctx, args);
      if ('error' in found) throw new Error(found.error);
      return ctx.store.repoJson(found.repo);
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
        title: { type: 'string', description: 'The title of the issue.' },
        body: { type: 'string', description: 'The body of the issue.' },
      },
      required: ['owner', 'repo', 'title'],
    },
    call: (ctx, args) => {
      const found = toolRepo(ctx, args);
      if ('error' in found) throw new Error(found.error);
      const title = pickArg(args, 'title') as string | undefined;
      if (typeof title !== 'string' || title.trim() === '') throw new Error('title is required');
      const issue = ctx.store.createIssue(found.repo, title, (pickArg(args, 'body') as string | undefined) ?? '', []);
      return ctx.store.issueJson(found.repo, issue);
    },
  },
  {
    name: 'list_issues',
    description: 'List the issues of a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
        state: { type: 'string', description: 'Filter by state: open, closed, or all.', enum: ['open', 'closed', 'all'] },
      },
      required: ['owner', 'repo'],
    },
    call: (ctx, args) => {
      const found = toolRepo(ctx, args);
      if ('error' in found) throw new Error(found.error);
      const state = (pickArg(args, 'state') as string | undefined) ?? 'open';
      let issues = [...found.repo.issues];
      if (state !== 'all') issues = issues.filter((issue) => issue.state === state);
      return issues.map((issue) => ctx.store.issueJson(found.repo, issue));
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
        title: { type: 'string', description: 'The title of the pull request.' },
        head: { type: 'string', description: 'The branch with the changes.' },
        base: { type: 'string', description: 'The branch to merge into.' },
        body: { type: 'string', description: 'The body of the pull request.' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
    call: (ctx, args) => {
      const found = toolRepo(ctx, args);
      if ('error' in found) throw new Error(found.error);
      const title = pickArg(args, 'title') as string | undefined;
      const head = pickArg(args, 'head', 'head_ref', 'headRef') as string | undefined;
      const base = pickArg(args, 'base', 'base_ref', 'baseRef') as string | undefined;
      if (typeof title !== 'string' || title.trim() === '') throw new Error('title is required');
      if (typeof head !== 'string' || head === '' || typeof base !== 'string' || base === '') throw new Error('head and base are required');
      const headSha = found.repo.refs.get(`refs/heads/${head}`);
      const baseSha = found.repo.refs.get(`refs/heads/${base}`);
      if (headSha === undefined || baseSha === undefined) throw new Error('head or base branch does not exist');
      const pr = ctx.store.createPull(found.repo, title, (pickArg(args, 'body') as string | undefined) ?? '', head, headSha, base, baseSha);
      return ctx.store.pullJson(found.repo, pr);
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request with a squash commit.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
        pull_number: { type: 'integer', description: 'The number of the pull request.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
    call: (ctx, args) => {
      const found = toolRepo(ctx, args);
      if ('error' in found) throw new Error(found.error);
      const number = pickArg(args, 'pull_number', 'pullNumber', 'pull_request_number', 'number') as number | undefined;
      if (typeof number !== 'number') throw new Error('pull_number is required');
      const pr = ctx.store.findPull(found.repo, number);
      if (pr === undefined) throw new Error(`Pull request #${number} not found`);
      if (pr.merged) throw new Error('Pull request already merged');
      if (pr.state !== 'open') throw new Error('Pull request is closed');
      const headSha = found.repo.refs.get(`refs/heads/${pr.headRef}`);
      const baseSha = found.repo.refs.get(`refs/heads/${pr.baseRef}`);
      if (headSha === undefined || baseSha === undefined) throw new Error('head or base branch does not exist');
      const merged = ctx.store.threeWayMerge(found.repo, baseSha, headSha);
      if (merged.conflict) throw new Error('Pull request has conflicts');
      const treeOid = ctx.store.buildTreeFromFlat(found.repo, merged.flat);
      const mergeSha = ctx.store.createCommit(found.repo, treeOid, [baseSha], pr.title);
      found.repo.refs.set(`refs/heads/${pr.baseRef}`, mergeSha);
      pr.state = 'closed';
      pr.merged = true;
      pr.mergedAt = ctx.store.isoSec(ctx.store.nowSec());
      pr.mergeCommitSha = mergeSha;
      pr.baseSha = mergeSha;
      return { merged: true, sha: mergeSha, message: 'Pull request successfully merged' };
    },
  },
  {
    name: 'list_repos',
    description: 'List the repositories owned by the authenticated user.',
    inputSchema: { type: 'object', properties: {} },
    call: (ctx) => ctx.store.listViewerRepos().map((repo) => ctx.store.repoJson(repo)),
  },
  {
    name: 'get_issue',
    description: 'Get an issue by number.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
        issue_number: { type: 'integer', description: 'The number of the issue.' },
      },
      required: ['owner', 'repo', 'issue_number'],
    },
    call: (ctx, args) => {
      const found = toolRepo(ctx, args);
      if ('error' in found) throw new Error(found.error);
      const number = pickArg(args, 'issue_number', 'issueNumber', 'number') as number | undefined;
      if (typeof number !== 'number') throw new Error('issue_number is required');
      const issue = ctx.store.findIssue(found.repo, number);
      if (issue === undefined) throw new Error(`Issue #${number} not found`);
      return ctx.store.issueJson(found.repo, issue);
    },
  },
  {
    name: 'create_comment',
    description: 'Create a comment on an issue.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
        issue_number: { type: 'integer', description: 'The number of the issue.' },
        body: { type: 'string', description: 'The body of the comment.' },
      },
      required: ['owner', 'repo', 'issue_number', 'body'],
    },
    call: (ctx, args) => {
      const found = toolRepo(ctx, args);
      if ('error' in found) throw new Error(found.error);
      const number = pickArg(args, 'issue_number', 'issueNumber', 'number') as number | undefined;
      if (typeof number !== 'number') throw new Error('issue_number is required');
      const issue = ctx.store.findIssue(found.repo, number);
      if (issue === undefined) throw new Error(`Issue #${number} not found`);
      const body = pickArg(args, 'body') as string | undefined;
      if (typeof body !== 'string') throw new Error('body is required');
      const comment = ctx.store.addComment(found.repo, issue, body);
      return ctx.store.commentJson(found.repo, issue, comment);
    },
  },
  {
    name: 'list_pull_requests',
    description: 'List the pull requests of a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
        state: { type: 'string', description: 'Filter by state: open, closed, or all.', enum: ['open', 'closed', 'all'] },
      },
      required: ['owner', 'repo'],
    },
    call: (ctx, args) => {
      const found = toolRepo(ctx, args);
      if ('error' in found) throw new Error(found.error);
      const state = (pickArg(args, 'state') as string | undefined) ?? 'open';
      let pulls = [...found.repo.pulls];
      if (state !== 'all') pulls = pulls.filter((pull) => pull.state === state);
      return pulls.map((pull) => ctx.store.pullJson(found.repo, pull));
    },
  },
];

/** A JSON-RPC response object. */
function rpcResponse(id: unknown, result: unknown): Record<string, unknown> {
  return { jsonrpc: '2.0', id, result };
}

/** A JSON-RPC error response object. */
function rpcError(id: unknown, code: number, message: string): Record<string, unknown> {
  return { jsonrpc: '2.0', id, error: { code, message } };
}

/** The protocol version the client asked for, or the newest we speak. */
function requestedVersion(request: Record<string, unknown>): string {
  const meta = request._meta as Record<string, unknown> | undefined;
  const fromMeta = meta?.['io.modelcontextprotocol/protocolVersion'];
  if (typeof fromMeta === 'string' && fromMeta !== '') return fromMeta;
  const params = request.params as Record<string, unknown> | undefined;
  const fromParams = params?.protocolVersion;
  if (typeof fromParams === 'string' && fromParams !== '') return fromParams;
  return SUPPORTED_VERSIONS[0] as string;
}

/** Handle one JSON-RPC request. Returns null for a notification (no reply). */
function handleRpc(ctx: Ctx, request: Record<string, unknown>): Record<string, unknown> | null {
  const id = request.id;
  const isNotification = id === undefined || id === null;
  const method = request.method;
  if (typeof method !== 'string') {
    if (isNotification) return null;
    return rpcError(id, -32600, 'Invalid Request: method is required');
  }
  // Notifications get no reply (a 202 with an empty body from the handler).
  if (isNotification) return null;

  if (method === 'server/discover') {
    return rpcResponse(id, {
      supportedVersions: SUPPORTED_VERSIONS,
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
      capabilities: { tools: {} },
    });
  }
  if (method === 'initialize') {
    return rpcResponse(id, {
      protocolVersion: requestedVersion(request),
      capabilities: { tools: {} },
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
      instructions: 'A GitHub-compatible git forge. Use the tools to manage repositories, issues and pull requests.',
    });
  }
  if (method === 'ping') {
    return rpcResponse(id, {});
  }
  if (method === 'tools/list') {
    return rpcResponse(id, {
      tools: TOOLS.map((tool) => ({ name: tool.name, description: tool.description, inputSchema: tool.inputSchema })),
    });
  }
  if (method === 'tools/call') {
    const params = (request.params as Record<string, unknown> | undefined) ?? {};
    const name = params.name as string | undefined;
    const tool = TOOLS.find((candidate) => candidate.name === name);
    if (tool === undefined) {
      return rpcError(id, -32601, `Unknown tool: ${typeof name === 'string' ? name : ''}`);
    }
    const args = (params.arguments as Record<string, unknown> | undefined) ?? {};
    try {
      const result = tool.call(ctx, args);
      return rpcResponse(id, {
        content: [{ type: 'text', text: jsonText(result) }],
        isError: false,
      });
    } catch (err) {
      return rpcResponse(id, {
        content: [{ type: 'text', text: err instanceof Error ? err.message : 'tool call failed' }],
        isError: true,
      });
    }
  }
  return rpcError(id, -32601, `Method not found: ${method}`);
}

/** Handle POST /mcp. */
export function handleMcp(ctx: Ctx, res: ServerResponse): boolean {
  if (ctx.path !== '/mcp') return false;
  if (ctx.method !== 'POST') {
    sendNotFound(res);
    return true;
  }
  if (!authorized(ctx)) {
    sendUnauthorized(res);
    return true;
  }
  // A body that is not JSON is HTTP 400 with a -32700 parse error.
  let request: unknown;
  try {
    request = JSON.parse(ctx.body.toString('utf8')) as unknown;
  } catch {
    sendJson(res, 400, rpcError(null, -32700, 'Parse error'));
    return true;
  }
  if (request === null || typeof request !== 'object' || Array.isArray(request)) {
    sendJson(res, 400, rpcError(null, -32600, 'Invalid Request'));
    return true;
  }
  const result = handleRpc(ctx, request as Record<string, unknown>);
  if (result === null) {
    // A notification: 202 with no body.
    res.writeHead(202, { ...baseHeaders() });
    res.end();
    return true;
  }
  sendJson(res, 200, result);
  return true;
}
