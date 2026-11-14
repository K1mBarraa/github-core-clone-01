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

/** The bearer token the harness gives this process. */
function shopToken(): string {
  return process.env.MAKEFUN_SHOP_TOKEN ?? '';
}

/** Decode a request path, falling back to the raw path when it is malformed. */
function decodePath(raw: string): string {
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

  // Git smart HTTP: /{owner}/{repo}.git/info/refs and the two services.
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

  // MCP.
  if (path === '/mcp') return handleMcp(ctx, res);

  // GraphQL.
  if (path === '/api/graphql') return handleGraphql(ctx, res);

  // REST v3.
  if (path === '/api/v3' || path.startsWith('/api/v3/')) return handleRest(ctx, res);

  return false;
}

/** Build the server the lifecycle in main.ts binds and registers. */
export function createApp(): Server {
  const store = new Store();
  const token = shopToken();
  const server = createServer((req, res) => {
    void (async () => {
      try {
        const body = await readBody(req);
        const url = new URL(req.url ?? '/', 'http://127.0.0.1');
        const ctx: Ctx = {
          store,
          token,
          method: req.method ?? 'GET',
          path: decodePath(url.pathname),
          query: url.searchParams,
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
