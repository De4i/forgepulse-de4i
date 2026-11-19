import { timingSafeEqual } from 'node:crypto';
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { advertisement, receivePack, uploadPack } from './git-http.ts';
import { GitProtocolError, type GitService } from './git-client.ts';
import { makeGraphQL } from './graphql.ts';
import { mcp, rpcError } from './mcp.ts';
import { rest, type Answer } from './rest.ts';
import { Fault, missing, record, Store } from './store.ts';

const jsonType = 'application/json; charset=utf-8';
const byteLimit = 32 * 1024 * 1024;
const jsonLimit = 2 * 1024 * 1024;

function readBody(req: IncomingMessage, max: number): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let count = 0;
    let rejected = false;
    req.on('data', (chunk: Buffer) => {
      if (rejected) return;
      count += chunk.length;
      if (count > max) {
        rejected = true;
        chunks.length = 0;
        reject(new Fault(413, 'Request body too large'));
      } else chunks.push(chunk);
    });
    req.once('end', () => {
      if (!rejected) resolve(chunks.length === 1 ? chunks[0]! : Buffer.concat(chunks, count));
    });
    req.once('error', reject);
    req.once('aborted', () => reject(new Fault(400, 'Request aborted')));
  });
}

async function jsonBody(req: IncomingMessage, allowEmpty = false): Promise<unknown> {
  const bytes = await readBody(req, jsonLimit);
  if (!bytes.length && allowEmpty) return {};
  try { return JSON.parse(bytes.toString('utf8')); }
  catch { throw new Fault(400, 'Malformed JSON body'); }
}

function json(res: ServerResponse, answer: Answer) {
  const { headers, status, body, encodedJSON } = answer;
  res.statusCode = status;
  res.setHeader('content-type', jsonType);
  if (headers) for (const [name, value] of Object.entries(headers)) res.setHeader(name, value);
  res.end(encodedJSON ?? (body === undefined ? undefined : JSON.stringify(body)));
}

function bytes(res: ServerResponse, value: Buffer, type: string) {
  res.statusCode = 200;
  res.setHeader('content-type', type);
  res.setHeader('cache-control', 'no-cache');
  res.end(value);
}

function authorized(header: string | undefined, credential: Buffer): boolean {
  if (!header || !header.startsWith('Bearer ')) return false;
  const value = Buffer.from(header.slice(7));
  return value.length === credential.length && timingSafeEqual(value, credential);
}

function requestTarget(req: IncomingMessage) {
  const target = req.url ?? '/';
  if (!target.startsWith('/') || target.startsWith('//')) throw new Fault(400, 'Invalid request target');
  const question = target.indexOf('?');
  const path = question < 0 ? target : target.slice(0, question);
  const query = question < 0 ? '' : target.slice(question + 1);
  return { path, query };
}

function pathSegments(path: string): string[] {
  if (!path || path === '/') return [];
  const segments = (path.endsWith('/') ? path.slice(1, -1) : path.slice(1)).split('/');
  return segments.map(segment => {
    const decoded = decodeURIComponent(segment);
    if (!decoded || decoded === '.' || decoded === '..' || /[\u0000-\u001f\\]/.test(decoded))
      throw new Fault(400, 'Invalid path segment');
    return decoded;
  });
}

function versionTwo(req: IncomingMessage): boolean {
  const value = req.headers['git-protocol'];
  return typeof value === 'string' && value.split(':').includes('version=2');
}

export function serve(owner: string, token: string, now?: string) {
  if (!token || /[\r\n]/.test(token)) throw new Error('MAKEFUN_SHOP_TOKEN is required');
  const key = Buffer.from(token);
  const store = new Store(owner, now);
  const graphql = makeGraphQL(store);
  let requestId = 0;
  const server = createServer({ requestTimeout: 30_000, headersTimeout: 10_000, keepAliveTimeout: 10_000,
    noDelay: true }, (req, res) => {
    res.sendDate = false;
    res.setHeader('x-github-request-id', (++requestId).toString(36));
    void dispatch(req, res).catch(error => {
      if (res.writableEnded || res.destroyed) return;
      if (res.headersSent) { res.destroy(); return; }
      if (error instanceof Fault) json(res, { status: error.status, body: { message: error.message } });
      else if (error instanceof GitProtocolError || error instanceof URIError)
        json(res, { status: 400, body: { message: 'Malformed request' } });
      else json(res, { status: 500, body: { message: 'Internal error' } });
    });
  });
  return server;

  async function dispatch(req: IncomingMessage, res: ServerResponse) {
    const { path, query } = requestTarget(req);
    const method = req.method ?? 'GET';
    if (path === '/healthz' && method === 'GET') return json(res, { status: 200, body: { ok: true } });
    if (!authorized(req.headers.authorization, key)) {
      req.resume();
      return json(res, { status: 401, body: { message: 'Requires authentication' } });
    }
    const origin = 'http://' + (req.headers.host ?? 'localhost');
    if (path === '/api/graphql' && method === 'POST')
      return json(res, { status: 200, body: graphql(await jsonBody(req)) });
    if (path === '/mcp' && method === 'POST') {
      let body: unknown;
      try { body = await jsonBody(req); } catch (error) {
        if (error instanceof Fault && error.status === 400) return json(res, rpcError(null, -32700, 'Parse error', 400));
        throw error;
      }
      return json(res, mcp(body, store, origin));
    }
    if (path === '/mcp' && (method === 'GET' || method === 'DELETE'))
      return json(res, { status: 405, headers: { allow: 'POST' }, body: { message: 'Method not allowed' } });
    if (path === '/api/v3' || path.startsWith('/api/v3/')) {
      const parts = pathSegments(path.slice(7));
      const input = () => jsonBody(req, true).then(value => {
        try { return record(value); } catch { throw new Fault(400, 'Expected a JSON object'); }
      });
      return json(res, await rest({ method, origin, path, parts, query: new URLSearchParams(query), json: input }, store));
    }
    const git = /^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/.exec(path);
    if (!git) return missing();
    const repo = store.lookup(decodeURIComponent(git[1]!), decodeURIComponent(git[2]!)) ?? missing();
    const v2 = versionTwo(req);
    const action = git[3];
    if (action === 'info/refs' && method === 'GET') {
      const service = new URLSearchParams(query).get('service');
      if (service !== 'git-upload-pack' && service !== 'git-receive-pack') return missing();
      return bytes(res, advertisement(repo, service, v2), 'application/x-' + service + '-advertisement');
    }
    if (action === 'info/refs' || method !== 'POST') return missing();
    const service = action as GitService;
    const body = await readBody(req, byteLimit);
    const result = service === 'git-receive-pack' ? receivePack(repo, body) : uploadPack(repo, body, v2);
    return bytes(res, result, 'application/x-' + service + '-result');
  }
}
