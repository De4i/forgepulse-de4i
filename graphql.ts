import { execute, field, parameter as arg, input, enumeration as enumType, object, withIntrospection,
  type Schema, type Resolver } from './gql-execute.ts';
import { Fault, missing, type Input, type Repository, type Issue, type Pull, type Store } from './store.ts';
import { issueList } from './rest.ts';

type NodeSource = { __typename: string; value: any; repo?: Repository };
const node = (type: string, value: unknown, repo?: Repository): NodeSource | null =>
  value == null ? null : { __typename: type, value, repo };
const repository = (repo?: Repository) => node('Repository', repo);
const issue = (repo: Repository, value?: Issue) => node('Issue', value, repo);
const pull = (repo: Repository, value?: Pull) => node('PullRequest', value, repo);
const user = (account: unknown) => node('User', account);
const id = field('ID!', (n: NodeSource) => n.value.node_id);
const dbId = field('Int!', (n: NodeSource) => n.value.id);
const fromValue = (name: string): Resolver => (source: NodeSource) => source.value[name];
const str = (name: string, type = 'String!') => field(type, fromValue(name));
const pageArgs = { first: arg('Int'), after: arg('String'), last: arg('Int'), before: arg('String') };
const states = (params: Input, state: string, merged = false) =>
  !Array.isArray(params.states) || !params.states.length || params.states.includes(merged ? 'MERGED' : state.toUpperCase());

function edgeCursor(scope: string, index: number): string {
  return Buffer.from(scope + ':' + index).toString('base64url');
}
function connection<T>(scope: string, values: T[], args: Input) {
  const boundary = (value: unknown, fallback: number): number => {
    if (value == null) return fallback;
    if (typeof value !== 'string' || !/^[A-Za-z0-9_-]+$/.test(value)) throw new Fault(422, 'Invalid cursor');
    const text = Buffer.from(value, 'base64url').toString('utf8');
    const prefix = scope + ':';
    const index = text.startsWith(prefix) && /^\d+$/.test(text.slice(prefix.length)) ? Number(text.slice(prefix.length)) : NaN;
    if (!Number.isSafeInteger(index) || index >= values.length) throw new Fault(422, 'Invalid cursor');
    return index;
  };
  const count = (value: unknown) => {
    if (value == null) return undefined;
    if (typeof value !== 'number' || value < 0 || value > 100) throw new Fault(422, 'Page size must be between 0 and 100');
    return value;
  };
  const first = count(args.first);
  const last = count(args.last);
  let start = boundary(args.after, -1) + 1;
  let end = Math.max(start, boundary(args.before, values.length));
  if (first !== undefined) end = Math.min(end, start + first);
  if (last !== undefined) start = Math.max(start, end - last);
  if (first === undefined && last === undefined) end = Math.min(end, start + 30);
  const nodes = values.slice(start, end);
  const edges = nodes.map((node, i) => ({ node, cursor: edgeCursor(scope, start + i) }));
  return { nodes, edges, totalCount: values.length,
    pageInfo: { hasNextPage: end < values.length, hasPreviousPage: start > 0,
      startCursor: edges[0]?.cursor ?? null, endCursor: edges.at(-1)?.cursor ?? null } };
}

function defineConnection(schema: Schema, parent: string, item = parent) {
  schema[parent + 'Edge'] = object({ node: field(item), cursor: field('String!') });
  schema[parent + 'Connection'] = object({ totalCount: field('Int!'), nodes: field('[' + item + ']'),
    edges: field('[' + parent + 'Edge]'), pageInfo: field('PageInfo!') });
}

function issueOrder(repo: Repository, args: Input): Issue[] {
  const filters = args.orderBy as Input | undefined;
  const values = issueList(repo, 'all', filters?.direction === 'ASC').filter(value => states(args, value.state));
  if (filters?.field === 'COMMENTS') {
    const direction = filters.direction === 'ASC' ? 1 : -1;
    values.sort((a, b) => direction * (a.comments - b.comments || a.number - b.number));
  }
  return values;
}

function schemaFor(store: Store): Schema {
  const lookup = (id: string) => {
    const match = store.nodes.get(id);
    return match && node(match.type, match.value, 'repo' in match ? match.repo : undefined);
  };
  const objectById = (id: string, type: string): NodeSource => {
    const entry = lookup(id);
    if (!entry || entry.__typename !== type) return missing();
    return entry;
  };
  const payload = (name: string, item: string) => object({ [name]: field(item), clientMutationId: field('String') });
  const schema: Schema = {
    PageInfo: object({ hasNextPage: field('Boolean!'), hasPreviousPage: field('Boolean!'),
      startCursor: field('String'), endCursor: field('String') }),
    DateTime: { kind: 'SCALAR' },
    Node: { kind: 'INTERFACE', fields: {id}, members: ['User', 'Repository', 'Issue', 'PullRequest', 'IssueComment', 'Label'] },
    User: object({ id, databaseId: dbId, login: str('login'), name: str('login', 'String') }),
    IssueState: enumType('OPEN', 'CLOSED'),
    PullRequestState: enumType('OPEN', 'CLOSED', 'MERGED'),
    MergeableState: enumType('MERGEABLE', 'CONFLICTING', 'UNKNOWN'),
    RepositoryPermission: enumType('ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'),
    RepositoryVisibility: enumType('PUBLIC', 'PRIVATE', 'INTERNAL'),
    SearchType: enumType('ISSUE', 'REPOSITORY', 'USER'),
    OrderDirection: enumType('ASC', 'DESC'),
    IssueOrderField: enumType('CREATED_AT', 'UPDATED_AT', 'COMMENTS'),
    IssueOrder: input({ field: arg('IssueOrderField!'), direction: arg('OrderDirection!') }),
    PullRequestOrderField: enumType('CREATED_AT', 'UPDATED_AT'),
    PullRequestOrder: input({ field: arg('PullRequestOrderField!'), direction: arg('OrderDirection!') }),
    PullRequestMergeMethod: enumType('MERGE', 'SQUASH', 'REBASE'),
    PullRequestChangedFile: object({ path: field('String!', change => change.filename), additions: field('Int!'),
      deletions: field('Int!'), changeType: field('String', change => change.status.toUpperCase()) }),
    GitObject: object({ oid: field('String!') }),
    Ref: object({ name: field('String!'), prefix: field('String!'), target: field('GitObject') }),
    Label: object({ id, databaseId: dbId, name: str('name'), color: str('color'), description: str('description', 'String') }),
    IssueComment: object({ id, databaseId: dbId, body: str('body'), author: field('User', n => user(n.value.user)),
      createdAt: str('created_at', 'DateTime!'), updatedAt: str('updated_at', 'DateTime!') }),
    Repository: object({
      id, databaseId: dbId, name: str('name'), nameWithOwner: str('fullName'), description: str('description', 'String'),
      owner: field('User!', n => user(n.value.owner)),
      defaultBranchRef: field('Ref', n => ({ name: n.value.defaultBranch, prefix: 'refs/heads/',
        target: n.value.tip ? { oid: n.value.tip } : null })),
      viewerPermission: field('RepositoryPermission', () => 'ADMIN'),
      hasIssuesEnabled: field('Boolean!', () => true),
      isPrivate: field('Boolean!', n => n.value.privateRepo),
      visibility: field('RepositoryVisibility!', n => n.value.privateRepo ? 'PRIVATE' : 'PUBLIC'),
      issue: field('Issue', (n, a) => issue(n.value, n.value.issues.get(a.number)), { number: arg('Int!') }),
      issues: field('IssueConnection!', (n, a) => connection('issue:' + n.value.id,
        issueOrder(n.value, a).map(value => issue(n.value, value)), a),
        { ...pageArgs, states: arg('[IssueState!]'), orderBy: arg('IssueOrder') }),
      pullRequest: field('PullRequest', (n, a) => pull(n.value, n.value.pulls.get(a.number)), { number: arg('Int!') }),
      pullRequests: field('PullRequestConnection!', (n, a) => {
        const values = [...(n.value as Repository).pulls.values()].filter(value => states(a, value.state, value.merged));
        if (a.orderBy?.direction !== 'ASC') values.reverse();
        return connection('pull:' + n.value.id, values.map(value => pull(n.value, value)), a);
      }, { ...pageArgs, states: arg('[PullRequestState!]'), orderBy: arg('PullRequestOrder') }),
      ref: field('Ref', (n, a) => {
        const oid = n.value.refs.get(a.qualifiedName);
        if (!oid) return null;
        const prefix = a.qualifiedName.startsWith('refs/heads/') ? 'refs/heads/' : 'refs/tags/';
        return { name: a.qualifiedName.slice(prefix.length), prefix, target: { oid } };
      }, {qualifiedName: arg('String!')}),
    }),
    Issue: object({
      id, databaseId: dbId, number: str('number', 'Int!'), title: str('title'), body: str('body'),
      state: field('IssueState!', n => n.value.state.toUpperCase()), closed: field('Boolean!', n => n.value.state === 'closed'),
      createdAt: str('created_at', 'DateTime!'), updatedAt: str('updated_at', 'DateTime!'), closedAt: str('closed_at', 'DateTime'),
      author: field('User', n => user(n.value.user)), repository: field('Repository!', n => repository(n.repo)),
      labels: field('LabelConnection', (n, a) => connection('label:' + n.value.id,
        n.value.labels.map((label: unknown) => node('Label', label)), a), pageArgs),
      comments: field('IssueCommentConnection!', (n, a) => connection('comment:' + n.value.id,
        (n.repo.comments.get(n.value.number) ?? []).map((value: unknown) => node('IssueComment', value, n.repo)), a), pageArgs),
    }),
    PullRequest: object({
      id, databaseId: dbId, number: str('number', 'Int!'), title: str('title'), body: str('body'),
      headRefName: str('head'), baseRefName: str('base'), merged: str('merged', 'Boolean!'),
      mergeable: field('MergeableState!', n => n.repo.canMerge(n.value) ? 'MERGEABLE' : 'CONFLICTING'),
      state: field('PullRequestState!', n => n.value.merged ? 'MERGED' : n.value.state.toUpperCase()),
      headRefOid: field('String!', n => n.repo.pullTips(n.value)[0]),
      baseRefOid: field('String!', n => n.repo.pullTips(n.value)[1]),
      createdAt: str('created_at', 'DateTime!'), updatedAt: str('updated_at', 'DateTime!'),
      closedAt: str('closed_at', 'DateTime'), mergedAt: str('merged_at', 'DateTime'),
      author: field('User', n => user(n.value.user)), repository: field('Repository!', n => repository(n.repo)),
      files: field('PullRequestChangedFileConnection', (n, a) => connection('file:' + n.value.id, n.repo.files(n.value), a), pageArgs),
    }),
    SearchResultItem: { kind: 'UNION', members: ['Issue', 'PullRequest', 'Repository', 'User'] },
    SearchResultItemConnection: object({ nodes: field('[SearchResultItem]'), issueCount: field('Int!'),
      repositoryCount: field('Int!'), userCount: field('Int!'), pageInfo: field('PageInfo!') }),
    CreateIssueInput: input({ repositoryId: arg('ID!'), title: arg('String!'), body: arg('String'),
      clientMutationId: arg('String') }),
    CloseIssueInput: input({ issueId: arg('ID!'), clientMutationId: arg('String') }),
    CreatePullRequestInput: input({ repositoryId: arg('ID!'), headRefName: arg('String!'), baseRefName: arg('String!'),
      title: arg('String!'), body: arg('String'), draft: arg('Boolean'), maintainerCanModify: arg('Boolean'), clientMutationId: arg('String') }),
    MergePullRequestInput: input({ pullRequestId: arg('ID!'), mergeMethod: arg('PullRequestMergeMethod'), expectedHeadOid: arg('String'),
      commitHeadline: arg('String'), commitBody: arg('String'), clientMutationId: arg('String') }),
    AddCommentInput: input({ subjectId: arg('ID!'), body: arg('String!'), clientMutationId: arg('String') }),
    CreateRepositoryInput: input({ name: arg('String!'), description: arg('String'), visibility: arg('RepositoryVisibility!'),
      clientMutationId: arg('String') }),
    CreateIssuePayload: payload('issue', 'Issue'),
    CloseIssuePayload: payload('issue', 'Issue'),
    CreatePullRequestPayload: payload('pullRequest', 'PullRequest'),
    MergePullRequestPayload: payload('pullRequest', 'PullRequest'),
    AddCommentPayload: payload('commentEdge', 'IssueCommentEdge'),
    CreateRepositoryPayload: payload('repository', 'Repository'),
    Query: object({
      viewer: field('User!', () => user(store.account)),
      repository: field('Repository', (_, a) => repository(store.lookup(a.owner, a.name)),
        { owner: arg('String!'), name: arg('String!'), followRenames: arg('Boolean', true) }),
      node: field('Node', (_, a) => lookup(a.id), { id: arg('ID!') }),
      nodes: field('[Node]!', (_, a) => a.ids.map(lookup), { ids: arg('[ID!]!') }),
      search: field('SearchResultItemConnection!', (_, a) => {
        const qualifiers = a.query.toLowerCase().split(/\s+/);
        const ownerRepo = qualifiers.find((term: string) => term.startsWith('repo:'))?.slice(5);
        const phrase = qualifiers.filter((term: string) => !term.includes(':')).join(' ');
        const results: NodeSource[] = [];
        let issueCount = 0, repositoryCount = 0, userCount = 0;
        if (a.type === 'USER' && store.account.login.toLowerCase().includes(phrase)) {
          results.push(user(store.account)!);
          userCount++;
        }
        for (const repo of store.repos.values()) {
          if (ownerRepo && repo.fullName.toLowerCase() !== ownerRepo) continue;
          if (a.type === 'REPOSITORY' && repo.fullName.toLowerCase().includes(phrase)) {
            results.push(repository(repo)!);
            repositoryCount++;
          }
          if (a.type !== 'ISSUE') continue;
          const open = qualifiers.includes('is:open');
          const closed = qualifiers.includes('is:closed');
          const matches = (value: Issue | Pull) => (!open || value.state === 'open') && (!closed || value.state === 'closed')
            && (value.title + ' ' + value.body).toLowerCase().includes(phrase);
          if (!qualifiers.includes('is:pr')) for (const value of repo.issues.values()) if (matches(value)) {
            results.push(issue(repo, value)!); issueCount++;
          }
          if (!qualifiers.includes('is:issue')) for (const value of repo.pulls.values()) if (matches(value)) {
            results.push(pull(repo, value)!); issueCount++;
          }
        }
        return { ...connection('search:' + a.query, results, a), issueCount, repositoryCount, userCount };
      }, { ...pageArgs, query: arg('String!'), type: arg('SearchType!') }),
    }),
    Mutation: object({
      createIssue: field('CreateIssuePayload', (_, a) => {
        const { input } = a;
        const repo: Repository = objectById(input.repositoryId, 'Repository').value;
        return { issue: issue(repo, repo.addIssue(input)), clientMutationId: input.clientMutationId };
      }, { input: arg('CreateIssueInput!') }),
      closeIssue: field('CloseIssuePayload', (_, a) => {
        const { input } = a;
        const found = objectById(input.issueId, 'Issue');
        found.repo!.updateIssue(found.value, {state: 'closed'});
        return { issue: found, clientMutationId: input.clientMutationId };
      }, { input: arg('CloseIssueInput!') }),
      createPullRequest: field('CreatePullRequestPayload', (_, a) => {
        const { input } = a;
        const repo: Repository = objectById(input.repositoryId, 'Repository').value;
        const value = repo.addPull({ ...input, head: input.headRefName, base: input.baseRefName });
        return { pullRequest: pull(repo, value), clientMutationId: input.clientMutationId };
      }, { input: arg('CreatePullRequestInput!') }),
      mergePullRequest: field('MergePullRequestPayload', (_, a) => {
        const { input } = a;
        const found = objectById(input.pullRequestId, 'PullRequest');
        if (input.mergeMethod && input.mergeMethod !== 'SQUASH') throw new Fault(405, 'Only squash merges are supported');
        found.repo!.merge(found.value, { sha: input.expectedHeadOid, commit_title: input.commitHeadline, commit_message: input.commitBody });
        return { pullRequest: found, clientMutationId: input.clientMutationId };
      }, { input: arg('MergePullRequestInput!') }),
      addComment: field('AddCommentPayload', (_, a) => {
        const { input } = a;
        const found = objectById(input.subjectId, 'Issue');
        const comment = found.repo!.addComment(found.value, input);
        const cursor = edgeCursor('comment:' + found.value.id, found.value.comments - 1);
        return { commentEdge: { cursor, node: node('IssueComment', comment, found.repo) }, clientMutationId: input.clientMutationId };
      }, { input: arg('AddCommentInput!') }),
      createRepository: field('CreateRepositoryPayload', (_, a) => {
        const { input } = a;
        if (input.visibility === 'INTERNAL') throw new Fault(422, 'Internal repository visibility is unavailable');
        const repo = store.createRepo({ ...input, private: input.visibility === 'PRIVATE' });
        return { repository: repository(repo), clientMutationId: input.clientMutationId };
      }, { input: arg('CreateRepositoryInput!') }),
    }),
  };
  for (const name of ['Issue', 'PullRequest', 'PullRequestChangedFile', 'IssueComment', 'Label']) defineConnection(schema, name);
  return withIntrospection(schema);
}

export function makeGraphQL(store: Store): (body: unknown) => unknown {
  const schema = schemaFor(store);
  const bad = (message: string) => ({ errors: [{ message }] });
  const single = (doc: unknown) => {
    if (typeof doc !== 'object' || !doc || Array.isArray(doc)) return bad('Expected a GraphQL request object.');
    const { query, variables, operationName } = doc as Input;
    if (typeof query !== 'string') return bad('Expected a GraphQL query string.');
    if (variables != null && (typeof variables !== 'object' || Array.isArray(variables))) return bad('Expected variables to be an object.');
    if (operationName != null && typeof operationName !== 'string') return bad('Expected operationName to be a string.');
    return execute(schema, query, (variables ?? {}) as Input, operationName as string | null);
  };
  return body => Array.isArray(body) ? (body.length && body.length <= 25 ? body.map(single) : bad('Invalid batch size.')) : single(body);
}
