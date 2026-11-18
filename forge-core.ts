import { createHash, randomBytes } from 'node:crypto';
import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import {
  decodePktLines,
  encodePktLine,
  pktLineFlush,
  readPack,
  writePack,
  hashObject,
  type GitObject,
} from './git-client.ts';

export type Json = Record<string, unknown>;
type AnyMap = Map<string, unknown>;
type RefMap = Map<string, string>;
type FileRecord = { content: string; sha: string; mode: string };
type UserRecord = { login: string; id: number; name: string };
type LabelRecord = { id: string; name: string; color: string; description: string };
type CommentRecord = { id: string; body: string; user: UserRecord };
type IssueRecord = {
  id: string;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  user: UserRecord;
  labels: LabelRecord[];
  comments: CommentRecord[];
  created_at: string;
  updated_at: string;
};
type PullRecord = {
  id: string;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergeable: boolean;
  user: UserRecord;
  head: { ref: string; sha: string };
  base: { ref: string; sha: string };
  files: Array<{ filename: string; status: string; additions: number; deletions: number }>;
  created_at: string;
  updated_at: string;
};
type CommitRecord = {
  sha: string;
  message: string;
  parents: string[];
  author: UserRecord;
  tree: string;
};
type RepositoryRecord = {
  id: string;
  databaseId: number;
  owner: string;
  name: string;
  description: string;
  defaultBranch: string;
  private: boolean;
  files: Map<string, FileRecord>;
  refs: RefMap;
  objects: Map<string, GitObject>;
  commits: CommitRecord[];
  issues: IssueRecord[];
  pulls: PullRecord[];
  labels: LabelRecord[];
  nextIssue: number;
  nextPull: number;
};

const jsonType = 'application/json; charset=utf-8';
const zeroOid = '0'.repeat(40);
const user: UserRecord = { login: 'agent', id: 1, name: 'Forge Agent' };
const repositories = new Map<string, RepositoryRecord>();
let nextRepository = 1;
let nextComment = 1;

function digest(value: string | Buffer): string {
  return createHash('sha1').update(value).digest('hex');
}

function stableId(kind: string, number: number): string {
  return `${kind}_${number}_${digest(`${kind}:${number}`).slice(0, 16)}`;
}

function repositoryKey(owner: string, name: string): string {
  return `${owner}/${name}`.toLowerCase();
}

function now(): string {
  return '2026-01-01T00:00:00.000Z';
}

function clone<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T;
}

function pathName(url: string | undefined): string {
  try {
    return new URL(url ?? '/', 'http://forge.local').pathname;
  } catch {
    return '/';
  }
}

function queryParams(url: string | undefined): URLSearchParams {
  try {
    return new URL(url ?? '/', 'http://forge.local').searchParams;
  } catch {
    return new URLSearchParams();
  }
}

function writeJson(res: ServerResponse, status: number, body: unknown, headers: Json = {}): void {
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, {
    'content-type': jsonType,
    'content-length': Buffer.byteLength(text),
    'x-github-request-id': randomBytes(8).toString('hex'),
    ...headers,
  });
  res.end(text);
}

function writeBytes(res: ServerResponse, status: number, body: Buffer, headers: Json = {}): void {
  res.writeHead(status, { 'content-length': body.length, ...headers });
  res.end(body);
}

function notFound(res: ServerResponse): void {
  writeJson(res, 404, { message: 'Not Found' });
}

function badRequest(res: ServerResponse, message: string): void {
  writeJson(res, 400, { message });
}

function unauthorized(res: ServerResponse): void {
  writeJson(res, 401, { message: 'Requires authentication' });
}

function readJson(req: IncomingMessage): Promise<Json> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on('data', (chunk) => chunks.push(Buffer.from(chunk)));
    req.on('error', reject);
    req.on('end', () => {
      if (chunks.length === 0) return resolve({});
      try {
        resolve(JSON.parse(Buffer.concat(chunks).toString('utf8')) as Json);
      } catch {
        reject(new Error('malformed JSON'));
      }
    });
  });
}

function readBytes(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on('data', (chunk) => chunks.push(Buffer.from(chunk)));
    req.on('error', reject);
    req.on('end', () => resolve(Buffer.concat(chunks)));
  });
}

function repositoryShape(repo: RepositoryRecord): Json {
  const fullName = `${repo.owner}/${repo.name}`;
  return {
    id: repo.id,
    databaseId: repo.databaseId,
    node_id: repo.id,
    name: repo.name,
    full_name: fullName,
    nameWithOwner: fullName,
    owner: { login: repo.owner, id: user.id, type: 'User' },
    private: repo.private,
    description: repo.description,
    fork: false,
    default_branch: repo.defaultBranch,
    defaultBranchRef: { name: repo.defaultBranch },
    has_issues: true,
    hasIssuesEnabled: true,
    has_projects: false,
    has_wiki: false,
    archived: false,
    disabled: false,
    visibility: 'public',
    viewerPermission: 'ADMIN',
    permissions: { admin: true, push: true, pull: true },
    created_at: now(),
    updated_at: now(),
    pushed_at: now(),
    html_url: `http://forge.local/${fullName}`,
    clone_url: `http://forge.local/${fullName}.git`,
    ssh_url: `git@forge.local:${fullName}.git`,
  };
}

function resolveRepository(path: string, allowNameFallback = false): RepositoryRecord | undefined {
  const match = path.match(/^\/api\/v3\/repos\/([^/]+)\/([^/]+)/);
  if (!match) return undefined;
  const exact = repositories.get(repositoryKey(match[1], match[2]));
  if (exact || !allowNameFallback) return exact;
  return [...repositories.values()].find((repo) => repo.name.toLowerCase() === match[2].toLowerCase());
}

function createRepository(owner: string, name: string, description = ''): RepositoryRecord {
  const existing = repositories.get(repositoryKey(owner, name));
  if (existing) return existing;
  const repo: RepositoryRecord = {
    id: stableId('repo', nextRepository),
    databaseId: nextRepository,
    owner,
    name,
    description,
    defaultBranch: 'main',
    private: false,
    files: new Map(),
    refs: new Map(),
    objects: new Map(),
    commits: [],
    issues: [],
    pulls: [],
    labels: [],
    nextIssue: 1,
    nextPull: 1,
  };
  nextRepository += 1;
  repositories.set(repositoryKey(owner, name), repo);
  return repo;
}

function ensureRepository(owner: string, name: string): RepositoryRecord {
  return repositories.get(repositoryKey(owner, name)) ?? createRepository(owner, name);
}

function createRepositoryWithVisibility(owner: string, name: string, description: string, privateRepo: boolean): RepositoryRecord {
  const repo = createRepository(owner, name, description);
  repo.private = privateRepo;
  return repo;
}

function parseRepoPath(path: string): { owner: string; name: string } | undefined {
  const match = path.match(/^\/api\/v3\/repos\/([^/]+)\/([^/]+)/);
  return match ? { owner: match[1], name: match[2] } : undefined;
}

function commitObject(repo: RepositoryRecord, message: string): CommitRecord {
  const previous = repo.commits.at(-1);
  const tree = digest([...repo.files].map(([name, file]) => `${name}:${file.sha}`).join('\n'));
  const commitSha = digest(`${repo.owner}/${repo.name}:${repo.commits.length}:${message}:${tree}`);
  const record: CommitRecord = {
    sha: commitSha,
    message,
    parents: previous ? [previous.sha] : [],
    author: clone(user),
    tree,
  };
  repo.commits.push(record);
  repo.refs.set(`refs/heads/${repo.defaultBranch}`, commitSha);
  repo.objects.set(commitSha, { type: 'commit', data: Buffer.from(message, 'utf8') });
  return record;
}

function addFile(repo: RepositoryRecord, path: string, content: string, message: string): FileRecord {
  const record: FileRecord = { content, sha: digest(content), mode: '100644' };
  repo.files.set(path, record);
  commitObject(repo, message);
  return record;
}

function newIssue(repo: RepositoryRecord, input: Json): IssueRecord {
  const issue: IssueRecord = {
    id: stableId('issue', repo.nextIssue),
    number: repo.nextIssue,
    title: String(input.title ?? ''),
    body: String(input.body ?? ''),
    state: input.state === 'closed' ? 'closed' : 'open',
    user: clone(user),
    labels: [],
    comments: [],
    created_at: now(),
    updated_at: now(),
  };
  repo.nextIssue += 1;
  const labels = Array.isArray(input.labels) ? input.labels : [];
  issue.labels = labels.map((label) => {
    const name = typeof label === 'string' ? label : String((label as Json).name ?? '');
    return repo.labels.find((item) => item.name === name) ?? {
      id: stableId('label', repo.labels.length + 1),
      name,
      color: 'ededed',
      description: '',
    };
  });
  repo.issues.push(issue);
  return issue;
}

function newPull(repo: RepositoryRecord, input: Json): PullRecord {
  const headRef = String(input.head ?? input.headRefName ?? 'feature');
  const baseRef = String(input.base ?? input.baseRefName ?? repo.defaultBranch);
  const pull: PullRecord = {
    id: stableId('pull', repo.nextPull),
    number: repo.nextPull,
    title: String(input.title ?? ''),
    body: String(input.body ?? ''),
    state: 'open',
    merged: false,
    mergeable: true,
    user: clone(user),
    head: { ref: headRef, sha: repo.refs.get(`refs/heads/${headRef}`) ?? digest(`${repo.name}:${headRef}`) },
    base: { ref: baseRef, sha: repo.refs.get(`refs/heads/${baseRef}`) ?? digest(`${repo.name}:${baseRef}`) },
    files: [],
    created_at: now(),
    updated_at: now(),
  };
  repo.nextPull += 1;
  repo.pulls.push(pull);
  return pull;
}

function listPage<T>(items: T[], params: URLSearchParams): { items: T[]; next: number | undefined } {
  const perPage = Math.max(1, Math.min(100, Number(params.get('per_page') ?? 30)));
  const page = Math.max(1, Number(params.get('page') ?? 1));
  const start = (page - 1) * perPage;
  const itemsForPage = items.slice(start, start + perPage);
  return { items: itemsForPage, next: start + perPage < items.length ? page + 1 : undefined };
}

function paginationHeaders(path: string, params: URLSearchParams, next: number | undefined): Json {
  if (!next) return {};
  const copy = new URLSearchParams(params);
  copy.set('page', String(next));
  return { link: `<${path}?${copy.toString()}>; rel="next"` };
}

function authenticate(req: IncomingMessage): boolean {
  const token = process.env.MAKEFUN_SHOP_TOKEN;
  return !token || req.headers.authorization === `Bearer ${token}`;
}

function refObjects(repo: RepositoryRecord): GitObject[] {
  const objects = [...repo.objects.values()];
  if (objects.length > 0) return objects;
  return [{ type: 'commit', data: Buffer.from('empty forge commit', 'utf8') }];
}

function capabilityAdvertisement(service: string, refs: RefMap): Buffer {
  const capabilities = service === 'git-receive-pack'
    ? ['report-status', 'report-status-v2', 'side-band-64k', 'delete-refs', 'ofs-delta', 'agent=forge/2']
    : ['ls-refs', 'fetch=shallow', 'side-band-64k', 'ofs-delta', 'agent=forge/2', 'object-format=sha1', 'unborn'];
  const entries = [...refs.entries()].map(([ref, oid]) => `${oid} ${ref}`);
  const first = entries.length > 0 ? `${entries[0]}\0${capabilities.join(' ')}\n` : `${zeroOid} capabilities^{}\0${capabilities.join(' ')}\n`;
  const rest = entries.slice(1).map((entry) => `${entry}\n`).join('');
  return Buffer.concat([
    encodePktLine(`# service=${service}\n`),
    encodePktLine(first + rest),
    pktLineFlush(),
  ]);
}

function parseReceiveCommands(body: Buffer): Array<{ oldOid: string; newOid: string; ref: string }> {
  const commands: Array<{ oldOid: string; newOid: string; ref: string }> = [];
  let offset = 0;
  while (offset + 4 <= body.length) {
    const size = Number.parseInt(body.subarray(offset, offset + 4).toString('ascii'), 16);
    if (!Number.isFinite(size) || size < 4) break;
    if (size === 4) { offset += 4; break; }
    const line = body.subarray(offset + 4, offset + size).toString('utf8').replace(/\0.*$/, '').trim();
    offset += size;
    const fields = line.split(' ');
    if (fields.length >= 3 && /^[0-9a-f]{40}$/i.test(fields[0]) && /^[0-9a-f]{40}$/i.test(fields[1]) && fields[2].startsWith('refs/')) {
      commands.push({ oldOid: fields[0], newOid: fields[1], ref: fields[2] });
    }
  }
  return commands;
}

function graphType(name: string): Json {
  const fields: Record<string, string[]> = {
    Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issues', 'pullRequests'],
    Issue: ['id', 'number', 'title', 'body', 'state', 'labels', 'comments'],
    PullRequest: ['id', 'number', 'title', 'body', 'state', 'merged', 'mergeable', 'headRefName', 'baseRefName', 'files'],
  };
  return { kind: 'OBJECT', name, fields: (fields[name] ?? []).map((field) => ({ name: field, type: { name: 'String' } })) };
}

function graphRepository(repo: RepositoryRecord): Json {
  return {
    ...repositoryShape(repo),
    issues: { nodes: clone(repo.issues), totalCount: repo.issues.length },
    pullRequests: { nodes: clone(repo.pulls), totalCount: repo.pulls.length },
    issue: repo.issues[0] ?? null,
    pullRequest: repo.pulls[0] ?? null,
  };
}

function graphQuery(query: string, variables: Json): Json {
  if (query.includes('__schema')) return { data: { __schema: { queryType: { name: 'Query' }, mutationType: { name: 'Mutation' } } } };
  if (query.includes('__type')) {
    const typeName = query.match(/__type\s*\(\s*name\s*:\s*"([^"]+)/)?.[1] ?? '';
    return { data: { __type: graphType(typeName) } };
  }
  const input = (variables.input ?? variables) as Json;
  const data: Json = {};
  if (query.includes('viewer')) data.viewer = { login: user.login, id: user.id, name: user.name };
  let repo: RepositoryRecord | undefined;
  if (input.repositoryId) repo = [...repositories.values()].find((item) => item.id === input.repositoryId);
  if (query.includes('createRepository')) {
    repo = createRepository(String(input.ownerLogin ?? input.owner ?? user.login), String(input.name ?? `repo-${nextRepository}`), String(input.description ?? ''));
    data.createRepository = { repository: repositoryShape(repo) };
  }
  if (query.includes('createIssue') && repo) {
    data.createIssue = { issue: newIssue(repo, input) };
  }
  if (query.includes('createPullRequest') && repo) {
    data.createPullRequest = { pullRequest: newPull(repo, input) };
  }
  if (query.includes('repository')) {
    const selected = repo ?? repositories.get(repositoryKey(String(input.owner ?? ''), String(input.name ?? '')));
    data.repository = selected ? graphRepository(selected) : null;
  }
  return { data };
}

function toolDefinitions(): Json[] {
  const common = { owner: { type: 'string' }, repo: { type: 'string' } };
  return [
    { name: 'create_repo', description: 'Create a repository in the forge', inputSchema: { type: 'object', properties: { ...common, description: { type: 'string' } }, required: ['owner', 'repo'] } },
    { name: 'get_repo', description: 'Read a repository', inputSchema: { type: 'object', properties: common, required: ['owner', 'repo'] } },
    { name: 'create_issue', description: 'Create an issue', inputSchema: { type: 'object', properties: { ...common, title: { type: 'string' }, body: { type: 'string' } }, required: ['owner', 'repo', 'title'] } },
    { name: 'list_issues', description: 'List issues', inputSchema: { type: 'object', properties: { ...common, state: { type: 'string' } }, required: ['owner', 'repo'] } },
    { name: 'create_pull_request', description: 'Create a pull request', inputSchema: { type: 'object', properties: { ...common, title: { type: 'string' }, head: { type: 'string' }, base: { type: 'string' } }, required: ['owner', 'repo', 'title', 'head', 'base'] } },
    { name: 'merge_pull_request', description: 'Merge a pull request', inputSchema: { type: 'object', properties: { ...common, pull_number: { type: 'number' } }, required: ['owner', 'repo', 'pull_number'] } },
  ];
}

function jsonRpcError(id: unknown, code: number, message: string): Json {
  return { jsonrpc: '2.0', id, error: { code, message } };
}

function toolResult(value: unknown): Json {
  return { content: [{ type: 'text', text: JSON.stringify(value) }] };
}

async function mcpHandler(req: IncomingMessage, res: ServerResponse): Promise<void> {
  try {
    const body = await readJson(req);
    const id = body.id ?? null;
    if (body.method === 'initialize') {
      return writeJson(res, 200, { jsonrpc: '2.0', id, result: { protocolVersion: '2026-07-28', capabilities: { requests: { list: true, call: true } }, serverInfo: { name: 'forgepulse', version: '2.0' } } });
    }
    if (body.method === 'tools/list') return writeJson(res, 200, { jsonrpc: '2.0', id, result: { tools: toolDefinitions() } });
    if (body.method !== 'tools/call') return writeJson(res, 200, jsonRpcError(id, -32601, 'Unknown method'));
    const name = String((body.params as Json | undefined)?.name ?? '');
    const args = ((body.params as Json | undefined)?.arguments ?? {}) as Json;
    const known = new Set(toolDefinitions().map((item) => String(item.name)));
    if (!known.has(name)) return writeJson(res, 200, jsonRpcError(id, -32601, `Unknown tool: ${name}`));
    const owner = String(args.owner ?? '');
    const repoName = String(args.repo ?? args.name ?? '');
    if (!owner || !repoName) return writeJson(res, 200, jsonRpcError(id, -32602, 'Missing owner or repo'));
    let repo = repositories.get(repositoryKey(owner, repoName));
    if (name === 'create_repo') {
      repo = createRepository(owner, repoName, String(args.description ?? ''));
      return writeJson(res, 200, { jsonrpc: '2.0', id, result: toolResult(repositoryShape(repo)) });
    }
    if (!repo) return writeJson(res, 200, jsonRpcError(id, -32602, 'Repository not found'));
    if (name === 'get_repo') return writeJson(res, 200, { jsonrpc: '2.0', id, result: toolResult(repositoryShape(repo)) });
    if (name === 'create_issue') return writeJson(res, 200, { jsonrpc: '2.0', id, result: toolResult(newIssue(repo, args)) });
    if (name === 'list_issues') return writeJson(res, 200, { jsonrpc: '2.0', id, result: toolResult(repo.issues.filter((item) => !args.state || args.state === 'all' || item.state === args.state)) });
    if (name === 'create_pull_request') return writeJson(res, 200, { jsonrpc: '2.0', id, result: toolResult(newPull(repo, args)) });
    const number = Number(args.pull_number ?? args.number);
    const pull = repo.pulls.find((item) => item.number === number);
    if (!pull) return writeJson(res, 200, jsonRpcError(id, -32602, 'Pull request not found'));
    pull.state = 'closed';
    pull.merged = true;
    return writeJson(res, 200, { jsonrpc: '2.0', id, result: toolResult({ merged: true, sha: digest(`${repo.name}:${number}`), message: 'Pull Request successfully merged' }) });
  } catch {
    writeJson(res, 200, jsonRpcError(null, -32700, 'Parse error'));
  }
}

async function gitHandler(req: IncomingMessage, res: ServerResponse, path: string): Promise<void> {
  const match = path.match(/^\/([^/]+)\/([^/]+?)(?:\.git)\/(info\/refs|git-upload-pack|git-receive-pack)$/);
  if (!match) return notFound(res);
  const repo = repositories.get(repositoryKey(match[1], match[2]));
  if (!repo) return notFound(res);
  const service = match[3].includes('receive') ? 'git-receive-pack' : 'git-upload-pack';
  if (match[3] === 'info/refs') {
    const advertisement = capabilityAdvertisement(service, repo.refs);
    return writeBytes(res, 200, advertisement, { 'content-type': `application/x-${service}-advertisement`, 'cache-control': 'no-cache' });
  }
  const body = await readBytes(req);
  if (service === 'git-receive-pack') {
    const commands = parseReceiveCommands(body);
    const packIndex = body.indexOf(Buffer.from('PACK'));
    if (packIndex >= 0) {
      try {
        const parsed = readPack(body.subarray(packIndex), new Map());
        for (const [oid, object] of parsed.objects) repo.objects.set(oid, object);
      } catch {
        // A malformed pack still receives per-ref failures below.
      }
    }
    const reports: Buffer[] = [];
    for (const command of commands) {
      const current = repo.refs.get(command.ref) ?? zeroOid;
      if (current !== command.oldOid && command.oldOid !== zeroOid) {
        reports.push(encodePktLine(`ng ${command.ref} stale ref\n`));
      } else {
        if (command.newOid === zeroOid) repo.refs.delete(command.ref);
        else repo.refs.set(command.ref, command.newOid);
        reports.push(encodePktLine(`ok ${command.ref}\n`));
      }
    }
    reports.push(pktLineFlush());
    return writeBytes(res, 200, Buffer.concat(reports), { 'content-type': 'application/x-git-receive-pack-result' });
  }
  const pack = writePack(refObjects(repo));
  const response = Buffer.concat([encodePktLine('NAK\n'), encodePktLine('packfile\n'), pack, pktLineFlush()]);
  return writeBytes(res, 200, response, { 'content-type': 'application/x-git-upload-pack-result' });
}

async function restHandler(req: IncomingMessage, res: ServerResponse, path: string): Promise<void> {
  if (!authenticate(req)) return unauthorized(res);
  const method = req.method ?? 'GET';
  const params = queryParams(req.url);
  if (method === 'GET' && (path === '/api/v3' || path === '/api/v3/')) return writeJson(res, 200, { current_user_url: '/api/v3/user', repository_url: '/api/v3/repos/{owner}/{repo}', graphql_url: '/api/graphql' }, { 'x-oauth-scopes': 'repo, read:org' });
  if (method === 'GET' && path === '/api/v3/user') return writeJson(res, 200, user);
  if (method === 'GET' && path === '/api/v3/meta') return writeJson(res, 200, { installed_version: 'forgepulse/2' });
  if (path === '/api/v3/user/repos' && method === 'GET') {
    const page = listPage([...repositories.values()].map(repositoryShape), params);
    return writeJson(res, 200, page.items, paginationHeaders(path, params, page.next));
  }
  if (path === '/api/v3/user/repos' && method === 'POST') {
    const input = await readJson(req);
    return writeJson(res, 201, repositoryShape(createRepositoryWithVisibility(user.login, String(input.name ?? `repo-${nextRepository}`), String(input.description ?? ''), input.private === true)));
  }
  const repoPath = parseRepoPath(path);
  const repo = resolveRepository(path);
  if (!repoPath || !repo) return notFound(res);
  const base = `/api/v3/repos/${repoPath.owner}/${repoPath.name}`;
  if (path === base && method === 'GET') return writeJson(res, 200, repositoryShape(repo));
  if (path === base && method === 'PATCH') {
    const input = await readJson(req);
    if (input.description !== undefined) repo.description = String(input.description);
    return writeJson(res, 200, repositoryShape(repo));
  }
  if (path === `${base}/branches` && method === 'GET') return writeJson(res, 200, [...repo.refs].map(([ref, sha]) => ({ name: ref.replace(/^refs\/heads\//, ''), commit: { sha } })));
  if (path === `${base}/git/refs` && method === 'POST') {
    const input = await readJson(req);
    const ref = String(input.ref ?? '');
    const sha = String(input.sha ?? '');
    if (!ref.startsWith('refs/') || !/^[0-9a-f]{40}$/i.test(sha)) return writeJson(res, 422, { message: 'Invalid ref or SHA' });
    repo.refs.set(ref, sha);
    return writeJson(res, 201, { ref, node_id: stableId('ref', repo.refs.size), object: { sha, type: 'commit' } });
  }
  if (path.startsWith(`${base}/git/refs/`) && method === 'GET') {
    const ref = decodeURIComponent(path.slice(`${base}/git/refs/`.length));
    const sha = repo.refs.get(ref);
    return sha ? writeJson(res, 200, { ref: `refs/${ref}`, object: { sha, type: 'commit' } }) : notFound(res);
  }
  if (path === `${base}/commits` && method === 'GET') {
    const selectedRef = params.get('sha');
    const ordered = [...repo.commits].reverse();
    const filtered = selectedRef ? ordered.filter((item) => item.sha === selectedRef || repo.refs.get(selectedRef) === item.sha) : ordered;
    return writeJson(res, 200, filtered.map((item) => ({ sha: item.sha, commit: { message: item.message, author: item.author, committer: item.author, tree: { sha: item.tree } }, parents: item.parents.map((sha) => ({ sha })) })));
  }
  if (path === `${base}/labels` && method === 'POST') {
    const input = await readJson(req);
    const label: LabelRecord = { id: stableId('label', repo.labels.length + 1), name: String(input.name ?? ''), color: String(input.color ?? 'ededed'), description: String(input.description ?? '') };
    repo.labels.push(label);
    return writeJson(res, 201, label);
  }
  const fileMatch = path.match(new RegExp(`^${base}/contents/(.+)$`));
  if (fileMatch) {
    const fileName = decodeURIComponent(fileMatch[1]);
    if (method === 'GET') {
      const file = repo.files.get(fileName);
      if (file) return writeJson(res, 200, { type: 'file', encoding: 'base64', name: fileName.split('/').at(-1), path: fileName, sha: file.sha, size: Buffer.byteLength(file.content), content: Buffer.from(file.content).toString('base64') });
      const prefix = fileName.endsWith('/') ? fileName : `${fileName}/`;
      const children = [...repo.files].filter(([name]) => name.startsWith(prefix)).map(([name, item]) => ({ type: 'file', name: name.slice(prefix.length), path: name, sha: item.sha, size: Buffer.byteLength(item.content) }));
      return children.length ? writeJson(res, 200, children) : notFound(res);
    }
    const input = await readJson(req);
    if (method === 'PUT') {
      const content = Buffer.from(String(input.content ?? ''), 'base64').toString('utf8');
      const existing = repo.files.get(fileName);
      if (existing && input.sha !== existing.sha) return writeJson(res, 409, { message: 'sha does not match' });
      const file = addFile(repo, fileName, content, String(input.message ?? 'update content'));
      return writeJson(res, existing ? 200 : 201, { content: { path: fileName, sha: file.sha, encoding: 'base64' }, commit: { sha: repo.commits.at(-1)?.sha } });
    }
    if (method === 'DELETE') {
      if (!repo.files.delete(fileName)) return notFound(res);
      const commit = commitObject(repo, String(input.message ?? 'delete content'));
      return writeJson(res, 200, { commit: { sha: commit.sha } });
    }
  }
  const issueMatch = path.match(new RegExp(`^${base}/issues(?:/(\d+))?(?:/comments)?$`));
  if (issueMatch) {
    const number = issueMatch[1] ? Number(issueMatch[1]) : 0;
    const comments = path.endsWith('/comments');
    if (comments && number) {
      const issue = repo.issues.find((item) => item.number === number);
      if (!issue) return notFound(res);
      if (method === 'GET') return writeJson(res, 200, issue.comments);
      if (method === 'POST') {
        const input = await readJson(req);
        const comment: CommentRecord = { id: stableId('comment', nextComment++), body: String(input.body ?? ''), user: clone(user) };
        issue.comments.push(comment);
        return writeJson(res, 201, comment);
      }
    }
    if (number) {
      const issue = repo.issues.find((item) => item.number === number);
      if (!issue) return notFound(res);
      if (method === 'PATCH') {
        const input = await readJson(req);
        if (input.title !== undefined) issue.title = String(input.title);
        if (input.body !== undefined) issue.body = String(input.body);
        if (input.state === 'open' || input.state === 'closed') issue.state = input.state;
      }
      return writeJson(res, 200, issue);
    }
    if (method === 'GET') {
      const state = params.get('state');
      const list = repo.issues.filter((item) => !state || state === 'all' || item.state === state);
      return writeJson(res, 200, list);
    }
    if (method === 'POST') {
      const input = await readJson(req);
      const missing = (Array.isArray(input.labels) ? input.labels : []).some((label) => !repo.labels.some((known) => known.name === String(typeof label === 'string' ? label : (label as Json).name)));
      if (missing) return writeJson(res, 422, { message: 'label does not exist' });
      return writeJson(res, 201, newIssue(repo, input));
    }
  }
  const pullMatch = path.match(new RegExp(`^${base}/pulls(?:/(\d+)(?:/files)?)?$`));
  if (pullMatch) {
    const number = pullMatch[1] ? Number(pullMatch[1]) : 0;
    if (!number && method === 'POST') return writeJson(res, 201, newPull(repo, await readJson(req)));
    if (!number) return writeJson(res, 200, repo.pulls);
    const pull = repo.pulls.find((item) => item.number === number);
    if (!pull) return notFound(res);
    if (path.endsWith('/files')) return writeJson(res, 200, pull.files);
    return writeJson(res, 200, pull);
  }
  const mergeMatch = path.match(new RegExp(`^${base}/pulls/(\d+)/merge$`));
  if (mergeMatch && method === 'PUT') {
    const pull = repo.pulls.find((item) => item.number === Number(mergeMatch[1]));
    if (!pull) return notFound(res);
    pull.merged = true;
    pull.state = 'closed';
    return writeJson(res, 200, { sha: digest(`${repo.name}:${pull.number}`), merged: true, message: 'Pull Request successfully merged' });
  }
  return notFound(res);
}

async function route(req: IncomingMessage, res: ServerResponse): Promise<void> {
  const path = pathName(req.url);
  if (req.method === 'GET' && path === '/healthz') return writeJson(res, 200, { ok: true });
  if (path === '/mcp' && req.method === 'POST') return mcpHandler(req, res);
  if (path === '/api/graphql' && req.method === 'POST') {
    try {
      const input = await readJson(req);
      return writeJson(res, 200, graphQuery(String(input.query ?? ''), (input.variables ?? {}) as Json));
    } catch { return badRequest(res, 'malformed JSON'); }
  }
  if (path.startsWith('/api/v3/')) return restHandler(req, res, path);
  if (path.includes('/info/refs') || path.includes('git-upload-pack') || path.includes('git-receive-pack')) return gitHandler(req, res, path);
  return notFound(res);
}

export function createForgeServer(): Server {
  return createServer((req, res) => { void route(req, res).catch(() => badRequest(res, 'request failed')); });
}

export function resetForgeState(): void {
  repositories.clear();
  nextRepository = 1;
  nextComment = 1;
}
