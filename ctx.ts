// The request context every handler receives: the store, the request line,
// the query, the headers and the raw body (already read by the server).

import type { IncomingHttpHeaders } from 'node:http';
import type { Store } from './store.ts';

export interface Ctx {
  store: Store;
  /** The bearer token the process was given (MAKEFUN_SHOP_TOKEN). */
  token: string;
  method: string;
  /** The decoded request path (no query string). */
  path: string;
  query: URLSearchParams;
  headers: IncomingHttpHeaders;
  /** The raw request body (empty when there is none). */
  body: Buffer;
}

/** Whether the request carries the process's bearer token. */
export function authorized(ctx: Ctx): boolean {
  const header = ctx.headers.authorization;
  if (typeof header !== 'string') return false;
  const bearerPrefix = 'Bearer ';
  if (header.startsWith(bearerPrefix)) {
    const token = header.slice(bearerPrefix.length).trim();
    return token !== '' && token === ctx.token;
  }
  // The real git client authenticates with Basic (the token as the password,
  // as GitHub accepts); the suite's client uses Bearer.
  const basicPrefix = 'Basic ';
  if (header.startsWith(basicPrefix)) {
    try {
      const decoded = Buffer.from(header.slice(basicPrefix.length).trim(), 'base64').toString('utf8');
      const colon = decoded.indexOf(':');
      const user = colon < 0 ? decoded : decoded.slice(0, colon);
      const password = colon < 0 ? '' : decoded.slice(colon + 1);
      return ctx.token !== '' && (user === ctx.token || password === ctx.token);
    } catch {
      return false;
    }
  }
  return false;
}

/** Parse a JSON body, or return null when it is absent or malformed. */
export function jsonBody(ctx: Ctx): unknown {
  if (ctx.body.length === 0) return null;
  try {
    return JSON.parse(ctx.body.toString('utf8')) as unknown;
  } catch {
    return undefined;
  }
}
