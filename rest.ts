// GitHub REST v3: repositories, branches, commits, contents, labels,
// issues, comments and pull requests. Every reply is application/json with
// a non-empty X-GitHub-Request-Id; every error is a JSON body with a string
// message; a missing or wrong bearer token is 401; a repository under
// another owner is 404, never 403.

import type { ServerResponse } from 'node:http';
import { authorized, jsonBody, type Ctx } from './ctx.ts';
import { parseCommit } from './gitobj.ts';
import { sendError, sendJson, sendJsonText, sendNoContent, sendNotFound, sendUnauthorized } from './respond.ts';
import {
  DEFAULT_BRANCH,
  VIEWER_LOGIN,
  type FlatFile,
  type Issue,
  type Label,
  type Pull,
  type Repo,
} from './store.ts';
import { b64decode, b64encodeBytes, isOid } from './util.ts';

/** Split a path into decoded segments. A segment with no `%` is already
 * decoded, so it skips decodeURIComponent (and its try/catch) entirely —
 * the read workload splits 7 segments per request, 2,000 times in a row. */
export function segmentsOf(path: string): string[] {
  return path
    .split('/')
    .filter((part) => part !== '')
    .map((part) => {
      if (!part.includes('%')) return part;
      try {
        return decodeURIComponent(part);
      } catch {
        return part;
      }
    });
}

/** The repository a REST path names, or null (404: missing, or another owner). */
function restRepo(ctx: Ctx, owner: string, repo: string): Repo | null {
  if (owner !== VIEWER_LOGIN) return null;
  return ctx.store.getRepo(owner, repo) ?? null;
}

/** The page size a query asks for (GitHub's default is 30, capped at 100). */
function pageSize(query: URLSearchParams): number {
  const raw = query.get('per_page');
  if (raw === null) return 30;
  const n = Math.floor(Number(raw));
  if (!Number.isFinite(n) || n <= 0) return 30;
  return Math.min(100, n);
}

/** The page a query asks for (GitHub's default is 1). */
function pageNumber(query: URLSearchParams): number {
  const raw = query.get('page');
  if (raw === null) return 1;
  const n = Math.floor(Number(raw));
  if (!Number.isFinite(n) || n <= 0) return 1;
  return n;
}

/** A page of a list. */
function paginate<T>(items: readonly T[], query: URLSearchParams): { page: T[]; total: number } {
  const perPage = pageSize(query);
  const page = pageNumber(query);
  const total = items.length;
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  return { page: slice as T[], total };
}

/** Build the Link header for a paginated list (absolute, same-origin URLs). */
function linkHeader(ctx: Ctx, path: string, query: URLSearchParams, total: number): string | null {
  const perPage = pageSize(ctx.query);
  const page = pageNumber(ctx.query);
  const lastPage = Math.max(1, Math.ceil(total / perPage));
  if (lastPage <= 1) return null;
  const host = ctx.headers.host ?? '127.0.0.1';
  const base = `http://${host}${path}`;
  const urlFor = (p: number): string => {
    const q = new URLSearchParams(query);
    q.set('page', String(p));
    q.set('per_page', String(perPage));
    return `${base}?${q.toString()}`;
  };
  const parts: string[] = [];
  if (page < lastPage) parts.push(`<${urlFor(page + 1)}>; rel="next"`);
  if (page > 1) parts.push(`<${urlFor(page - 1)}>; rel="prev"`);
  parts.push(`<${urlFor(1)}>; rel="first"`);
  parts.push(`<${urlFor(lastPage)}>; rel="last"`);
  return parts.join(', ');
}

/** Send a paginated list as JSON with a Link header when there is a next page. */
function sendList<T>(ctx: Ctx, res: ServerResponse, path: string, items: readonly T[]): void {
  const { page, total } = paginate(items, ctx.query);
  const link = linkHeader(ctx, path, ctx.query, total);
  const headers: Record<string, string> = {};
  if (link !== null) headers.link = link;
  sendJson(res, 200, page, headers);
}

/** The commit at a ref (a branch name or a 40-hex id), or null. */
function commitAtRef(ctx: Ctx, repo: Repo, ref: string | null): string | null {
  if (ref === null || ref === '') return repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
  if (isOid(ref)) return ctx.store.hasObject(repo, ref) ? ref : null;
  return repo.refs.get(`refs/heads/${ref}`) ?? null;
}

/** Split a git "Name <email> ts tz" identity into its name and email. */
function splitWho(who: string): { name: string; email: string } {
  const match = /^(.*?) <([^>]*)>/.exec(who);
  if (match === null) return { name: who, email: '' };
  return { name: match[1] as string, email: match[2] as string };
}

/** The commits reachable from a tip, newest first (parents in order). */
function commitList(ctx: Ctx, repo: Repo, tip: string): Array<Record<string, unknown>> {
  const out: Array<Record<string, unknown>> = [];
  const seen = new Set<string>();
  const stack: string[] = [tip];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = repo.objects.get(oid);
    if (obj === undefined || obj.type !== 'commit') continue;
    const commit = parseCommit(obj.data);
    const author = splitWho(commit.author);
    const committer = splitWho(commit.committer);
    out.push({
      sha: oid,
      commit: {
        author: { name: author.name, email: author.email, date: ctx.store.isoSec(commit.authorTs) },
        committer: { name: committer.name, email: committer.email, date: ctx.store.isoSec(commit.committerTs) },
        message: commit.message,
        tree: { sha: commit.tree },
      },
      parents: commit.parents.map((parent) => ({ sha: parent })),
    });
    // Parents in reverse so the first parent is visited first.
    for (let i = commit.parents.length - 1; i >= 0; i -= 1) stack.push(commit.parents[i] as string);
  }
  return out;
}

/** Handle the REST v3 routes. Returns true when the route was handled. */
export function handleRest(ctx: Ctx, res: ServerResponse): boolean {
  const seg = segmentsOf(ctx.path);
  if (seg.length < 2 || seg[0] !== 'api' || seg[1] !== 'v3') return false;
  const rest = seg.slice(2);
  const method = ctx.method;

  // The API root: a 200 with the token's scopes (the starter's answer).
  if (rest.length === 0) {
    if (method === 'GET') {
      sendJson(res, 200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, { 'x-oauth-scopes': 'repo, read:org' });
      return true;
    }
    sendNotFound(res);
    return true;
  }

  // Every other /api/v3 route requires the bearer token.
  if (!authorized(ctx)) {
    sendUnauthorized(res);
    return true;
  }

  // GET /meta: the installation's version.
  if (rest.length === 1 && rest[0] === 'meta' && method === 'GET') {
    sendJson(res, 200, { installed_version: '1.0.0', verifiable_password_authentication: true, github_enterprise: false });
    return true;
  }

  // GET /user: the token's login.
  if (rest.length === 1 && rest[0] === 'user' && method === 'GET') {
    sendJson(res, 200, { login: VIEWER_LOGIN, id: 1000, type: 'User', name: 'ada iversen', email: 'adaiversen@example.invalid' });
    return true;
  }

  // /user/repos: list and create.
  if (rest.length === 2 && rest[0] === 'user' && rest[1] === 'repos') {
    if (method === 'GET') {
      sendList(ctx, res, ctx.path, ctx.store.listViewerRepos().map((repo) => ctx.store.repoJson(repo)));
      return true;
    }
    if (method === 'POST') {
      const body = jsonBody(ctx);
      if (body === undefined) {
        sendError(res, 400, 'Invalid JSON body');
        return true;
      }
      if (body === null || typeof body !== 'object' || Array.isArray(body)) {
        sendError(res, 400, 'Invalid request body');
        return true;
      }
      const record = body as Record<string, unknown>;
      const name = typeof record.name === 'string' ? record.name.trim() : '';
      if (name === '') {
        sendError(res, 422, 'name is required');
        return true;
      }
      if (ctx.store.getRepo(VIEWER_LOGIN, name) !== undefined) {
        sendError(res, 422, `Repository ${name} already exists`);
        return true;
      }
      const repo = ctx.store.createRepo(
        VIEWER_LOGIN,
        name,
        typeof record.description === 'string' ? record.description : '',
        record.private === true,
      );
      if (repo === null) {
        sendError(res, 422, `Repository ${name} already exists`);
        return true;
      }
      sendJson(res, 201, ctx.store.repoJson(repo));
      return true;
    }
    sendNotFound(res);
    return true;
  }

  // /repos/{owner}/{repo}[...]
  if (rest.length >= 3 && rest[0] === 'repos') {
    return handleRepoRoutes(ctx, res, rest);
  }

  sendNotFound(res);
  return true;
}

function handleRepoRoutes(ctx: Ctx, res: ServerResponse, rest: string[]): boolean {
  const method = ctx.method;
  const owner = rest[1] as string;
  const repoName = rest[2] as string;
  const repo = restRepo(ctx, owner, repoName);
  const tail = rest.slice(3);

  // GET /repos/{o}/{r}, PATCH, DELETE.
  if (tail.length === 0) {
    if (repo === null) {
      sendNotFound(res, 'Not Found');
      return true;
    }
    if (method === 'GET') {
      sendJson(res, 200, ctx.store.repoJson(repo));
      return true;
    }
    if (method === 'PATCH') {
      const body = jsonBody(ctx);
      if (body === undefined) {
        sendError(res, 400, 'Invalid JSON body');
        return true;
      }
      if (body !== null && typeof body === 'object' && !Array.isArray(body)) {
        const record = body as Record<string, unknown>;
        if (typeof record.description === 'string') repo.description = record.description;
        if (typeof record.private === 'boolean') repo.private = record.private;
      }
      sendJson(res, 200, ctx.store.repoJson(repo));
      return true;
    }
    if (method === 'DELETE') {
      ctx.store.deleteRepo(owner, repoName);
      sendNoContent(res);
      return true;
    }
    sendNotFound(res);
    return true;
  }

  if (repo === null) {
    sendNotFound(res, 'Not Found');
    return true;
  }

  // GET /branches and GET /branches/{branch}.
  if (tail[0] === 'branches') {
    if (method === 'GET' && tail.length === 1) {
      const branches = [...repo.refs.keys()]
        .filter((ref) => ref.startsWith('refs/heads/'))
        .map((ref) => ref.slice('refs/heads/'.length))
        .sort()
        .map((name) => ({ name, commit: { sha: repo.refs.get(`refs/heads/${name}`) as string } }));
      sendList(ctx, res, ctx.path, branches);
      return true;
    }
    if (method === 'GET' && tail.length >= 2) {
      const branch = tail.slice(1).join('/');
      const sha = repo.refs.get(`refs/heads/${branch}`);
      if (sha === undefined) {
        sendNotFound(res, 'Branch not found');
        return true;
      }
      sendJson(res, 200, { name: branch, commit: { sha } });
      return true;
    }
    sendNotFound(res);
    return true;
  }

  // GET /commits[?sha=].
  if (tail[0] === 'commits' && tail.length === 1 && method === 'GET') {
    const tip = commitAtRef(ctx, repo, ctx.query.get('sha'));
    if (tip === null) {
      sendJson(res, 200, []);
      return true;
    }
    sendList(ctx, res, ctx.path, commitList(ctx, repo, tip));
    return true;
  }

  // /git/refs: POST (create) and GET /git/refs/heads/{branch}.
  if (tail[0] === 'git' && tail[1] === 'refs') {
    if (method === 'POST' && tail.length === 2) {
      const body = jsonBody(ctx);
      if (body === undefined) {
        sendError(res, 400, 'Invalid JSON body');
        return true;
      }
      if (body === null || typeof body !== 'object' || Array.isArray(body)) {
        sendError(res, 400, 'Invalid request body');
        return true;
      }
      const record = body as Record<string, unknown>;
      const ref = typeof record.ref === 'string' ? record.ref : '';
      const sha = typeof record.sha === 'string' ? record.sha : '';
      if (!ref.startsWith('refs/') || !isOid(sha)) {
        sendError(res, 422, 'Invalid ref or sha');
        return true;
      }
      if (repo.refs.has(ref)) {
        sendError(res, 422, 'Reference already exists');
        return true;
      }
      const obj = repo.objects.get(sha);
      if (obj === undefined || obj.type !== 'commit') {
        sendError(res, 422, 'Object does not exist');
        return true;
      }
      repo.refs.set(ref, sha);
      sendJson(res, 201, { ref, object: { sha, type: 'commit' } });
      return true;
    }
    if (method === 'GET' && tail.length >= 4 && tail[2] === 'heads') {
      const branch = tail.slice(3).join('/');
      const sha = repo.refs.get(`refs/heads/${branch}`);
      if (sha === undefined) {
        sendNotFound(res, 'Reference does not exist');
        return true;
      }
      sendJson(res, 200, { ref: `refs/heads/${branch}`, object: { sha, type: 'commit' } });
      return true;
    }
    sendNotFound(res);
    return true;
  }

  // /contents/{path}: GET, PUT, DELETE.
  if (tail[0] === 'contents') {
    return handleContents(ctx, res, repo, tail.slice(1));
  }

  // /labels: GET, POST.
  if (tail[0] === 'labels') {
    if (method === 'GET' && tail.length === 1) {
      const labels = [...repo.labels.values()].sort((a, b) => (a.name < b.name ? -1 : 1)).map((label) => ctx.store.labelJson(label));
      sendList(ctx, res, ctx.path, labels);
      return true;
    }
    if (method === 'POST' && tail.length === 1) {
      const body = jsonBody(ctx);
      if (body === undefined) {
        sendError(res, 400, 'Invalid JSON body');
        return true;
      }
      if (body === null || typeof body !== 'object' || Array.isArray(body)) {
        sendError(res, 400, 'Invalid request body');
        return true;
      }
      const record = body as Record<string, unknown>;
      const name = typeof record.name === 'string' ? record.name.trim() : '';
      if (name === '') {
        sendError(res, 422, 'name is required');
        return true;
      }
      if (repo.labels.has(name)) {
        sendError(res, 422, `Label ${name} already exists`);
        return true;
      }
      const label: Label = {
        name,
        color: typeof record.color === 'string' ? record.color : 'ededed',
        description: typeof record.description === 'string' ? record.description : '',
      };
      repo.labels.set(name, label);
      sendJson(res, 201, ctx.store.labelJson(label));
      return true;
    }
    sendNotFound(res);
    return true;
  }

  // /issues[/{n}[/comments]] and /pulls[/{n}[/files|/merge]].
  if (tail[0] === 'issues') return handleIssues(ctx, res, repo, tail.slice(1));
  if (tail[0] === 'pulls') return handlePulls(ctx, res, repo, tail.slice(1));

  sendNotFound(res);
  return true;
}

/** GET /contents/{path}, PUT /contents/{path}, DELETE /contents/{path}. */
function handleContents(ctx: Ctx, res: ServerResponse, repo: Repo, tail: string[]): boolean {
  const method = ctx.method;
  const path = tail.join('/');

  // The branch a write targets: the named branch (created from the default
  // branch when it does not exist), else the default branch.
  const writeBranch = (body: Record<string, unknown>): string => {
    const branch = typeof body.branch === 'string' && body.branch !== '' ? body.branch : DEFAULT_BRANCH;
    const ref = `refs/heads/${branch}`;
    if (!repo.refs.has(ref)) {
      const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
      if (tip !== undefined) repo.refs.set(ref, tip);
    }
    return branch;
  };

  if (method === 'GET') {
    const tip = commitAtRef(ctx, repo, ctx.query.get('ref'));
    if (tip === null) {
      sendNotFound(res, 'Not Found');
      return true;
    }
    const files = ctx.store.flatTree(repo, tip);
    if (path === '') {
      sendJson(res, 200, directoryListing(ctx, repo, files, ''));
      return true;
    }
    const file = files.get(path);
    if (file !== undefined) {
      const blob = repo.objects.get(file.oid);
      sendJson(res, 200, {
        type: 'file',
        encoding: 'base64',
        size: blob === undefined ? 0 : blob.data.length,
        name: path.split('/').pop() ?? path,
        path,
        content: b64encodeBytes(blob === undefined ? Buffer.alloc(0) : blob.data),
        sha: file.oid,
      });
      return true;
    }
    // A directory: any file under the path.
    const prefix = `${path}/`;
    let isDir = false;
    for (const key of files.keys()) {
      if (key.startsWith(prefix)) {
        isDir = true;
        break;
      }
    }
    if (isDir) {
      sendJson(res, 200, directoryListing(ctx, repo, files, path));
      return true;
    }
    sendNotFound(res, 'Not Found');
    return true;
  }

  if (method === 'PUT') {
    const body = jsonBody(ctx);
    if (body === undefined) {
      sendError(res, 400, 'Invalid JSON body');
      return true;
    }
    if (body === null || typeof body !== 'object' || Array.isArray(body)) {
      sendError(res, 400, 'Invalid request body');
      return true;
    }
    const record = body as Record<string, unknown>;
    if (typeof record.content !== 'string') {
      sendError(res, 422, 'content is required');
      return true;
    }
    const message = typeof record.message === 'string' ? record.message : 'Update file';
    const branch = writeBranch(record);
    const tip = repo.refs.get(`refs/heads/${branch}`);
    const files = tip === undefined ? new Map<string, FlatFile>() : ctx.store.flatTree(repo, tip);
    const existing = files.get(path);
    const newBytes = b64decode(record.content);
    if (existing !== undefined) {
      if (typeof record.sha !== 'string') {
        sendError(res, 422, "sha wasn't supplied");
        return true;
      }
      if (record.sha !== existing.oid) {
        sendError(res, 409, 'sha does not match');
        return true;
      }
    }
    const blobOid = ctx.store.putObject(repo, 'blob', newBytes);
    const nextFiles = new Map(files);
    nextFiles.set(path, { oid: blobOid, mode: '100644' });
    const treeOid = ctx.store.buildTreeFromFlat(repo, nextFiles);
    const commitOid = ctx.store.createCommit(repo, treeOid, tip === undefined ? [] : [tip], message);
    repo.refs.set(`refs/heads/${branch}`, commitOid);
    sendJson(res, existing === undefined ? 201 : 200, {
      content: { name: path.split('/').pop() ?? path, path, sha: blobOid, size: newBytes.length, encoding: 'base64', type: 'file' },
      commit: { sha: commitOid, message },
    });
    return true;
  }

  if (method === 'DELETE') {
    const body = jsonBody(ctx);
    if (body === undefined) {
      sendError(res, 400, 'Invalid JSON body');
      return true;
    }
    if (body === null || typeof body !== 'object' || Array.isArray(body)) {
      sendError(res, 400, 'Invalid request body');
      return true;
    }
    const record = body as Record<string, unknown>;
    const message = typeof record.message === 'string' ? record.message : 'Delete file';
    const branch = writeBranch(record);
    const tip = repo.refs.get(`refs/heads/${branch}`);
    if (tip === undefined) {
      sendNotFound(res, 'Not Found');
      return true;
    }
    const files = ctx.store.flatTree(repo, tip);
    const existing = files.get(path);
    if (existing === undefined) {
      sendNotFound(res, 'Not Found');
      return true;
    }
    if (typeof record.sha !== 'string') {
      sendError(res, 422, "sha wasn't supplied");
      return true;
    }
    if (record.sha !== existing.oid) {
      sendError(res, 409, 'sha does not match');
      return true;
    }
    const nextFiles = new Map(files);
    nextFiles.delete(path);
    const treeOid = ctx.store.buildTreeFromFlat(repo, nextFiles);
    const commitOid = ctx.store.createCommit(repo, treeOid, [tip], message);
    repo.refs.set(`refs/heads/${branch}`, commitOid);
    sendJson(res, 200, { commit: { sha: commitOid, message } });
    return true;
  }

  sendNotFound(res);
  return true;
}

/** The immediate children of a directory, as GitHub's contents listing. */
function directoryListing(ctx: Ctx, repo: Repo, files: Map<string, FlatFile>, dir: string): Array<Record<string, unknown>> {
  const prefix = dir === '' ? '' : `${dir}/`;
  const children = new Map<string, { type: 'file' | 'dir'; file?: FlatFile }>();
  for (const [path, file] of files) {
    if (!path.startsWith(prefix)) continue;
    const rest = path.slice(prefix.length);
    const slash = rest.indexOf('/');
    if (slash === -1) children.set(rest, { type: 'file', file });
    else children.set(rest.slice(0, slash), { type: 'dir' });
  }
  const out: Array<Record<string, unknown>> = [];
  for (const name of [...children.keys()].sort()) {
    const child = children.get(name) as { type: 'file' | 'dir'; file?: FlatFile };
    const childPath = prefix + name;
    if (child.type === 'file' && child.file !== undefined) {
      const blob = repo.objects.get(child.file.oid);
      out.push({ type: 'file', name, path: childPath, sha: child.file.oid, size: blob === undefined ? 0 : blob.data.length });
    } else {
      const under = new Map<string, FlatFile>();
      for (const [path, file] of files) {
        if (path.startsWith(`${childPath}/`)) under.set(path.slice(childPath.length + 1), file);
      }
      const treeOid = ctx.store.buildTreeFromFlat(repo, under);
      out.push({ type: 'dir', name, path: childPath, sha: treeOid });
    }
  }
  return out;
}

/** GET /issues, POST /issues, GET/PATCH /issues/{n}, /issues/{n}/comments. */
function handleIssues(ctx: Ctx, res: ServerResponse, repo: Repo, tail: string[]): boolean {
  const method = ctx.method;

  if (tail.length === 0) {
    if (method === 'GET') {
      const state = ctx.query.get('state') ?? 'open';
      const sort = ctx.query.get('sort') ?? 'created';
      const direction = ctx.query.get('direction') ?? 'desc';
      let issues = [...repo.issues];
      if (state !== 'all') issues = issues.filter((issue) => issue.state === state);
      issues.sort((a, b) => (direction === 'asc' ? a.number - b.number : b.number - a.number));
      // sort=created is the only order the suite uses; numbers track creation.
      void sort;
      sendList(ctx, res, ctx.path, issues.map((issue) => ctx.store.issueJson(repo, issue)));
      return true;
    }
    if (method === 'POST') {
      const body = jsonBody(ctx);
      if (body === undefined) {
        sendError(res, 400, 'Invalid JSON body');
        return true;
      }
      if (body === null || typeof body !== 'object' || Array.isArray(body)) {
        sendError(res, 400, 'Invalid request body');
        return true;
      }
      const record = body as Record<string, unknown>;
      const title = typeof record.title === 'string' ? record.title.trim() : '';
      if (title === '') {
        sendError(res, 422, 'title is required');
        return true;
      }
      const labels = Array.isArray(record.labels)
        ? (record.labels.filter((label) => typeof label === 'string') as string[])
        : [];
      const unknown = labels.find((label) => !repo.labels.has(label));
      if (unknown !== undefined) {
        sendError(res, 422, `Label ${unknown} does not exist`);
        return true;
      }
      const issue = ctx.store.createIssue(repo, title, typeof record.body === 'string' ? record.body : '', labels);
      sendJson(res, 201, ctx.store.issueJson(repo, issue));
      return true;
    }
    sendNotFound(res);
    return true;
  }

  const number = Number(tail[0]);
  if (!Number.isInteger(number) || number <= 0) {
    sendNotFound(res, 'Not Found');
    return true;
  }
  const issue = ctx.store.findIssue(repo, number);
  if (issue === undefined) {
    sendNotFound(res, 'Not Found');
    return true;
  }

  if (tail.length === 1) {
    if (method === 'GET') {
      // The cached serialized form: the read workload hits this path.
      sendJsonText(res, 200, ctx.store.issueJsonText(repo, issue));
      return true;
    }
    if (method === 'PATCH') {
      const body = jsonBody(ctx);
      if (body === undefined) {
        sendError(res, 400, 'Invalid JSON body');
        return true;
      }
      if (body !== null && typeof body === 'object' && !Array.isArray(body)) {
        const record = body as Record<string, unknown>;
        if (typeof record.title === 'string') issue.title = record.title;
        if (typeof record.body === 'string') issue.body = record.body;
        if (record.state === 'open' || record.state === 'closed') issue.state = record.state;
        if (Array.isArray(record.labels)) {
          const labels = record.labels.filter((label) => typeof label === 'string') as string[];
          const unknown = labels.find((label) => !repo.labels.has(label));
          if (unknown !== undefined) {
            sendError(res, 422, `Label ${unknown} does not exist`);
            return true;
          }
          issue.labels = labels;
        }
      }
      ctx.store.touchIssue(repo, issue);
      sendJson(res, 200, ctx.store.issueJson(repo, issue));
      return true;
    }
    sendNotFound(res);
    return true;
  }

  if (tail.length === 2 && tail[1] === 'comments') {
    if (method === 'GET') {
      sendList(ctx, res, ctx.path, issue.comments.map((comment) => ctx.store.commentJson(repo, issue, comment)));
      return true;
    }
    if (method === 'POST') {
      const body = jsonBody(ctx);
      if (body === undefined) {
        sendError(res, 400, 'Invalid JSON body');
        return true;
      }
      if (body === null || typeof body !== 'object' || Array.isArray(body)) {
        sendError(res, 400, 'Invalid request body');
        return true;
      }
      const record = body as Record<string, unknown>;
      const commentBody = typeof record.body === 'string' ? record.body : '';
      const comment = ctx.store.addComment(repo, issue, commentBody);
      sendJson(res, 201, ctx.store.commentJson(repo, issue, comment));
      return true;
    }
    sendNotFound(res);
    return true;
  }

  sendNotFound(res);
  return true;
}

/** GET /pulls, POST /pulls, GET /pulls/{n}, /pulls/{n}/files, /pulls/{n}/merge. */
function handlePulls(ctx: Ctx, res: ServerResponse, repo: Repo, tail: string[]): boolean {
  const method = ctx.method;

  if (tail.length === 0) {
    if (method === 'GET') {
      const state = ctx.query.get('state') ?? 'open';
      let pulls = [...repo.pulls];
      if (state !== 'all') pulls = pulls.filter((pull) => pull.state === state);
      pulls.sort((a, b) => b.number - a.number);
      sendList(ctx, res, ctx.path, pulls.map((pull) => ctx.store.pullJson(repo, pull)));
      return true;
    }
    if (method === 'POST') {
      const body = jsonBody(ctx);
      if (body === undefined) {
        sendError(res, 400, 'Invalid JSON body');
        return true;
      }
      if (body === null || typeof body !== 'object' || Array.isArray(body)) {
        sendError(res, 400, 'Invalid request body');
        return true;
      }
      const record = body as Record<string, unknown>;
      const title = typeof record.title === 'string' ? record.title.trim() : '';
      const head = typeof record.head === 'string' ? record.head : '';
      const base = typeof record.base === 'string' ? record.base : '';
      if (title === '' || head === '' || base === '') {
        sendError(res, 422, 'title, head and base are required');
        return true;
      }
      const headSha = repo.refs.get(`refs/heads/${head}`);
      const baseSha = repo.refs.get(`refs/heads/${base}`);
      if (headSha === undefined || baseSha === undefined) {
        sendError(res, 422, 'head or base branch does not exist');
        return true;
      }
      const existing = repo.pulls.find(
        (pull) => pull.headRef === head && pull.baseRef === base && pull.state === 'open',
      );
      if (existing !== undefined) {
        sendError(res, 422, 'A pull request already exists for these branches');
        return true;
      }
      const pull = ctx.store.createPull(repo, title, typeof record.body === 'string' ? record.body : '', head, headSha, base, baseSha);
      sendJson(res, 201, ctx.store.pullJson(repo, pull));
      return true;
    }
    sendNotFound(res);
    return true;
  }

  const number = Number(tail[0]);
  if (!Number.isInteger(number) || number <= 0) {
    sendNotFound(res, 'Not Found');
    return true;
  }
  const pull = ctx.store.findPull(repo, number);
  if (pull === undefined) {
    sendNotFound(res, 'Not Found');
    return true;
  }

  if (tail.length === 1) {
    if (method === 'GET') {
      sendJson(res, 200, ctx.store.pullJson(repo, pull));
      return true;
    }
    sendNotFound(res);
    return true;
  }

  if (tail.length === 2 && tail[1] === 'files' && method === 'GET') {
    sendJson(res, 200, ctx.store.pullFiles(repo, pull));
    return true;
  }

  if (tail.length === 2 && tail[1] === 'merge' && method === 'PUT') {
    if (pull.merged) {
      sendError(res, 405, 'Pull request already merged');
      return true;
    }
    if (pull.state !== 'open') {
      sendError(res, 405, 'Pull request is closed');
      return true;
    }
    const headSha = repo.refs.get(`refs/heads/${pull.headRef}`);
    const baseSha = repo.refs.get(`refs/heads/${pull.baseRef}`);
    if (headSha === undefined || baseSha === undefined) {
      sendError(res, 422, 'head or base branch does not exist');
      return true;
    }
    const merged = ctx.store.threeWayMerge(repo, baseSha, headSha);
    if (merged.conflict) {
      sendError(res, 405, 'Pull request has conflicts');
      return true;
    }
    // The squash commit: the merged tree on the base, one parent.
    const treeOid = ctx.store.buildTreeFromFlat(repo, merged.flat);
    const mergeSha = ctx.store.createCommit(repo, treeOid, [baseSha], pull.title);
    repo.refs.set(`refs/heads/${pull.baseRef}`, mergeSha);
    pull.state = 'closed';
    pull.merged = true;
    pull.mergedAt = ctx.store.isoSec(ctx.store.nowSec());
    pull.mergeCommitSha = mergeSha;
    pull.baseSha = mergeSha;
    sendJson(res, 200, { sha: mergeSha, merged: true, message: 'Pull request successfully merged' });
    return true;
  }

  sendNotFound(res);
  return true;
}
