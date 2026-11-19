import { Fault, missing, optional, type Input, type Repository, type Store, type Pull } from './store.ts';

export type Answer = { status: number; body?: unknown; encodedJSON?: Buffer; headers?: Record<string, string> };
export type RestContext = { method: string; origin: string; path: string; parts: string[]; query: URLSearchParams;
  json: () => Promise<Input> };
const ok = (body: unknown, status = 200): Answer => ({ status, body });
const number = (part: string | undefined): number => part && /^[1-9]\d*$/.test(part) ? Number(part) : missing();

export function repoView(repo: Repository, origin: string) {
  const name = repo.fullName;
  const endpoint = origin + '/api/v3/repos/' + name;
  return {
    id: repo.id, node_id: repo.node_id, name: repo.name, full_name: name,
    owner: repo.owner, description: repo.description, private: repo.privateRepo,
    visibility: repo.privateRepo ? 'private' : 'public', default_branch: repo.defaultBranch,
    html_url: origin + '/' + name, clone_url: origin + '/' + name + '.git', url: endpoint,
    issues_url: endpoint + '/issues{/number}', pulls_url: endpoint + '/pulls{/number}',
    created_at: repo.createdAt, updated_at: repo.updatedAt, has_issues: true, archived: false,
    fork: false, permissions: { admin: true, maintain: true, push: true, triage: true, pull: true },
  };
}

export function pullView(repo: Repository, pull: Pull, origin: string) {
  const [headSha, baseSha] = repo.pullTips(pull);
  const repository = repoView(repo, origin);
  return {
    id: pull.id, node_id: pull.node_id, number: pull.number, title: pull.title, body: pull.body,
    state: pull.state, user: pull.user, merged: pull.merged, mergeable: repo.canMerge(pull),
    merge_commit_sha: pull.merge_commit_sha, created_at: pull.created_at, updated_at: pull.updated_at,
    closed_at: pull.closed_at, merged_at: pull.merged_at,
    head: { ref: pull.head, label: repo.owner.login + ':' + pull.head, sha: headSha, repo: repository },
    base: { ref: pull.base, label: repo.owner.login + ':' + pull.base, sha: baseSha, repo: repository },
  };
}

export function issueList(repo: Repository, state = 'open', ascending = false) {
  const issues = [...repo.issues.values()].filter(issue => state === 'all' || issue.state === state);
  return ascending ? issues : issues.reverse();
}

function int(value: string | null, fallback: number): number {
  const candidate = Number(value);
  return Number.isSafeInteger(candidate) && candidate > 0 ? candidate : fallback;
}

function pageBounds(ctx: RestContext) {
  const size = Math.min(int(ctx.query.get('per_page'), 30), 100);
  const page = int(ctx.query.get('page'), 1);
  return { size, page, offset: (page - 1) * size };
}

function paginated<T, R = T>(source: Iterable<T>, ctx: RestContext, map?: (value: T) => R): Answer {
  const { size, page, offset } = pageBounds(ctx);
  const values: Array<T | R> = [];
  let index = 0;
  let hasNext = false;
  for (const item of source) {
    if (index++ < offset) continue;
    if (values.length === size) { hasNext = true; break; }
    values.push(map ? map(item) : item);
  }
  if (!hasNext) return ok(values);
  const query = new URLSearchParams(ctx.query);
  query.set('page', String(page + 1));
  return { status: 200, body: values, headers: { link: `<${ctx.origin}${ctx.path}?${query}>; rel="next"` } };
}

function gitIdentity(line: string) {
  const match = /^(.*?) <([^>]*)> (\d+) [+-]\d{4}$/.exec(line);
  return match ? { name: match[1], email: match[2], date: new Date(Number(match[3]) * 1000).toISOString() }
    : { name: line, email: '', date: null };
}

function commitView(repo: Repository, sha: string) {
  const commit = repo.commit(sha);
  return { sha, commit: { author: gitIdentity(commit.author), committer: gitIdentity(commit.committer),
    message: commit.message.trimEnd(), tree: { sha: commit.tree } },
    parents: commit.parents.map(parent => ({ sha: parent })) };
}

export async function rest(ctx: RestContext, store: Store): Promise<Answer> {
  const { method: verb, origin, parts: route, query, json } = ctx;
  const [first, owner, name, resource, key, action] = route;
  if (!route.length && verb === 'GET') {
    return { status: 200, headers: { 'x-oauth-scopes': 'repo, read:org' }, body: {
      current_user_url: origin + '/api/v3/user', repository_url: origin + '/api/v3/repos/{owner}/{repo}',
      graphql_url: origin + '/api/graphql',
    } };
  }
  if (route.length === 1 && verb === 'GET') {
    if (first === 'meta') return ok({ installed_version: 'forgepulse/3' });
    if (first === 'user') return ok(store.account);
  }
  if (first === 'user' && owner === 'repos' && route.length === 2) {
    if (verb === 'POST') return ok(repoView(store.createRepo(await json()), origin), 201);
    if (verb === 'GET') return paginated(store.repos.values(), ctx, repo => repoView(repo, origin));
  }
  if (first !== 'repos' || !owner || !name) return missing();
  const repo = store.lookup(owner, name) ?? missing();
  if (route.length === 3) {
    if (verb === 'GET') return ok(repoView(repo, origin));
    if (verb === 'DELETE') { store.deleteRepo(repo); return { status: 204 }; }
    if (verb === 'PATCH') {
      const input = await json();
      repo.description = optional(input, 'description', repo.description);
      if (typeof input.private === 'boolean') repo.privateRepo = input.private;
      repo.updatedAt = store.now;
      return ok(repoView(repo, origin));
    }
  }
  switch (resource) {
    case 'branches': {
      if (verb !== 'GET') break;
      if (route.length === 4) {
        const branches = [...repo.refs].filter(([ref]) => ref.startsWith('refs/heads/'));
        return paginated(branches, ctx, ([ref, sha]) => ({ name: ref.slice(11), commit: { sha }, protected: false }));
      }
      const branch = route.slice(4).join('/');
      const oid = repo.refs.get('refs/heads/' + branch) ?? missing();
      return ok({ name: branch, commit: { sha: oid }, protected: false });
    }
    case 'commits': {
      if (verb !== 'GET') break;
      if (route.length === 5 && key) return ok(commitView(repo, repo.resolve(key) ?? missing()));
      if (route.length !== 4) break;
      const ref = query.get('sha');
      const oid = repo.resolve(ref);
      if (ref && !oid) return missing();
      return paginated(repo.history(oid), ctx, sha => commitView(repo, sha));
    }
    case 'git': {
      if (key !== 'refs') break;
      if (route.length === 5 && verb === 'POST') return ok(repo.createRef(await json()), 201);
      if (route.length > 5 && verb === 'GET') return ok(repo.refObject('refs/' + route.slice(5).join('/')));
      break;
    }
    case 'contents': {
      const file = route.slice(4).join('/');
      if (verb === 'GET') return ok(repo.readContent(file, query.get('ref')));
      if (verb === 'PUT' || verb === 'DELETE') {
        const result = repo.editContent(file, await json(), verb === 'DELETE');
        return ok(result.result, result.status);
      }
      break;
    }
    case 'labels': {
      if (route.length !== 4) break;
      if (verb === 'POST') return ok(repo.addLabel(await json()), 201);
      if (verb === 'GET') return paginated(repo.labels.values(), ctx);
      break;
    }
    case 'issues': {
      if (route.length === 4) {
        if (verb === 'POST') return ok(repo.addIssue(await json()), 201);
        if (verb === 'GET') return paginated(issueList(repo, query.get('state') ?? 'open', query.get('direction') === 'asc'), ctx);
        break;
      }
      const issue = repo.issue(number(key));
      if (route.length === 5 && verb === 'GET') return { status: 200, encodedJSON: repo.issueJSON(issue) };
      if (route.length === 5 && verb === 'PATCH') return ok(repo.updateIssue(issue, await json()));
      if (route.length === 6 && action === 'comments') {
        if (verb === 'GET') return paginated(repo.comments.get(issue.number) ?? [], ctx);
        if (verb === 'POST') return ok(repo.addComment(issue, await json()), 201);
      }
      break;
    }
    case 'pulls': {
      if (route.length === 4) {
        if (verb === 'POST') return ok(pullView(repo, repo.addPull(await json()), origin), 201);
        if (verb === 'GET') {
          const state = query.get('state') ?? 'open';
          const pulls = [...repo.pulls.values()].filter(pull => state === 'all' || pull.state === state).reverse();
          return paginated(pulls, ctx, pull => pullView(repo, pull, origin));
        }
        break;
      }
      const pull = repo.pull(number(key));
      if (route.length === 5 && verb === 'GET') return ok(pullView(repo, pull, origin));
      if (route.length === 6) {
        if (action === 'files' && verb === 'GET') return paginated(repo.files(pull), ctx);
        if (action === 'merge' && verb === 'PUT') return ok(repo.merge(pull, await json()));
        if (action === 'merge' && verb === 'GET') return pull.merged ? { status: 204 } : missing();
      }
      break;
    }
  }
  throw new Fault(404, 'Not Found');
}
