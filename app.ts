// The server: one process, one in-memory store, four surfaces. It reads the
// whole request body, builds the context, and routes: git smart HTTP at
// /{owner}/{repo}.git, MCP at /mcp, GraphQL at /api/graphql, REST v3 under
// /api/v3, and /healthz for the harness's liveness probe. Every reply is
// JSON (or a git binary body) with a non-empty X-GitHub-Request-Id; an
// unknown route is a JSON 404, never HTML.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { handleGraphql } from './graphql.ts';
import { handleInfoRefs, handleReceivePack, handleUploadPack } from './githttp.ts';
import { handleMcp } from './mcp.ts';
import { handleRest } from './rest.ts';
import { sendJson, sendNotFound } from './respond.ts';
import { Store } from './store.ts';
import type { Ctx } from './ctx.ts';

/** The largest request body accepted (the suite's packs are far smaller). */
const MAX_BODY_BYTES = 64 * 1024 * 1024;

/** The shared empty body (a bodyless request reuses it, never allocates). */
const EMPTY_BODY = Buffer.alloc(0);
/** The shared empty query (a request with no `?` reuses it). */
const EMPTY_QUERY = new URLSearchParams();

/** Whether the request carries a body: a chunked transfer, or a positive
 * Content-Length. A GET with no Content-Length, and a Content-Length of 0,
 * has none. */
function hasBody(req: IncomingMessage): boolean {
  if (req.headers['transfer-encoding'] !== undefined) return true;
  const length = req.headers['content-length'];
  if (length === undefined) return false;
  const n = Number(length);
  return Number.isFinite(n) && n > 0;
}

/** The bearer token the harness gives this process. */
function shopToken(): string {
  return process.env.MAKEFUN_SHOP_TOKEN ?? '';
}

/** Decode a request path, falling back to the raw path when it is malformed.
 * A path with no `%` is already decoded, so it skips decodeURIComponent. */
function decodePath(raw: string): string {
  if (!raw.includes('%')) return raw;
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw;
  }
}

/** Read the whole request body (bounded), or reject with an error. */
function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let size = 0;
    req.on('data', (chunk: Buffer) => {
      size += chunk.length;
      if (size > MAX_BODY_BYTES) {
        reject(new Error('request body too large'));
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

/** Route one request. Returns true when a handler answered it. */
function route(ctx: Ctx, res: ServerResponse): boolean {
  const path = ctx.path;

  // The harness's liveness probe (no auth).
  if (path === '/healthz' && ctx.method === 'GET') {
    sendJson(res, 200, { ok: true });
    return true;
  }

  // REST v3: the hot path (the read and write workloads are all REST).
  // A cheap prefix check first, so no regex ever runs on a REST path.
  if (path === '/api/v3' || path.startsWith('/api/v3/')) return handleRest(ctx, res);

  // GraphQL.
  if (path === '/api/graphql') return handleGraphql(ctx, res);

  // MCP.
  if (path === '/mcp') return handleMcp(ctx, res);

  // Git smart HTTP: only a path carrying '.git/' can match the pattern, so
  // the regex runs solely on those, never on a REST/GraphQL/MCP path.
  if (path.includes('.git/')) {
    const gitMatch = /^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/.exec(path);
    if (gitMatch !== null) {
      const owner = gitMatch[1] as string;
      const repo = gitMatch[2] as string;
      const service = gitMatch[3] as string;
      if (service === 'info/refs' && ctx.method === 'GET') {
        handleInfoRefs(ctx, res, owner, repo);
        return true;
      }
      if (service === 'git-upload-pack' && ctx.method === 'POST') {
        handleUploadPack(ctx, res, owner, repo);
        return true;
      }
      if (service === 'git-receive-pack' && ctx.method === 'POST') {
        handleReceivePack(ctx, res, owner, repo);
        return true;
      }
      sendNotFound(res);
      return true;
    }
  }

  return false;
}

/** Build the server the lifecycle in main.ts binds and registers. */
export function createApp(): Server {
  const store = new Store();
  const token = shopToken();
  const server = createServer((req, res) => {
    void (async () => {
      try {
        // A request with no body (every GET, and a bodyless DELETE) need not
        // wait for 'end': the body is empty either way. But the stream must
        // still be drained (resume) so the keep-alive socket is reusable
        // for the next request on it — a paused, never-ended stream stalls
        // the connection the read workload's 2,000 sequential GETs share.
        const body = hasBody(req) ? await readBody(req) : (req.resume(), EMPTY_BODY);
        // Split the path and query by hand: `new URL` parses the whole URL
        // (scheme, host, port) on every request, and only the path and the
        // query string are ever read.
        const rawUrl = req.url ?? '/';
        const qmark = rawUrl.indexOf('?');
        const rawPath = qmark < 0 ? rawUrl : rawUrl.slice(0, qmark);
        const ctx: Ctx = {
          store,
          token,
          method: req.method ?? 'GET',
          path: decodePath(rawPath),
          query: qmark < 0 ? EMPTY_QUERY : new URLSearchParams(rawUrl.slice(qmark + 1)),
          headers: req.headers,
          body,
        };
        if (!route(ctx, res)) {
          sendNotFound(res);
        }
      } catch (err) {
        // A request that cannot be read, or a handler that threw: answer a
        // JSON 500 rather than hanging the connection.
        if (!res.headersSent) {
          sendJson(res, 500, { message: err instanceof Error ? err.message : 'internal error' });
        } else {
          res.end();
        }
      }
    })();
  });
  return server;
}
