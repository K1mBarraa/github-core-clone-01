// The in-memory store: repositories, git objects and refs, issues, pull
// requests, labels and comments. One process, one trial, one fresh store:
// every trial starts empty and the store is never persisted.
//
// The token's owner is the scenario's viewer. The scenario is fixed by the
// kit's public seed (wire.ts SCENARIO_SEED), and buildScenario() names its
// viewer `adaiversen` (id 1000); the token itself carries no login, so the
// forge answers with that login. It is verified by the local runner.

import { b64url, isoOf, trialNowSec, ZERO_OID } from './util.ts';
import {
  buildCommit,
  buildTree,
  hashObject,
  parseCommit,
  parseTag,
  parseTree,
  type GitObject,
  type ObjType,
  type TreeEntry,
} from './gitobj.ts';

/** The login the bearer token names (the scenario's viewer). */
export const VIEWER_LOGIN = 'adaiversen';
/** The viewer's numeric id (the scenario's viewer id). */
export const VIEWER_ID = 1000;
/** The viewer's email, in the scenario's `login@example.invalid` shape. */
export const VIEWER_EMAIL = 'adaiversen@example.invalid';
/** The default branch every repository is born with. */
export const DEFAULT_BRANCH = 'main';

export interface Label {
  name: string;
  color: string;
  description: string;
}

export interface Comment {
  id: number;
  body: string;
  user: string;
  createdSeq: number;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  comments: Comment[];
  createdSeq: number;
  updatedSeq: number;
  user: string;
}

export interface Pull {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  headRef: string;
  headSha: string;
  baseRef: string;
  baseSha: string;
  createdSeq: number;
  user: string;
}

export interface Repo {
  owner: string;
  name: string;
  /** The numeric database id (GraphQL `databaseId`, REST `id`). */
  id: number;
  description: string;
  private: boolean;
  defaultBranch: string;
  createdSeq: number;
  /** Git objects by id. */
  objects: Map<string, GitObject>;
  /** Refs by full name (`refs/heads/main`, `refs/tags/v1`). */
  refs: Map<string, string>;
  issues: Issue[];
  pulls: Pull[];
  labels: Map<string, Label>;
  nextIssueNumber: number;
  nextPullNumber: number;
  nextCommentId: number;
  /** Serialized issue JSON by number, for the hot read path. */
  issueCache: Map<number, string>;
}

/** A file in a flattened tree: path -> blob id and mode. */
export interface FlatFile {
  oid: string;
  mode: string;
}

export class Store {
  readonly repos = new Map<string, Repo>();
  private nextRepoId = 10000;
  private nextIssueId = 20000;
  private nextPullId = 40000;
  private nextCommentId = 30000;
  /** A monotonic sequence: creation order and the deterministic clock. */
  private seq = 0;
  /** Cached ISO timestamps by sequence number (the trial clock is fixed). */
  private isoCache = new Map<number, string>();
  /**
   * Cached fetch responses, keyed by the fetch parameters. The response
   * for a given key depends only on the repo's objects, so any new object
   * clears the cache (see putObject and handleReceivePack).
   */
  packCache = new Map<string, Buffer>();

  /** The next sequence number (1-based). */
  nextSeq(): number {
    this.seq += 1;
    return this.seq;
  }

  /** The deterministic "now" for a new event, in seconds since the epoch. */
  nowSec(): number {
    return this.timeFor(this.nextSeq());
  }

  /** The ISO timestamp for a sequence number, cached (no Date per call). */
  isoFor(seq: number): string {
    return this.isoSec(this.timeFor(seq));
  }

  /** The ISO timestamp for an epoch second, cached (no Date per call). */
  isoSec(sec: number): string {
    let cached = this.isoCache.get(sec);
    if (cached === undefined) {
      cached = isoOf(sec);
      this.isoCache.set(sec, cached);
    }
    return cached;
  }

  /** A deterministic timestamp for sequence `seq` (seconds since the epoch). */
  timeFor(seq: number): number {
    return trialNowSec() + seq;
  }

  key(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.key(owner, name));
  }

  listViewerRepos(): Repo[] {
    const out: Repo[] = [];
    for (const repo of this.repos.values()) {
      if (repo.owner === VIEWER_LOGIN) out.push(repo);
    }
    out.sort((a, b) => a.createdSeq - b.createdSeq);
    return out;
  }

  createRepo(owner: string, name: string, description: string, isPrivate: boolean): Repo | null {
    if (this.repos.has(this.key(owner, name))) return null;
    const repo: Repo = {
      owner,
      name,
      id: this.nextRepoId,
      description,
      private: isPrivate,
      defaultBranch: DEFAULT_BRANCH,
      createdSeq: this.nextSeq(),
      objects: new Map(),
      refs: new Map(),
      issues: [],
      pulls: [],
      labels: new Map(),
      nextIssueNumber: 1,
      nextPullNumber: 1,
      nextCommentId: 1,
      issueCache: new Map(),
    };
    this.nextRepoId += 1;
    this.repos.set(this.key(owner, name), repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.key(owner, name));
  }

  // ---------------------------------------------------------- git objects

  getObject(repo: Repo, oid: string): GitObject | undefined {
    return repo.objects.get(oid);
  }

  hasObject(repo: Repo, oid: string): boolean {
    return repo.objects.has(oid);
  }

  /** Store an object, deduplicated by id; returns its id. */
  putObject(repo: Repo, type: ObjType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!repo.objects.has(oid)) {
      repo.objects.set(oid, { type, data });
      // A new object can change any fetch's pack, so drop cached packs.
      this.packCache.clear();
    }
    return oid;
  }

  /** Drop all cached packs (call after a push adds objects directly). */
  invalidatePackCache(): void {
    this.packCache.clear();
  }

  /** The tip of the default branch, or undefined when the repo is unborn. */
  headOid(repo: Repo): string | undefined {
    return repo.refs.get(`refs/heads/${repo.defaultBranch}`);
  }

  refOid(repo: Repo, ref: string): string | undefined {
    return repo.refs.get(ref);
  }

  setRef(repo: Repo, ref: string, oid: string): void {
    repo.refs.set(ref, oid);
  }

  deleteRef(repo: Repo, ref: string): boolean {
    return repo.refs.delete(ref);
  }

  /** Peel a tag object to its commit, following nested tags. */
  peelToCommit(repo: Repo, oid: string): string | null {
    let current = oid;
    for (let hops = 0; hops < 16; hops += 1) {
      const obj = repo.objects.get(current);
      if (obj === undefined) return null;
      if (obj.type === 'commit') return current;
      if (obj.type !== 'tag') return null;
      current = parseTag(obj.data).object;
    }
    return null;
  }

  /** Whether `ancestor` is an ancestor of (or equal to) `descendant`. */
  isAncestor(repo: Repo, ancestor: string, descendant: string): boolean {
    if (ancestor === descendant) return true;
    const seen = new Set<string>();
    const stack: string[] = [descendant];
    while (stack.length > 0) {
      const oid = stack.pop() as string;
      if (oid === ancestor) return true;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const obj = repo.objects.get(oid);
      if (obj === undefined || obj.type !== 'commit') continue;
      for (const parent of parseCommit(obj.data).parents) stack.push(parent);
    }
    return false;
  }

  /** All ancestors of `oid` (including itself) with their depth from it. */
  ancestors(repo: Repo, oid: string): Map<string, number> {
    const depths = new Map<string, number>();
    const queue: Array<{ oid: string; depth: number }> = [{ oid, depth: 0 }];
    let qhead = 0;
    while (qhead < queue.length) {
      const { oid: current, depth } = queue[qhead++] as { oid: string; depth: number };
      if (depths.has(current)) continue;
      depths.set(current, depth);
      const obj = repo.objects.get(current);
      if (obj === undefined || obj.type !== 'commit') continue;
      for (const parent of parseCommit(obj.data).parents) {
        if (!depths.has(parent)) queue.push({ oid: parent, depth: depth + 1 });
      }
    }
    return depths;
  }

  /** The merge base of two commits: the common ancestor closest to both. */
  mergeBase(repo: Repo, a: string, b: string): string | null {
    const fromA = this.ancestors(repo, a);
    const fromB = this.ancestors(repo, b);
    let best: string | null = null;
    let bestDepth = Infinity;
    for (const [oid, depthA] of fromA) {
      const depthB = fromB.get(oid);
      if (depthB === undefined) continue;
      const depth = depthA + depthB;
      if (depth < bestDepth) {
        bestDepth = depth;
        best = oid;
      }
    }
    return best;
  }

  /** Flatten a commit's tree into path -> file, recursing into subtrees. */
  flatTree(repo: Repo, commitOid: string): Map<string, FlatFile> {
    const out = new Map<string, FlatFile>();
    const obj = repo.objects.get(commitOid);
    if (obj === undefined || obj.type !== 'commit') return out;
    const root = parseCommit(obj.data).tree;
    const walk = (treeOid: string, prefix: string): void => {
      const tree = repo.objects.get(treeOid);
      if (tree === undefined || tree.type !== 'tree') return;
      for (const entry of parseTree(tree.data)) {
        const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
        if (entry.mode === '40000') walk(entry.oid, path);
        else if (entry.mode !== '160000') out.set(path, { oid: entry.oid, mode: entry.mode });
      }
    };
    walk(root, '');
    return out;
  }

  /**
   * Build nested trees from a flat file map, storing every tree object, and
   * return the root tree id. Entries are sorted in git's tree order.
   */
  buildTreeFromFlat(repo: Repo, flat: Map<string, FlatFile>): string {
    // Group files by directory, bottom-up.
    const dirs = new Map<string, Map<string, FlatFile>>();
    const subdirs = new Map<string, Set<string>>();
    for (const [path, file] of flat) {
      const slash = path.lastIndexOf('/');
      const dir = slash < 0 ? '' : path.slice(0, slash);
      const name = slash < 0 ? path : path.slice(slash + 1);
      let entries = dirs.get(dir);
      if (entries === undefined) {
        entries = new Map();
        dirs.set(dir, entries);
      }
      entries.set(name, file);
      // Record every ancestor directory.
      let parent = dir;
      while (parent !== '') {
        const at = parent.lastIndexOf('/');
        const up = at < 0 ? '' : parent.slice(0, at);
        const base = at < 0 ? parent : parent.slice(at + 1);
        let set = subdirs.get(up);
        if (set === undefined) {
          set = new Set();
          subdirs.set(up, set);
        }
        set.add(base);
        parent = up;
      }
    }
    // Depth of a directory: number of slashes + 1 ('' is 0).
    const depthOf = (dir: string): number => (dir === '' ? 0 : dir.split('/').length);
    const allDirs = [...new Set([...dirs.keys(), ...subdirs.keys()])];
    allDirs.sort((a, b) => depthOf(b) - depthOf(a));
    const treeOidOf = new Map<string, string>();
    for (const dir of allDirs) {
      const entries: TreeEntry[] = [];
      const files = dirs.get(dir);
      if (files !== undefined) {
        for (const [name, file] of files) entries.push({ mode: file.mode, name, oid: file.oid });
      }
      const subs = subdirs.get(dir);
      if (subs !== undefined) {
        for (const name of subs) {
          const child = dir === '' ? name : `${dir}/${name}`;
          const oid = treeOidOf.get(child);
          if (oid !== undefined) entries.push({ mode: '40000', name, oid });
        }
      }
      const data = buildTree(entries);
      treeOidOf.set(dir, this.putObject(repo, 'tree', data));
    }
    const root = treeOidOf.get('');
    if (root === undefined) {
      // An empty tree.
      return this.putObject(repo, 'tree', buildTree([]));
    }
    return root;
  }

  /** Create a commit object (stored) and return its id. */
  createCommit(
    repo: Repo,
    tree: string,
    parents: string[],
    message: string,
    timestampSec?: number,
  ): string {
    const ts = timestampSec ?? this.timeFor(this.nextSeq());
    const data = buildCommit({
      tree,
      parents,
      message,
      author: { name: VIEWER_LOGIN, email: VIEWER_EMAIL },
      committer: { name: VIEWER_LOGIN, email: VIEWER_EMAIL },
      timestamp: ts,
    });
    return this.putObject(repo, 'commit', data);
  }

  // ---------------------------------------------------------- three-way merge

  /**
   * Three-way merge of `headOid` into `baseOid` over their merge base.
   * Returns the merged flat tree and whether any path conflicts (both sides
   * changed it differently). A path changed on one side only wins; a path
   * changed identically on both wins; anything else conflicts.
   */
  threeWayMerge(repo: Repo, baseOid: string, headOid: string): { flat: Map<string, FlatFile>; conflict: boolean } {
    const base = this.mergeBase(repo, baseOid, headOid);
    const baseFlat = base === null ? new Map<string, FlatFile>() : this.flatTree(repo, base);
    const headFlat = this.flatTree(repo, headOid);
    const tipFlat = this.flatTree(repo, baseOid);
    const merged = new Map<string, FlatFile>();
    let conflict = false;
    const paths = new Set<string>([...baseFlat.keys(), ...headFlat.keys(), ...tipFlat.keys()]);
    for (const path of paths) {
      const b = baseFlat.get(path);
      const h = headFlat.get(path);
      const t = tipFlat.get(path);
      const same = (x: FlatFile | undefined, y: FlatFile | undefined): boolean =>
        (x === undefined && y === undefined) || (x !== undefined && y !== undefined && x.oid === y.oid && x.mode === y.mode);
      if (same(h, b)) {
        // Unchanged on the head side: keep the base side.
        if (t !== undefined) merged.set(path, t);
      } else if (same(t, b)) {
        // Unchanged on the base side: take the head side.
        if (h !== undefined) merged.set(path, h);
      } else if (same(h, t)) {
        // Both changed identically.
        if (h !== undefined) merged.set(path, h);
      } else {
        conflict = true;
      }
    }
    return { flat: merged, conflict };
  }

  /** Whether a pull request's head can merge cleanly into its base. */
  mergeable(repo: Repo, pr: Pull): boolean {
    const head = repo.refs.get(`refs/heads/${pr.headRef}`);
    const base = repo.refs.get(`refs/heads/${pr.baseRef}`);
    if (head === undefined || base === undefined) return false;
    return !this.threeWayMerge(repo, base, head).conflict;
  }

  // ---------------------------------------------------------- issues and pulls

  createIssue(repo: Repo, title: string, body: string, labels: string[]): Issue {
    const seq = this.nextSeq();
    const issue: Issue = {
      id: this.nextIssueId,
      number: repo.nextIssueNumber,
      title,
      body,
      state: 'open',
      labels: [...labels],
      comments: [],
      createdSeq: seq,
      updatedSeq: seq,
      user: VIEWER_LOGIN,
    };
    this.nextIssueId += 1;
    repo.nextIssueNumber += 1;
    repo.issues.push(issue);
    repo.issueCache.delete(issue.number);
    return issue;
  }

  createPull(repo: Repo, title: string, body: string, headRef: string, headSha: string, baseRef: string, baseSha: string): Pull {
    const seq = this.nextSeq();
    const pr: Pull = {
      id: this.nextPullId,
      number: repo.nextPullNumber,
      title,
      body,
      state: 'open',
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      headRef,
      headSha,
      baseRef,
      baseSha,
      createdSeq: seq,
      user: VIEWER_LOGIN,
    };
    this.nextPullId += 1;
    repo.nextPullNumber += 1;
    repo.pulls.push(pr);
    return pr;
  }

  findIssue(repo: Repo, number: number): Issue | undefined {
    return repo.issues.find((issue) => issue.number === number);
  }

  findPull(repo: Repo, number: number): Pull | undefined {
    return repo.pulls.find((pr) => pr.number === number);
  }

  /** Add a comment to an issue; returns the comment. */
  addComment(repo: Repo, issue: Issue, body: string): Comment {
    const comment: Comment = {
      id: repo.nextCommentId,
      body,
      user: VIEWER_LOGIN,
      createdSeq: this.nextSeq(),
    };
    repo.nextCommentId += 1;
    issue.comments.push(comment);
    this.touchIssue(repo, issue);
    return comment;
  }

  // ---------------------------------------------------------- global ids

  repoGlobalId(repo: Repo): string {
    return b64url(`R:${repo.owner}/${repo.name}`);
  }

  issueGlobalId(repo: Repo, issue: Issue): string {
    return b64url(`I:${repo.owner}/${repo.name}:${issue.number}`);
  }

  pullGlobalId(repo: Repo, pr: Pull): string {
    return b64url(`P:${repo.owner}/${repo.name}:${pr.number}`);
  }

  commentGlobalId(repo: Repo, issue: Issue, comment: Comment): string {
    return b64url(`C:${repo.owner}/${repo.name}:${issue.number}:${comment.id}`);
  }

  /** Resolve an opaque global id back to its object, or null. */
  resolveGlobalId(id: string): { kind: 'repo'; repo: Repo } | { kind: 'issue'; repo: Repo; issue: Issue } | { kind: 'pull'; repo: Repo; pull: Pull } | null {
    for (const repo of this.repos.values()) {
      if (id === this.repoGlobalId(repo)) return { kind: 'repo', repo };
      for (const issue of repo.issues) {
        if (id === this.issueGlobalId(repo, issue)) return { kind: 'issue', repo, issue };
      }
      for (const pr of repo.pulls) {
        if (id === this.pullGlobalId(repo, pr)) return { kind: 'pull', repo, pull: pr };
      }
    }
    return null;
  }

  // ---------------------------------------------------------- JSON shapes

  repoJson(repo: Repo): Record<string, unknown> {
    const created = this.isoFor(repo.createdSeq);
    return {
      id: repo.id,
      node_id: this.repoGlobalId(repo),
      name: repo.name,
      full_name: `${repo.owner}/${repo.name}`,
      owner: { login: repo.owner, id: repo.owner === VIEWER_LOGIN ? VIEWER_ID : 0, type: 'User' },
      private: repo.private,
      description: repo.description,
      fork: false,
      created_at: created,
      updated_at: created,
      pushed_at: created,
      default_branch: repo.defaultBranch,
      has_issues: true,
      has_projects: false,
      has_wiki: false,
      archived: false,
      disabled: false,
      visibility: repo.private ? 'private' : 'public',
    };
  }

  labelJson(label: Label): Record<string, unknown> {
    return { name: label.name, color: label.color, description: label.description, default: false };
  }

  issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
    const labels = issue.labels
      .map((name) => repo.labels.get(name))
      .filter((label): label is Label => label !== undefined)
      .map((label) => this.labelJson(label));
    return {
      id: issue.id,
      node_id: this.issueGlobalId(repo, issue),
      number: issue.number,
      title: issue.title,
      body: issue.body,
      state: issue.state,
      locked: false,
      user: { login: issue.user, id: issue.user === VIEWER_LOGIN ? VIEWER_ID : 0, type: 'User' },
      labels,
      comments: issue.comments.length,
      created_at: this.isoFor(issue.createdSeq),
      updated_at: this.isoFor(issue.updatedSeq),
    };
  }

  /** The cached serialized issue JSON (invalidated on mutation). */
  issueJsonText(repo: Repo, issue: Issue): string {
    const cached = repo.issueCache.get(issue.number);
    if (cached !== undefined) return cached;
    const text = JSON.stringify(this.issueJson(repo, issue));
    repo.issueCache.set(issue.number, text);
    return text;
  }

  touchIssue(repo: Repo, issue: Issue): void {
    issue.updatedSeq = this.nextSeq();
    repo.issueCache.delete(issue.number);
  }

  commentJson(repo: Repo, issue: Issue, comment: Comment): Record<string, unknown> {
    return {
      id: comment.id,
      node_id: this.commentGlobalId(repo, issue, comment),
      body: comment.body,
      user: { login: comment.user, id: comment.user === VIEWER_LOGIN ? VIEWER_ID : 0, type: 'User' },
      created_at: this.isoFor(comment.createdSeq),
      updated_at: this.isoFor(comment.createdSeq),
    };
  }

  pullJson(repo: Repo, pr: Pull): Record<string, unknown> {
    return {
      id: pr.id,
      node_id: this.pullGlobalId(repo, pr),
      number: pr.number,
      title: pr.title,
      body: pr.body,
      state: pr.state,
      locked: false,
      user: { login: pr.user, id: pr.user === VIEWER_LOGIN ? VIEWER_ID : 0, type: 'User' },
      head: { ref: pr.headRef, sha: pr.headSha, label: `${repo.owner}:${pr.headRef}` },
      base: { ref: pr.baseRef, sha: pr.baseSha, label: `${repo.owner}:${pr.baseRef}` },
      merged: pr.merged,
      mergeable: pr.state === 'open' ? this.mergeable(repo, pr) : null,
      merged_at: pr.mergedAt,
      merge_commit_sha: pr.mergeCommitSha,
      created_at: this.isoFor(pr.createdSeq),
      updated_at: this.isoFor(pr.createdSeq),
    };
  }

  /** The changed files of a pull request against its merge base. */
  pullFiles(repo: Repo, pr: Pull): Array<{ filename: string; status: string; additions: number; deletions: number; changes: number }> {
    const base = this.mergeBase(repo, pr.baseSha, pr.headSha);
    const baseFlat = base === null ? new Map<string, FlatFile>() : this.flatTree(repo, base);
    const headFlat = this.flatTree(repo, pr.headSha);
    const out: Array<{ filename: string; status: string; additions: number; deletions: number; changes: number }> = [];
    const paths = new Set<string>([...baseFlat.keys(), ...headFlat.keys()]);
    for (const path of paths) {
      const b = baseFlat.get(path);
      const h = headFlat.get(path);
      if (b !== undefined && h !== undefined && b.oid === h.oid) continue;
      if (b === undefined && h !== undefined) {
        out.push({ filename: path, status: 'added', additions: 1, deletions: 0, changes: 1 });
      } else if (b !== undefined && h === undefined) {
        out.push({ filename: path, status: 'removed', additions: 0, deletions: 1, changes: 1 });
      } else {
        out.push({ filename: path, status: 'modified', additions: 1, deletions: 1, changes: 2 });
      }
    }
    out.sort((a, b) => (a.filename < b.filename ? -1 : a.filename > b.filename ? 1 : 0));
    return out;
  }
}
