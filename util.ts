// Small shared helpers: hashing, base64, deterministic time, JSON.
// Only the Node standard library is used anywhere in this entry.

import { createHash } from 'node:crypto';

/** SHA-1 of `data`, as a lowercase 40-hex string. */
export function sha1Hex(data: Uint8Array | string): string {
  return createHash('sha1').update(data).digest('hex');
}

/** The all-zero object id. */
export const ZERO_OID = '0000000000000000000000000000000000000000';

/** Whether `value` is a 40-hex object id. */
export function isOid(value: string): boolean {
  if (value.length !== 40) return false;
  for (let i = 0; i < 40; i += 1) {
    const c = value.charCodeAt(i);
    const hex = (c >= 48 && c <= 57) || (c >= 97 && c <= 102) || (c >= 65 && c <= 70);
    if (!hex) return false;
  }
  return true;
}

/** Standard base64 of UTF-8 text. */
export function b64encode(text: string): string {
  return Buffer.from(text, 'utf8').toString('base64');
}

/** Standard base64 of bytes. */
export function b64encodeBytes(data: Uint8Array): string {
  return Buffer.from(data).toString('base64');
}

/** Decode base64, ignoring whitespace (GitHub wraps content lines). */
export function b64decode(content: string): Buffer {
  return Buffer.from(content.replace(/[\r\n\t ]+/g, ''), 'base64');
}

/** Base64url (no padding) of UTF-8 text, for opaque global ids. */
export function b64url(text: string): string {
  return Buffer.from(text, 'utf8').toString('base64url');
}

/**
 * The trial's clock: MAKEFUN_TRIAL_NOW is the trial's "now" in milliseconds
 * when the harness sets it; otherwise the scenario's epoch, so a run never
 * reads the wall clock for data. Returns seconds.
 */
export function trialNowSec(): number {
  const raw = process.env.MAKEFUN_TRIAL_NOW;
  if (raw !== undefined && raw !== '') {
    const ms = Number(raw);
    if (Number.isFinite(ms) && ms > 0) return Math.floor(ms / 1000);
  }
  return 1735689600;
}

/** ISO-8601 of `epochSec`. */
export function isoOf(epochSec: number): string {
  return new Date(epochSec * 1000).toISOString();
}

/** JSON.stringify that never throws (falls back to a quoted string). */
export function jsonText(value: unknown): string {
  try {
    const text = JSON.stringify(value);
    return text === undefined ? 'null' : text;
  } catch {
    return '"<unserializable>"';
  }
}

/** A short, stable request id: `req-<counter>`. */
let requestCounter = 0;
export function nextRequestId(): string {
  requestCounter += 1;
  return `req-${requestCounter}`;
}

/** Compare two strings byte-wise (for deterministic ordering). */
export function cmpStr(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}
