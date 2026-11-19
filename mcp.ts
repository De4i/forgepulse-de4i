import { Fault, missing, optional, record, type Input, type Store } from './store.ts';
import { issueList, pullView, repoView, type Answer } from './rest.ts';

const revision = '2026-07-28';
const previous = '2025-11-25';
const serverInfo = { name: 'forgepulse', version: '3.0.0' };
const capabilities = { tools: { listChanged: false } };
type Properties = Record<string, {type: 'string' | 'integer' | 'boolean'; description?: string}>;
const string = { type: 'string' } as const;
const integer = { type: 'integer' } as const;
const boolean = { type: 'boolean' } as const;
const target = {owner: string, repo: string};
const requiredTarget = ['owner', 'repo'];
function tool(name: string, description: string, properties: Properties, required: string[]) {
  return { name, description, inputSchema: { type: 'object', properties, required, additionalProperties: false } };
}
const tools = [
  tool('create_repo', 'Create a repository for the authenticated account.', {name: string, description: string, private: boolean}, ['name']),
  tool('get_repo', 'Get a repository by owner and repository name.', target, requiredTarget),
  tool('create_issue', 'Open an issue in a repository.', {...target, title: string, body: string}, [...requiredTarget, 'title']),
  tool('list_issues', 'List issues by state (open, closed, or all).', {...target, state: string}, requiredTarget),
  tool('create_pull_request', 'Open a pull request from an existing branch into its base.', {...target, title: string, body: string,
    head: string, base: string}, [...requiredTarget, 'title', 'head', 'base']),
  tool('merge_pull_request', 'Squash a pull request into its base branch.', {...target, pull_number: integer}, [...requiredTarget, 'pull_number']),
];
const schemas = new Map(tools.map(spec => [spec.name, spec.inputSchema]));

function reply(id: unknown, result: unknown): Answer {
  return { status: 200, body: { jsonrpc: '2.0', id, result } };
}
export function rpcError(id: unknown, code: number, message: string, status = 200): Answer {
  return { status, body: { jsonrpc: '2.0', id, error: { code, message } } };
}
function toolResult(value: unknown): object {
  return { content: [{ type: 'text', text: JSON.stringify(value) }],
    ...(typeof value === 'object' && value !== null && !Array.isArray(value)
      ? { structuredContent: value } : {}) };
}

function invoke(name: string, args: Input, store: Store, origin: string): unknown {
  if (name === 'create_repo') return repoView(store.createRepo(args), origin);
  const repo = store.lookup(args.owner as string, args.repo as string) ?? missing();
  switch (name) {
    case 'get_repo': return repoView(repo, origin);
    case 'create_issue': return repo.addIssue(args);
    case 'list_issues': {
      const state = optional(args, 'state', 'open');
      if (!['open', 'closed', 'all'].includes(state)) throw new Fault(422, 'Invalid issue state');
      return issueList(repo, state);
    }
    case 'create_pull_request': return pullView(repo, repo.addPull(args), origin);
    case 'merge_pull_request': return repo.merge(repo.pull(args.pull_number as number), args);
  }
  throw new Fault(404, 'Unknown tool');
}

export function mcp(value: unknown, store: Store, origin: string): Answer {
  let request: Input;
  try { request = record(value); } catch { return rpcError(null, -32600, 'Invalid request'); }
  const id = request.id ?? null;
  if (request.jsonrpc !== '2.0' || typeof request.method !== 'string'
    || !(id === null || typeof id === 'string' || (typeof id === 'number' && Number.isSafeInteger(id))))
    return rpcError(null, -32600, 'Invalid request');
  if (request.method.startsWith('notifications/') && request.id === undefined) return { status: 202 };
  try {
    const params = request.params === undefined ? {} : record(request.params);
    if (request.method === 'server/discover')
      return reply(id, { serverInfo, capabilities, supportedVersions: [revision, previous] });
    if (request.method === 'initialize')
      return reply(id, { serverInfo, capabilities, protocolVersion: params.protocolVersion === revision ? revision : previous });
    if (request.method === 'ping') return reply(id, {});
    if (request.method === 'tools/list') return reply(id, { tools });
    if (request.method !== 'tools/call') return rpcError(id, -32601, 'Method not found');
    const requested = typeof params.name === 'string' ? params.name : '';
    const name = requested.toLowerCase().split('__').at(-1) ?? '';
    const schema = schemas.get(name);
    if (!schema) return rpcError(id, -32602, 'Unknown tool');
    const args = params.arguments === undefined ? {} : record(params.arguments);
    for (const key of schema.required)
      if (!Object.hasOwn(args, key)) return rpcError(id, -32602, `Missing required argument: ${key}`);
    for (const [key, value] of Object.entries(args)) {
      const field = Object.hasOwn(schema.properties, key) ? schema.properties[key] : undefined;
      const valid = field?.type === 'integer'
        ? typeof value === 'number' && Number.isSafeInteger(value) && value > 0
        : typeof value === field?.type;
      if (!valid) return rpcError(id, -32602, `Invalid argument: ${key}`);
    }
    return reply(id, toolResult(invoke(name, args, store, origin)));
  } catch (error) {
    if (error instanceof Fault) {
      if (error.status === 422) return rpcError(id, -32602, error.message);
      return reply(id, { isError: true, content: [{ type: 'text', text: error.message }] });
    }
    return rpcError(id, -32603, 'Internal error');
  }
}
