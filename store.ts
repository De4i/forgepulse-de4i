import { buildCommit, buildTree, hashObject, isObjectId, parseCommit, parseTag, parseTree,
  type CommitView, type GitObject, type TreeEntry } from './git-client.ts';

export class Fault extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

export type Input = Record<string, unknown>;
export function record(value: unknown): Input {
  if (value === null || typeof value !== 'object' || Array.isArray(value))
    throw new Fault(422, 'Expected an object');
  return value as Input;
}
export function required(input: Input, key: string): string {
  const value = input[key];
  if (typeof value !== 'string' || !value.trim()) throw new Fault(422, `${key} is required`);
  return value;
}
export function optional(input: Input, key: string, fallback = ''): string {
  const value = input[key];
  if (value === undefined || value === null) return fallback;
  if (typeof value !== 'string') throw new Fault(422, `${key} must be a string`);
  return value;
}
export function missing(): never { throw new Fault(404, 'Not Found'); }
export function refNameValid(name: string): boolean {
  return name.startsWith('refs/') && !/[\x00-\x20\x7f~^:?*\[\\]/.test(name)
    && !name.includes('..') && !name.includes('@{') && !name.endsWith('.')
    && name.split('/').every(segment => segment && !segment.startsWith('.') && !segment.endsWith('.lock'));
}

type Account = { login: string; id: number; node_id: string; type: 'User' };
type Label = { id: number; node_id: string; name: string; color: string; description: string };
type Comment = { id: number; node_id: string; body: string; user: Account; created_at: string; updated_at: string };
type State = 'open' | 'closed';
export type Issue = {
  id: number; node_id: string; number: number; title: string; body: string; state: State;
  user: Account; labels: Label[]; comments: number; created_at: string; updated_at: string; closed_at: string | null;
};
export type Pull = {
  id: number; node_id: string; number: number; title: string; body: string; state: State;
  head: string; base: string; headAtOpen: string; baseAtOpen: string; user: Account;
  merged: boolean; merge_commit_sha: string | null; created_at: string; updated_at: string;
  closed_at: string | null; merged_at: string | null;
};
export type Change = { filename: string; status: string; additions: number; deletions: number; changes: number; sha: string };
type Entry = { mode: string; oid: string };
type Snapshot = Map<string, Entry>;
type NodeEntry = { type: 'Repository'; value: Repository } | { type: 'Issue'; value: Issue; repo: Repository }
  | { type: 'PullRequest'; value: Pull; repo: Repository } | { type: 'IssueComment'; value: Comment; repo: Repository }
  | { type: 'User'; value: Account };
const HEADS = 'refs/heads/';
const accountName = /^[a-z\d](?:[a-z\d-]*[a-z\d])?$/i;
export const branchRef = (branch: string): string => HEADS + branch;
function sameEntry(a?: Entry, b?: Entry): boolean { return a?.oid === b?.oid && a?.mode === b?.mode; }

function safePath(path: string): string[] {
  if (!path) return [];
  const parts = path.split('/');
  if (parts.some(p => !p || p === '.' || p === '..' || p.toLowerCase() === '.git' || /[\0\\]/.test(p)))
    throw new Fault(422, 'Invalid file path');
  return parts;
}

export class Repository {
  readonly store: Store;
  readonly owner: Account;
  readonly id: number;
  readonly node_id: string;
  readonly name: string;
  readonly defaultBranch = 'main';
  readonly refs = new Map<string, string>();
  readonly objects = new Map<string, GitObject>();
  readonly packCache = new Map<string, Buffer>();
  readonly issues = new Map<number, Issue>();
  readonly pulls = new Map<number, Pull>();
  readonly labels = new Map<string, Label>();
  readonly comments = new Map<number, Comment[]>();
  readonly nodes = new Set<string>();
  readonly counts = { number: 0 };
  readonly createdAt: string;
  updatedAt: string;
  description: string;
  privateRepo: boolean;
  private readonly commits = new Map<string, CommitView>();
  private readonly trees = new Map<string, TreeEntry[]>();
  private readonly tags = new Map<string, string>();
  private readonly snapshots = new Map<string, Snapshot>();
  private packCacheBytes = 0;

  constructor(store: Store, name: string, description: string, privateRepo: boolean) {
    this.store = store;
    this.owner = store.account;
    this.id = store.allocate();
    this.node_id = store.globalId('Repository', this.id);
    this.name = name;
    this.description = description;
    this.privateRepo = privateRepo;
    this.createdAt = this.updatedAt = store.now;
  }

  get fullName(): string { return `${this.owner.login}/${this.name}`; }
  get headRef(): string { return branchRef(this.defaultBranch); }
  get tip(): string | undefined { return this.refs.get(this.headRef); }

  setRef(ref: string, oid?: string): void {
    if (oid === this.refs.get(ref)) return;
    if (oid === undefined) this.refs.delete(ref);
    else this.refs.set(ref, oid);
    this.packCache.clear();
    this.packCacheBytes = 0;
  }

  cachePack(key: string, packed: Buffer): void {
    const budget = 8 * 1024 * 1024;
    if (packed.length > budget) return;
    if (this.packCacheBytes + packed.length > budget || this.packCache.size === 24) {
      this.packCache.clear();
      this.packCacheBytes = 0;
    }
    this.packCache.set(key, packed);
    this.packCacheBytes += packed.length;
  }

  addObject(type: GitObject['type'], data: Buffer): string {
    const oid = hashObject(type, data);
    if (!this.objects.has(oid)) this.objects.set(oid, { type, data });
    return oid;
  }

  commit(oid: string): CommitView {
    const cached = this.commits.get(oid);
    if (cached) return cached;
    const object = this.objects.get(oid);
    if (object?.type !== 'commit') throw new Fault(422, 'Expected a commit object');
    const value = parseCommit(object.data);
    this.commits.set(oid, value);
    return value;
  }

  tree(oid: string): TreeEntry[] {
    const cached = this.trees.get(oid);
    if (cached) return cached;
    const object = this.objects.get(oid);
    if (object?.type !== 'tree') throw new Fault(422, 'Expected a tree object');
    const value = parseTree(object.data);
    this.trees.set(oid, value);
    return value;
  }

  tagTarget(oid: string): string | undefined {
    const object = this.objects.get(oid);
    if (object?.type !== 'tag') return undefined;
    const cached = this.tags.get(oid);
    if (cached) return cached;
    const target = parseTag(object.data).object;
    this.tags.set(oid, target);
    return target;
  }

  peel(oid: string): string {
    let target: string | undefined;
    const seen = new Set<string>();
    while ((target = this.tagTarget(oid)) !== undefined && !seen.has(oid)) {
      seen.add(oid);
      oid = target;
    }
    return oid;
  }

  resolve(ref?: string | null): string | undefined {
    if (!ref) return this.tip;
    let result = this.refs.get(ref) ?? this.refs.get(branchRef(ref)) ?? this.refs.get('refs/tags/' + ref);
    if (!result && isObjectId(ref) && this.objects.has(ref.toLowerCase())) result = ref.toLowerCase();
    if (!result && ref === 'HEAD') result = this.tip;
    return result && this.peel(result);
  }

  *history(root: string | undefined): Generator<string> {
    if (!root) return;
    const todo = [root];
    const seen = new Set<string>();
    while (todo.length) {
      const oid = todo.pop()!;
      if (seen.has(oid)) continue;
      seen.add(oid);
      yield oid;
      const parents = this.commit(oid).parents;
      for (let i = parents.length - 1; i >= 0; i--) todo.push(parents[i]!);
    }
  }

  ancestor(before: string, after: string): boolean {
    if (before === after) return true;
    if (this.objects.get(after)?.type !== 'commit' || this.objects.get(before)?.type !== 'commit') return false;
    if (this.commit(after).parents.includes(before)) return true;
    for (const oid of this.history(after)) if (oid === before) return true;
    return false;
  }

  snapshot(commit: string | undefined): Snapshot {
    if (!commit) return new Map();
    const root = this.commit(commit).tree;
    const cached = this.snapshots.get(root);
    if (cached) return cached;
    const result: Snapshot = new Map();
    const todo = [{ oid: root, prefix: '' }];
    while (todo.length) {
      const {oid, prefix} = todo.pop()!;
      for (const entry of this.tree(oid)) {
        const path = prefix + entry.name;
        if (entry.mode === '40000' || entry.mode === '040000') todo.push({ oid: entry.oid, prefix: path + '/' });
        else result.set(path, { mode: entry.mode, oid: entry.oid });
      }
    }
    if (this.snapshots.size === 32) this.snapshots.clear();
    this.snapshots.set(root, result);
    return result;
  }

  private writeTree(entries: Snapshot): string {
    type Directory = { directories: Map<string, Directory>; files: Map<string, Entry> };
    const directory = (): Directory => ({ directories: new Map(), files: new Map() });
    const root = directory();
    for (const [path, file] of entries) {
      const names = safePath(path);
      const name = names.pop()!;
      let current = root;
      for (const level of names) {
        if (current.files.has(level)) throw new Fault(409, 'File and directory collide');
        let next = current.directories.get(level);
        if (!next) current.directories.set(level, next = directory());
        current = next;
      }
      if (current.directories.has(name)) throw new Fault(409, 'File and directory collide');
      current.files.set(name, file);
    }
    const emit = (folder: Directory): string => {
      const parts: TreeEntry[] = [];
      for (const [name, file] of folder.files) parts.push({ name, ...file });
      for (const [name, sub] of folder.directories) parts.push({ name, mode: '40000', oid: emit(sub) });
      return this.addObject('tree', buildTree(parts));
    };
    return emit(root);
  }

  writeCommit(ref: string, files: Snapshot, message: string): string {
    const parent = this.refs.get(ref);
    const who = `${this.owner.login} <${this.owner.login}@forge.invalid>`;
    const tree = this.writeTree(files);
    const body = buildCommit({ tree, parents: parent ? [parent] : [], message: message.trimEnd() + '\n',
      author: who, timestamp: this.store.epoch });
    const oid = this.addObject('commit', body);
    this.setRef(ref, oid);
    return oid;
  }

  readContent(path: string, ref?: string | null): unknown {
    const parts = safePath(path);
    const commit = this.resolve(ref);
    if (!commit) return missing();
    const shape = (entry: TreeEntry, location: string, includeBytes: boolean) => {
      const type = entry.mode === '40000' || entry.mode === '040000' ? 'dir'
        : entry.mode === '160000' ? 'submodule' : entry.mode === '120000' ? 'symlink' : 'file';
      const data = this.objects.get(entry.oid)?.data;
      const value = { type, path: location, name: entry.name, sha: entry.oid, size: type === 'dir' ? 0 : data?.length ?? 0 };
      return includeBytes && data ? { ...value, encoding: 'base64', content: data.toString('base64') } : value;
    };
    let tree = this.commit(commit).tree;
    for (let i = 0; i < parts.length; i++) {
      const entry = this.tree(tree).find(candidate => candidate.name === parts[i]);
      if (!entry) return missing();
      if (entry.mode !== '40000' && entry.mode !== '040000')
        return i === parts.length - 1 ? shape(entry, path, true) : missing();
      tree = entry.oid;
    }
    return this.tree(tree).map(entry => shape(entry, path ? path + '/' + entry.name : entry.name, false));
  }

  editContent(path: string, input: Input, remove: boolean) {
    if (!safePath(path).length) throw new Fault(422, 'File path is required');
    const branch = optional(input, 'branch', this.defaultBranch);
    const ref = branchRef(branch);
    if (!refNameValid(ref) || (!this.refs.has(ref) && ref !== this.headRef)) return missing();
    const files = new Map(this.snapshot(this.refs.get(ref)));
    const old = files.get(path);
    if (remove && !old) return missing();
    if ((old && old.oid !== input.sha) || (!old && input.sha != null)) throw new Fault(409, 'sha does not match');
    const message = required(input, 'message');
    if (remove) {
      files.delete(path);
      return { status: 200, result: { content: null, commit: { sha: this.writeCommit(ref, files, message) } } };
    }
    if (typeof input.content !== 'string') throw new Fault(422, 'content is required');
    const encoded = input.content.replace(/\s/g, '');
    if (!/^[A-Za-z0-9+/]*={0,2}$/.test(encoded) || encoded.length % 4 === 1)
      throw new Fault(422, 'Invalid base64 content');
    const data = Buffer.from(encoded, 'base64');
    const oid = this.addObject('blob', data);
    files.set(path, { oid, mode: old?.mode ?? '100644' });
    const sha = this.writeCommit(ref, files, message);
    return { status: old ? 200 : 201, result: { content: { name: path.split('/').at(-1), path, sha: oid,
      size: data.length, type: 'file', encoding: 'base64' }, commit: { sha } } };
  }

  createRef(input: Input) {
    const ref = required(input, 'ref');
    const sha = required(input, 'sha').toLowerCase();
    const object = this.objects.get(sha);
    if (!refNameValid(ref) || !object || this.refs.has(ref) || (ref.startsWith(HEADS) && object.type !== 'commit'))
      throw new Fault(422, 'Invalid ref or SHA');
    if (this.refCollision(ref)) throw new Fault(422, 'Ref name collides with an existing ref');
    this.setRef(ref, sha);
    return this.refObject(ref);
  }

  refCollision(ref: string): boolean {
    for (const existing of this.refs.keys())
      if (existing.startsWith(ref + '/') || ref.startsWith(existing + '/')) return true;
    return false;
  }

  refObject(ref: string) {
    const sha = this.refs.get(ref);
    if (!sha) return missing();
    return { ref, node_id: Buffer.from(`Ref:${this.id}:${ref}`).toString('base64url'),
      object: { sha, type: this.objects.get(sha)?.type } };
  }

  addLabel(input: Input): Label {
    const name = required(input, 'name');
    const key = name.toLowerCase();
    if (this.labels.has(key)) throw new Fault(422, 'Label already exists');
    const id = this.store.allocate();
    const label = { id, node_id: this.store.globalId('Label', id), name,
      color: optional(input, 'color', 'ededed'), description: optional(input, 'description') };
    this.labels.set(key, label);
    return label;
  }

  addIssue(input: Input): Issue {
    const title = required(input, 'title');
    const body = optional(input, 'body');
    const labels: Label[] = [];
    if (input.labels != null) {
      if (!Array.isArray(input.labels)) throw new Fault(422, 'labels must be an array');
      for (const item of input.labels) {
        const name = typeof item === 'string' ? item : required(record(item), 'name');
        const label = this.labels.get(name.toLowerCase());
        if (!label) throw new Fault(422, 'Label does not exist');
        labels.push(label);
      }
    }
    const id = this.store.allocate();
    const issue: Issue = { id, node_id: this.store.globalId('Issue', id), number: ++this.counts.number,
      title, body, labels, state: 'open', user: this.owner, comments: 0,
      created_at: this.store.now, updated_at: this.store.now, closed_at: null };
    this.issues.set(issue.number, issue);
    this.store.register(issue.node_id, {type: 'Issue', value: issue, repo: this});
    return issue;
  }

  issue(number: number): Issue { return this.issues.get(number) ?? missing(); }
  pull(number: number): Pull { return this.pulls.get(number) ?? missing(); }
  updateIssue(issue: Issue, input: Input): Issue {
    const title = input.title === undefined ? issue.title : required(input, 'title');
    const body = optional(input, 'body', issue.body);
    const state = optional(input, 'state', issue.state);
    if (state !== 'open' && state !== 'closed') throw new Fault(422, 'Invalid issue state');
    issue.title = title;
    issue.body = body;
    issue.state = state;
    issue.closed_at = state === 'closed' ? issue.closed_at ?? this.store.now : null;
    issue.updated_at = this.store.now;
    return issue;
  }

  addComment(issue: Issue, input: Input): Comment {
    const body = required(input, 'body');
    const id = this.store.allocate();
    const comment = { id, node_id: this.store.globalId('IssueComment', id), body,
      user: this.owner, created_at: this.store.now, updated_at: this.store.now };
    let list = this.comments.get(issue.number);
    if (!list) this.comments.set(issue.number, list = []);
    list.push(comment);
    issue.comments = list.length;
    this.store.register(comment.node_id, {type: 'IssueComment', value: comment, repo: this});
    return comment;
  }

  addPull(input: Input): Pull {
    const title = required(input, 'title');
    const body = optional(input, 'body');
    const source = required(input, 'head').split(':');
    if (source.length > 2 || (source.length === 2 && source[0]!.toLowerCase() !== this.owner.login.toLowerCase()))
      throw new Fault(422, 'Head must belong to this repository');
    const head = source.at(-1)!;
    const base = required(input, 'base');
    const headAtOpen = this.refs.get(branchRef(head));
    const baseAtOpen = this.refs.get(branchRef(base));
    if (!headAtOpen || !baseAtOpen || head === base) throw new Fault(422, 'Invalid head or base branch');
    const id = this.store.allocate();
    const pull: Pull = { id, node_id: this.store.globalId('PullRequest', id), number: ++this.counts.number,
      title, body, user: this.owner, head, base, headAtOpen, baseAtOpen, state: 'open', merged: false,
      created_at: this.store.now, updated_at: this.store.now, closed_at: null, merged_at: null, merge_commit_sha: null };
    this.pulls.set(pull.number, pull);
    this.store.register(pull.node_id, {type: 'PullRequest', value: pull, repo: this});
    return pull;
  }

  pullTips(pull: Pull): [string, string] {
    if (pull.merged) return [pull.headAtOpen, pull.baseAtOpen];
    return [this.refs.get(branchRef(pull.head)) ?? pull.headAtOpen, this.refs.get(branchRef(pull.base)) ?? pull.baseAtOpen];
  }

  mergeBase(head: string, base: string): string | undefined {
    if (this.ancestor(base, head)) return base;
    if (this.ancestor(head, base)) return head;
    const fromBase = new Set(this.history(base));
    const queue = [head];
    const visited = new Set<string>();
    for (let i = 0; i < queue.length; i++) {
      const current = queue[i]!;
      if (fromBase.has(current)) return current;
      if (visited.has(current)) continue;
      visited.add(current);
      queue.push(...this.commit(current).parents);
    }
    return undefined;
  }

  private changes(pull: Pull) {
    const [head, base] = this.pullTips(pull);
    const common = this.mergeBase(head, base);
    const before = this.snapshot(common);
    const current = this.snapshot(base);
    const after = this.snapshot(head);
    const paths = [...new Set([...before.keys(), ...after.keys()])].sort()
      .filter(path => !sameEntry(before.get(path), after.get(path)));
    return { head, base, before, current, after, paths };
  }

  files(pull: Pull): Change[] {
    const { before, after, paths } = this.changes(pull);
    return paths.map(filename => {
      const old = before.get(filename);
      const next = after.get(filename);
      const [additions, deletions] = lineChanges(old && this.objects.get(old.oid)?.data,
        next && this.objects.get(next.oid)?.data);
      return { filename, status: !old ? 'added' : !next ? 'removed' : 'modified',
        additions, deletions, changes: additions + deletions, sha: (next ?? old)!.oid };
    });
  }

  private mergeTree(pull: Pull): Snapshot | undefined {
    const { before, current, after, paths } = this.changes(pull);
    const result = new Map(current);
    for (const path of paths) {
      const old = before.get(path);
      const base = current.get(path);
      const head = after.get(path);
      if (!sameEntry(old, base) && !sameEntry(base, head)) {
        const merged = this.mergeText(old, base, head);
        if (!merged) return undefined;
        result.set(path, merged);
      } else if (head) result.set(path, head);
      else result.delete(path);
    }
    for (const file of result.keys()) {
      for (let i = file.indexOf('/'); i >= 0; i = file.indexOf('/', i + 1))
        if (result.has(file.slice(0, i))) return undefined;
    }
    return result;
  }

  private mergeText(old?: Entry, base?: Entry, head?: Entry): Entry | undefined {
    if (!old || !base || !head || base.mode !== head.mode || old.mode !== base.mode) return;
    const objects = [old, base, head].map(entry => this.objects.get(entry.oid));
    if (objects.some(object => object?.type !== 'blob' || object.data.includes(0))) return;
    const [a, b, c] = objects.map(o => o!.data.toString('utf8').split(/(?<=\n)/)) as [string[], string[], string[]];
    const edit = (lines: string[]) => {
      let start = 0;
      let end = a.length;
      let last = lines.length;
      while (start < end && start < last && a[start] === lines[start]) start++;
      while (end > start && last > start && a[end - 1] === lines[last - 1]) { end--; last--; }
      return { start, end, lines: lines.slice(start, last) };
    };
    const edits = [edit(b), edit(c)].sort((x, y) => x.start - y.start);
    const [left, right] = [edits[0]!, edits[1]!];
    if (left.end > right.start || left.start === right.start) return;
    const result = a.slice(0, left.start).concat(left.lines, a.slice(left.end, right.start),
      right.lines, a.slice(right.end)).join('');
    return { mode: old.mode, oid: this.addObject('blob', Buffer.from(result)) };
  }

  canMerge(pull: Pull): boolean {
    return pull.merged || (pull.state === 'open' && this.refs.has(branchRef(pull.head))
      && this.refs.has(branchRef(pull.base)) && this.mergeTree(pull) !== undefined);
  }

  merge(pull: Pull, input: Input = {}) {
    const [head, base] = this.pullTips(pull);
    if (pull.merged || pull.state !== 'open' || !this.refs.has(branchRef(pull.head)) || !this.refs.has(branchRef(pull.base)))
      throw new Fault(405, 'Pull request is not mergeable');
    if (input.sha != null && input.sha !== head) throw new Fault(409, 'Head has changed');
    const files = this.mergeTree(pull);
    if (!files) throw new Fault(405, 'Pull request has conflicts');
    const title = optional(input, 'commit_title', `${pull.title} (#${pull.number})`);
    const message = optional(input, 'commit_message', pull.body);
    const sha = this.writeCommit(branchRef(pull.base), files, message ? title + '\n\n' + message : title);
    pull.headAtOpen = head;
    pull.baseAtOpen = base;
    pull.merged = true;
    pull.state = 'closed';
    pull.merge_commit_sha = sha;
    pull.merged_at = pull.closed_at = pull.updated_at = this.store.now;
    return { sha, merged: true, message: 'Pull Request successfully merged' };
  }
}

function lineChanges(before?: Buffer, after?: Buffer): [number, number] {
  if (before?.includes(0) || after?.includes(0)) return [0, 0];
  const lines = (buf?: Buffer) => buf?.length ? buf.toString('utf8').split(/(?<=\n)/) : [];
  const a = lines(before);
  const b = lines(after);
  let left = 0, endA = a.length, endB = b.length;
  while (left < endA && left < endB && a[left] === b[left]) left++;
  while (endA > left && endB > left && a[endA - 1] === b[endB - 1]) { endA--; endB--; }
  if ((endA - left) * (endB - left) > 500_000) return [endB - left, endA - left];
  const lengths = new Uint32Array(endB - left + 1);
  for (let i = left; i < endA; i++) {
    let diagonal = 0;
    for (let j = left; j < endB; j++) {
      const index = j - left + 1;
      const previous = lengths[index]!;
      lengths[index] = a[i] === b[j] ? diagonal + 1 : Math.max(previous, lengths[index - 1]!);
      diagonal = previous;
    }
  }
  const common = lengths.at(-1)!;
  return [endB - left - common, endA - left - common];
}

export class Store {
  readonly account: Account;
  readonly epoch: number;
  readonly now: string;
  readonly repos = new Map<string, Repository>();
  readonly nodes = new Map<string, NodeEntry>();
  private next = 2;

  constructor(login: string, trialTime?: string) {
    if (!accountName.test(login)) throw new Error('Invalid configured forge owner');
    this.epoch = Math.floor((trialTime && Number.isFinite(Date.parse(trialTime)) ? Date.parse(trialTime) : Date.UTC(2026, 0, 1)) / 1000);
    this.now = new Date(this.epoch * 1000).toISOString();
    this.account = { login, id: 1, node_id: this.globalId('User', 1), type: 'User' };
    this.register(this.account.node_id, { type: 'User', value: this.account });
  }

  allocate(): number { return this.next++; }
  globalId(kind: string, id: number): string { return Buffer.from(`${kind}:${id}`).toString('base64url'); }
  register(id: string, entry: NodeEntry): void {
    this.nodes.set(id, entry);
    if (entry.type === 'Repository') entry.value.nodes.add(id);
    else if ('repo' in entry) entry.repo.nodes.add(id);
  }

  lookup(owner: string, repo: string): Repository | undefined {
    return owner.toLowerCase() === this.account.login.toLowerCase() ? this.repos.get(repo.toLowerCase()) : undefined;
  }

  createRepo(input: Input): Repository {
    const name = required(input, 'name');
    if (!/^[a-z\d_.-]{1,100}$/i.test(name) || ['.', '..'].includes(name) || name.toLowerCase().endsWith('.git'))
      throw new Fault(422, 'Invalid repository name');
    const key = name.toLowerCase();
    if (this.repos.has(key)) throw new Fault(422, 'Repository already exists');
    const repo = new Repository(this, name, optional(input, 'description'), input.private === true);
    this.repos.set(key, repo);
    this.register(repo.node_id, { type: 'Repository', value: repo });
    return repo;
  }

  deleteRepo(repo: Repository): void {
    this.repos.delete(repo.name.toLowerCase());
    for (const id of repo.nodes) this.nodes.delete(id);
  }
}
