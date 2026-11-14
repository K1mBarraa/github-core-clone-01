// Git smart HTTP: info/refs, git-upload-pack (protocol v2 and v0) and
// git-receive-pack (v0). The protocol follows git's wire exactly, because
// the suite speaks it with a real client: pkt-lines, sideband framing,
// v2 capability advertisement, ls-refs, shallow/deepen fetches and
// report-status pushes.
//
// Auth: every git request must carry the process's bearer token; a missing
// or wrong token is 401. A repository under another owner is 404, never 403.

import type { ServerResponse } from 'node:http';
import { authorized, type Ctx } from './ctx.ts';
import {
  decodePktLines,
  encodePktLine,
  FLUSH_PKT,
  planFetch,
  PktLineDecoder,
  readPack,
  writePack,
  type GitObject,
} from './gitobj.ts';
import { sendBytes, sendError, sendNotFound, sendUnauthorized } from './respond.ts';
import { VIEWER_LOGIN, type Repo, type Store } from './store.ts';
import { isOid, ZERO_OID } from './util.ts';

const AGENT = 'github-core-clone-forge/1.0.0';
const UPLOAD_AD = 'application/x-git-upload-pack-advertisement';
const RECEIVE_AD = 'application/x-git-receive-pack-advertisement';
const UPLOAD_RESULT = 'application/x-git-upload-pack-result';
const RECEIVE_RESULT = 'application/x-git-receive-pack-result';

/**
 * The cache key for a fetch response. The response depends only on the
 * protocol version, the repo's objects and (wants, haves, depth, shallow);
 * any new object clears the cache, so a cached response is never stale.
 * This matters for the clone workload: 50 concurrent shallow clones of
 * the same repo would otherwise each re-walk the 200-commit history and
 * re-deflate the same pack on the single-threaded event loop.
 */
function fetchKey(
  version: string,
  repo: Repo,
  wants: string[],
  haves: string[],
  depth: number | undefined,
  shallow: string[],
): string {
  return `${version}|${repo.owner}/${repo.name}|${[...wants].sort().join(',')}|${[...haves].sort().join(',')}|${depth ?? ''}|${[...shallow].sort().join(',')}`;
}

/** Store a fetch response in the cache, evicting everything when full. */
function storeFetchResponse(store: Store, key: string, response: Buffer): Buffer {
  if (store.packCache.size >= 128) store.packCache.clear();
  store.packCache.set(key, response);
  return response;
}

/** The repository a git path names, or null (404: missing, or another owner). */
function gitRepo(ctx: Ctx, owner: string, repo: string): Repo | null {
  if (owner !== VIEWER_LOGIN) return null;
  return ctx.store.getRepo(owner, repo) ?? null;
}

/** All refs of a repo, sorted by name, as (oid, name) pairs. */
function sortedRefs(repo: Repo): Array<{ oid: string; name: string }> {
  const out: Array<{ oid: string; name: string }> = [];
  for (const [name, oid] of repo.refs) out.push({ oid, name });
  out.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  return out;
}

/** The peeled commit of a ref that points at a tag, or null. */
function peelOf(store: Store, repo: Repo, oid: string): string | null {
  const obj = repo.objects.get(oid);
  if (obj === undefined || obj.type !== 'tag') return null;
  return store.peelToCommit(repo, oid);
}

/** The v0 capability list for upload-pack (the client requires side-band-64k and shallow). */
function v0UploadCaps(repo: Repo): string {
  const symref = `symref=HEAD:refs/heads/${repo.defaultBranch}`;
  return `${symref} side-band-64k thin-pack ofs-delta shallow multi_ack agent=${AGENT}`;
}

/** The v0 capability list for receive-pack. */
function v0ReceiveCaps(): string {
  return `report-status side-band-64k delete-refs ofs-delta agent=${AGENT}`;
}

/** The v0 advertisement body for a service. */
function v0Advertisement(store: Store, repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const parts: Buffer[] = [];
  parts.push(encodePktLine(`# service=${service}\n`));
  // The smart-HTTP advertisement puts a flush after the service line, before
  // the refs (a real client requires it on receive-pack).
  parts.push(FLUSH_PKT);
  const refs = sortedRefs(repo);
  const caps = service === 'git-upload-pack' ? v0UploadCaps(repo) : v0ReceiveCaps();
  const head = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
  if (refs.length === 0) {
    // An unborn repo advertises only the capabilities pseudo-ref.
    parts.push(encodePktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
  } else {
    parts.push(encodePktLine(`${head ?? ZERO_OID} HEAD\0${caps}\n`));
    for (const ref of refs) {
      parts.push(encodePktLine(`${ref.oid} ${ref.name}\n`));
      const peeled = peelOf(store, repo, ref.oid);
      if (peeled !== null) parts.push(encodePktLine(`${peeled} ${ref.name}^{}\n`));
    }
  }
  parts.push(FLUSH_PKT);
  return Buffer.concat(parts);
}

/** The v2 advertisement body (upload-pack only). */
function v2Advertisement(): Buffer {
  const lines = [
    'version 2\n',
    `agent=${AGENT}\n`,
    'ls-refs=unborn\n',
    'fetch=shallow\n',
    'object-format=sha1\n',
    'side-band-64k\n',
  ];
  // The service line comes first with a flush, as in the v0 advertisement.
  const parts: Buffer[] = [encodePktLine('# service=git-upload-pack\n'), FLUSH_PKT];
  for (const line of lines) parts.push(encodePktLine(line));
  parts.push(FLUSH_PKT);
  return Buffer.concat(parts);
}

/**
 * Handle GET /{owner}/{repo}.git/info/refs?service=<service>. A v2 request
 * (Git-Protocol: version=2 on upload-pack) gets the v2 advertisement;
 * anything else gets the v0 advertisement with its service preamble.
 */
export function handleInfoRefs(ctx: Ctx, res: ServerResponse, owner: string, repoName: string): void {
  if (!authorized(ctx)) {
    sendUnauthorized(res);
    return;
  }
  const repo = gitRepo(ctx, owner, repoName);
  if (repo === null) {
    sendNotFound(res, 'Not Found');
    return;
  }
  const service = ctx.query.get('service');
  if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
    sendError(res, 400, 'unknown service');
    return;
  }
  const isV2 = service === 'git-upload-pack' && ctx.headers['git-protocol'] === 'version=2';
  const body = isV2 ? v2Advertisement() : v0Advertisement(ctx.store, repo, service);
  sendBytes(res, 200, body, service === 'git-upload-pack' ? UPLOAD_AD : RECEIVE_AD);
}

/** One ls-refs answer line, with its attributes. */
function lsRefLine(store: Store, repo: Repo, oid: string, name: string, opts: { peel: boolean; symrefs: boolean }): string {
  let line = `${oid} ${name}`;
  if (name === 'HEAD' && opts.symrefs) line += ` symref-target:refs/heads/${repo.defaultBranch}`;
  if (opts.peel) {
    const peeled = peelOf(store, repo, oid);
    if (peeled !== null) line += ` peeled:${peeled}`;
  }
  return `${line}\n`;
}

/** Handle a v2 ls-refs command. */
function handleLsRefs(ctx: Ctx, repo: Repo, args: string[]): Buffer {
  let peel = false;
  let symrefs = false;
  let unborn = false;
  const prefixes: string[] = [];
  for (const arg of args) {
    if (arg === 'peel') peel = true;
    else if (arg === 'symrefs') symrefs = true;
    else if (arg === 'unborn') unborn = true;
    else if (arg.startsWith('ref-prefix ')) prefixes.push(arg.slice('ref-prefix '.length));
  }
  const matches = (name: string): boolean => prefixes.length === 0 || prefixes.some((prefix) => name.startsWith(prefix));
  const lines: Buffer[] = [];
  const head = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
  if (head !== undefined) {
    if (matches('HEAD')) lines.push(encodePktLine(lsRefLine(ctx.store, repo, head, 'HEAD', { peel, symrefs })));
  } else if (unborn) {
    if (matches('HEAD')) lines.push(encodePktLine(`unborn HEAD symref-target:refs/heads/${repo.defaultBranch}\n`));
  }
  for (const ref of sortedRefs(repo)) {
    if (!matches(ref.name)) continue;
    lines.push(encodePktLine(lsRefLine(ctx.store, repo, ref.oid, ref.name, { peel, symrefs })));
  }
  lines.push(FLUSH_PKT);
  return Buffer.concat(lines);
}

/** Chunk pack bytes into sideband band-1 packets. A pkt-line is at most
 * 65520 bytes total (4 for its length, 1 for the band), so a data chunk is
 * at most 65515 bytes; one byte more and the packet is rejected. */
function sidebandPackets(data: Buffer): Buffer[] {
  const out: Buffer[] = [];
  const MAX = 65515;
  for (let at = 0; at < data.length; at += MAX) {
    const chunk = data.subarray(at, Math.min(data.length, at + MAX));
    const payload = Buffer.concat([Buffer.from([1]), chunk]);
    out.push(encodePktLine(payload));
  }
  return out;
}

/** Parse a v2 command request: the command line, then argument lines. */
function parseV2Request(body: Buffer): { command: string; args: string[] } {
  const packets = decodePktLines(body);
  const lines: string[] = [];
  for (const pkt of packets) {
    if (pkt.kind === 'data') lines.push(pkt.payload.toString('utf8').replace(/\n$/, ''));
  }
  const commandLine = lines[0] ?? '';
  const match = /^command=([a-z-]+)$/.exec(commandLine);
  if (match === null) throw new Error(`bad v2 command line ${JSON.stringify(commandLine)}`);
  return { command: match[1] as string, args: lines.slice(1) };
}

/** Handle a v2 fetch command. */
function handleFetchV2(ctx: Ctx, repo: Repo, args: string[]): Buffer {
  const wants: string[] = [];
  const haves: string[] = [];
  const shallow: string[] = [];
  let depth: number | undefined;
  for (const arg of args) {
    if (arg.startsWith('want ')) wants.push(arg.slice(5).trim());
    else if (arg.startsWith('have ')) haves.push(arg.slice(5).trim());
    else if (arg.startsWith('shallow ')) shallow.push(arg.slice(8).trim());
    else if (arg.startsWith('deepen ')) {
      const n = Number(arg.slice(7).trim());
      if (Number.isFinite(n) && n > 0) depth = Math.floor(n);
    }
    // thin-pack, no-progress, ofs-delta, filter, agent, object-format,
    // deepen-since, deepen-not and done need no action here.
  }
  const lookup = (oid: string): GitObject | undefined => repo.objects.get(oid);
  // A cached response is byte-identical: the response depends only on the
  // fetch parameters and the repo's objects, and any new object clears it.
  const key = fetchKey('v2', repo, wants, haves, depth, shallow);
  const cached = ctx.store.packCache.get(key);
  if (cached !== undefined) return cached;
  const plan = planFetch(lookup, wants, haves, depth, shallow);
  const parts: Buffer[] = [];
  // Acknowledgments: only when the client sent haves. A fetch with no haves
  // (a clone) expects the packfile (or shallow-info) section first; sending
  // an acknowledgments section there is a protocol error for a real client.
  if (haves.length > 0) {
    parts.push(encodePktLine('acknowledgments\n'));
    const acked = haves.filter((have) => repo.objects.has(have));
    if (acked.length === 0) {
      parts.push(encodePktLine('NAK\n'));
    } else {
      for (const have of acked) parts.push(encodePktLine(`ACK ${have}\n`));
      parts.push(encodePktLine('ready\n'));
    }
    parts.push(FLUSH_PKT);
  }
  // Shallow-info, when the boundary moved.
  if (plan.shallow.length > 0 || plan.unshallow.length > 0) {
    parts.push(encodePktLine('shallow-info\n'));
    for (const oid of plan.shallow) parts.push(encodePktLine(`shallow ${oid}\n`));
    for (const oid of plan.unshallow) parts.push(encodePktLine(`unshallow ${oid}\n`));
    parts.push(FLUSH_PKT);
  }
  // Packfile, only when there is something to send: an empty packfile
  // section is a protocol error for the client.
  if (plan.objects.length > 0) {
    const pack = writePack(plan.objects.map((obj) => ({ type: obj.type, data: obj.data })));
    parts.push(encodePktLine('packfile\n'));
    for (const pkt of sidebandPackets(pack)) parts.push(pkt);
    parts.push(FLUSH_PKT);
  }
  return storeFetchResponse(ctx.store, key, Buffer.concat(parts));
}

/** Handle a v0 fetch: wants, then haves, in two flush-separated sections. */
function handleFetchV0(ctx: Ctx, repo: Repo, body: Buffer): Buffer {
  const packets = decodePktLines(body);
  const wants: string[] = [];
  const haves: string[] = [];
  const shallow: string[] = [];
  let depth: number | undefined;
  for (const pkt of packets) {
    if (pkt.kind === 'flush') {
      continue;
    }
    if (pkt.kind !== 'data') continue;
    const line = pkt.payload.toString('utf8').replace(/\n$/, '');
    if (line.startsWith('want ')) {
      // The first want line may carry space-separated features after the oid.
      wants.push(line.slice(5).split(' ')[0] as string);
    } else if (line.startsWith('shallow ')) shallow.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) {
      const n = Number(line.slice(7).trim());
      if (Number.isFinite(n) && n > 0) depth = Math.floor(n);
    } else if (line.startsWith('have ')) haves.push(line.slice(5).trim());
    // 'done' ends the negotiation.
  }
  const lookup = (oid: string): GitObject | undefined => repo.objects.get(oid);
  const key = fetchKey('v0', repo, wants, haves, depth, shallow);
  const cached = ctx.store.packCache.get(key);
  if (cached !== undefined) return cached;
  const plan = planFetch(lookup, wants, haves, depth, shallow);
  const parts: Buffer[] = [];
  const acked = haves.filter((have) => repo.objects.has(have));
  if (acked.length === 0) parts.push(encodePktLine('NAK\n'));
  else for (const have of acked) parts.push(encodePktLine(`ACK ${have}\n`));
  for (const oid of plan.shallow) parts.push(encodePktLine(`shallow ${oid}\n`));
  for (const oid of plan.unshallow) parts.push(encodePktLine(`unshallow ${oid}\n`));
  if (plan.objects.length > 0) {
    const pack = writePack(plan.objects.map((obj) => ({ type: obj.type, data: obj.data })));
    for (const pkt of sidebandPackets(pack)) parts.push(pkt);
  }
  parts.push(FLUSH_PKT);
  return storeFetchResponse(ctx.store, key, Buffer.concat(parts));
}

/**
 * Handle POST /{owner}/{repo}.git/git-upload-pack: v2 when the Git-Protocol
 * header says version=2, else v0.
 */
export function handleUploadPack(ctx: Ctx, res: ServerResponse, owner: string, repoName: string): void {
  if (!authorized(ctx)) {
    sendUnauthorized(res);
    return;
  }
  const repo = gitRepo(ctx, owner, repoName);
  if (repo === null) {
    sendNotFound(res, 'Not Found');
    return;
  }
  try {
    let body: Buffer;
    if (ctx.headers['git-protocol'] === 'version=2') {
      const { command, args } = parseV2Request(ctx.body);
      if (command === 'ls-refs') body = handleLsRefs(ctx, repo, args);
      else if (command === 'fetch') body = handleFetchV2(ctx, repo, args);
      else throw new Error(`unknown v2 command ${JSON.stringify(command)}`);
    } else {
      body = handleFetchV0(ctx, repo, ctx.body);
    }
    sendBytes(res, 200, body, UPLOAD_RESULT);
  } catch (err) {
    sendError(res, 400, err instanceof Error ? err.message : 'malformed upload-pack request');
  }
}

// ---------------------------------------------------------- receive-pack

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

/** Parse a receive-pack body: command lines, then a raw packfile. */
function parseReceivePack(body: Buffer): { commands: PushCommand[]; pack: Buffer } {
  const decoder = new PktLineDecoder();
  decoder.push(body);
  const commands: PushCommand[] = [];
  let packStart = -1;
  for (;;) {
    const pkt = decoder.next();
    if (pkt === null) throw new Error('receive-pack body ends before the command flush');
    if (pkt.kind === 'flush') {
      packStart = body.length - decoder.rest.length;
      break;
    }
    if (pkt.kind !== 'data') continue;
    let line = pkt.payload.toString('utf8').replace(/\n$/, '');
    // The first command line carries NUL-separated capabilities.
    const nul = line.indexOf('\0');
    if (nul >= 0) line = line.slice(0, nul);
    const parts = line.split(' ');
    if (parts.length !== 3) throw new Error(`bad receive-pack command ${JSON.stringify(line)}`);
    const [oldOid, newOid, ref] = parts as [string, string, string];
    if (!isOid(oldOid) || !isOid(newOid)) throw new Error(`bad object id in ${JSON.stringify(line)}`);
    if (!ref.startsWith('refs/')) throw new Error(`bad ref ${JSON.stringify(ref)}`);
    commands.push({ oldOid, newOid, ref });
  }
  if (commands.length === 0) throw new Error('receive-pack needs at least one command');
  return { commands, pack: body.subarray(packStart) };
}

/** Handle POST /{owner}/{repo}.git/git-receive-pack. */
export function handleReceivePack(ctx: Ctx, res: ServerResponse, owner: string, repoName: string): void {
  if (!authorized(ctx)) {
    sendUnauthorized(res);
    return;
  }
  const repo = gitRepo(ctx, owner, repoName);
  if (repo === null) {
    sendNotFound(res, 'Not Found');
    return;
  }
  let commands: PushCommand[];
  let pack: Buffer;
  try {
    const parsed = parseReceivePack(ctx.body);
    commands = parsed.commands;
    pack = parsed.pack;
  } catch (err) {
    sendError(res, 400, err instanceof Error ? err.message : 'malformed receive-pack request');
    return;
  }
  // Unpack first: every object in the pack is stored, whether or not its
  // ref update is accepted (git unpacks before it updates refs).
  let unpackOk = true;
  let unpackDetail = 'ok';
  if (pack.length > 0) {
    try {
      const read = readPack(pack, (oid) => repo.objects.get(oid));
      for (const [oid, obj] of read.objects) {
        if (!repo.objects.has(oid)) repo.objects.set(oid, obj);
      }
      // New objects can change any fetch's pack: drop cached packs.
      ctx.store.invalidatePackCache();
    } catch (err) {
      unpackOk = false;
      unpackDetail = err instanceof Error ? err.message : 'unpack failed';
    }
  }
  // Then apply the ref updates, one report line each.
  const report: string[] = [`unpack ${unpackOk ? 'ok' : unpackDetail}\n`];
  if (unpackOk) {
    for (const cmd of commands) {
      const current = repo.refs.get(cmd.ref);
      if (cmd.oldOid === ZERO_OID && cmd.newOid !== ZERO_OID) {
        // Create.
        if (current !== undefined) {
          report.push(`ng ${cmd.ref} already exists\n`);
        } else if (!repo.objects.has(cmd.newOid)) {
          report.push(`ng ${cmd.ref} object not found\n`);
        } else {
          repo.refs.set(cmd.ref, cmd.newOid);
          report.push(`ok ${cmd.ref}\n`);
        }
      } else if (cmd.oldOid !== ZERO_OID && cmd.newOid !== ZERO_OID) {
        // Update: must fast-forward from the advertised old value.
        if (current === undefined) {
          report.push(`ng ${cmd.ref} no such ref\n`);
        } else if (current !== cmd.oldOid) {
          report.push(`ng ${cmd.ref} non-fast-forward\n`);
        } else if (!repo.objects.has(cmd.newOid)) {
          report.push(`ng ${cmd.ref} object not found\n`);
        } else if (cmd.newOid !== cmd.oldOid && !ctx.store.isAncestor(repo, cmd.oldOid, cmd.newOid)) {
          report.push(`ng ${cmd.ref} non-fast-forward\n`);
        } else {
          repo.refs.set(cmd.ref, cmd.newOid);
          report.push(`ok ${cmd.ref}\n`);
        }
      } else if (cmd.oldOid !== ZERO_OID && cmd.newOid === ZERO_OID) {
        // Delete.
        if (current === undefined) {
          report.push(`ng ${cmd.ref} no such ref\n`);
        } else {
          repo.refs.delete(cmd.ref);
          report.push(`ok ${cmd.ref}\n`);
        }
      } else {
        report.push(`ng ${cmd.ref} invalid command\n`);
      }
    }
  }
  // The report-status stream is itself pkt-lined inside band 1: each report
  // line is a pkt-line, then a flush, all inside band-1 sideband packets,
  // then the outer flush. A real client reads pkt-lines out of band 1, so
  // raw text there is a protocol error ("bad line length character: unpa").
  const inner: Buffer[] = [];
  for (const line of report) inner.push(encodePktLine(line));
  inner.push(FLUSH_PKT);
  const parts: Buffer[] = [...sidebandPackets(Buffer.concat(inner)), FLUSH_PKT];
  sendBytes(res, 200, Buffer.concat(parts), RECEIVE_RESULT);
}
