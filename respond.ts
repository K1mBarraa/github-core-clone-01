// Response helpers: every reply carries a non-empty X-GitHub-Request-Id,
// JSON replies are `application/json; charset=utf-8`, and error bodies are
// JSON objects with a string `message` (the protocol group's discipline).

import type { ServerResponse } from 'node:http';
import { jsonText, nextRequestId } from './util.ts';

export const JSON_TYPE = 'application/json; charset=utf-8';

/** Headers every response carries. */
export function baseHeaders(requestId?: string): Record<string, string> {
  return { 'x-github-request-id': requestId ?? nextRequestId() };
}

/** Send a JSON body with a status and extra headers. */
export function sendJson(res: ServerResponse, status: number, body: unknown, headers: Readonly<Record<string, string>> = {}): void {
  const text = `${jsonText(body)}\n`;
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': Buffer.byteLength(text),
    ...baseHeaders(),
    ...headers,
  });
  res.end(text);
}

/** Send an already-serialized JSON body (the hot read path's cache). */
export function sendJsonText(res: ServerResponse, status: number, text: string, headers: Readonly<Record<string, string>> = {}): void {
  const body = `${text}\n`;
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': Buffer.byteLength(body),
    ...baseHeaders(),
    ...headers,
  });
  res.end(body);
}

/** Send a 204 with no body (a deleted repository). */
export function sendNoContent(res: ServerResponse): void {
  res.writeHead(204, { ...baseHeaders() });
  res.end();
}

/** Send raw bytes with a content type and extra headers. */
export function sendBytes(
  res: ServerResponse,
  status: number,
  data: Buffer,
  contentType: string,
  headers: Readonly<Record<string, string>> = {},
): void {
  res.writeHead(status, {
    'content-type': contentType,
    'content-length': data.length,
    ...baseHeaders(),
    ...headers,
  });
  res.end(data);
}

/** Send a JSON error body: {"message": "..."}. */
export function sendError(res: ServerResponse, status: number, message: string): void {
  sendJson(res, status, { message });
}

/** A 404 JSON error for an unknown route or a missing object. */
export function sendNotFound(res: ServerResponse, what = 'Not Found'): void {
  sendError(res, 404, what);
}

/** A 401 JSON error for a missing or wrong bearer token. */
export function sendUnauthorized(res: ServerResponse): void {
  sendError(res, 401, 'Bad credentials');
}
