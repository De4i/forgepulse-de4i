import { createHash } from 'node:crypto';
import { deflateSync } from 'node:zlib';
import { ZERO_OID, PKT_LINE_MAX_PAYLOAD, encodePktLine, pktLineFlush, pktLineDelim, decodePktLines,
  isObjectId, readPack, parseCommit, parseTree, parseTag, type GitObject, type GitService } from './git-client.ts';
import { Fault, refNameValid, type Repository } from './store.ts';

const FLUSH = pktLineFlush();
const DELIM = pktLineDelim();
const EMPTY = Buffer.alloc(0);
const UPLOAD = 'git-upload-pack';
const RECEIVE = 'git-receive-pack';
const AGENT = 'agent=forgepulse/3';
const uploadV2 = Buffer.concat(['version 2', AGENT, 'ls-refs=unborn', 'fetch=shallow wait-for-done',
  'object-format=sha1'].map(s => encodePktLine(s + '\n')).concat(FLUSH));
const receiveCaps = ['report-status', 'side-band-64k', 'delete-refs', 'ofs-delta', AGENT, 'object-format=sha1'];
const uploadCaps = ['side-band-64k', 'shallow', 'no-progress', 'include-tag', 'ofs-delta', AGENT, 'object-format=sha1'];
const preambles = {
  [UPLOAD]: Buffer.concat([encodePktLine(`# service=${UPLOAD}\n`), FLUSH]),
  [RECEIVE]: Buffer.concat([encodePktLine(`# service=${RECEIVE}\n`), FLUSH]),
};

function sortedRefs(repo: Repository): Array<[string, string]> {
  return [...repo.refs].sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0);
}

export function advertisement(repo: Repository, service: GitService, v2: boolean): Buffer {
  if (service === UPLOAD && v2) return uploadV2;
  const caps = service === RECEIVE ? receiveCaps : uploadCaps.concat(`symref=HEAD:${repo.headRef}`);
  const refs = sortedRefs(repo);
  if (service === UPLOAD && repo.tip) refs.unshift(['HEAD', repo.tip]);
  if (!refs.length) refs.push(['capabilities^{}', ZERO_OID]);
  const lines = refs.map(([ref, oid], index) =>
    encodePktLine(`${oid} ${ref}${index === 0 ? '\0' + caps.join(' ') : ''}\n`));
  for (const [ref, oid] of refs) {
    if (ref.startsWith('refs/tags/')) {
      const peeled = repo.peel(oid);
      if (peeled !== oid) lines.push(encodePktLine(`${peeled} ${ref}^{}\n`));
    }
  }
  return Buffer.concat([preambles[service], ...lines, FLUSH]);
}

function sideband(body: Buffer, band = 1): Buffer[] {
  const result: Buffer[] = [];
  for (let offset = 0; offset < body.length; offset += PKT_LINE_MAX_PAYLOAD - 1) {
    const count = Math.min(PKT_LINE_MAX_PAYLOAD - 1, body.length - offset);
    const packet = Buffer.allocUnsafe(count + 5);
    packet.write((count + 5).toString(16).padStart(4, '0'), 0, 4, 'ascii');
    packet[4] = band;
    body.copy(packet, 5, offset, offset + count);
    result.push(packet);
  }
  return result;
}

type Update = { old: string; next: string; ref: string };
function parseUpdates(body: Buffer) {
  const commands: Update[] = [];
  let caps = new Set<string>();
  for (let offset = 0; offset + 4 <= body.length;) {
    const header = body.toString('ascii', offset, offset + 4);
    if (!/^[a-f\d]{4}$/i.test(header)) break;
    const length = Number.parseInt(header, 16);
    if (length === 0) return { commands, caps, pack: body.subarray(offset + 4) };
    if (length < 4 || length > 65520 || offset + length > body.length) break;
    const line = body.toString('utf8', offset + 4, offset + length).replace(/\n$/, '');
    const sep = line.indexOf('\0');
    if (sep !== -1) {
      if (commands.length !== 0) throw new Fault(400, 'Unexpected receive-pack capabilities');
      caps = new Set(line.slice(sep + 1).split(' '));
    }
    const fields = (sep === -1 ? line : line.slice(0, sep)).split(' ');
    if (fields.length !== 3 || !isObjectId(fields[0]!) || !isObjectId(fields[1]!))
      throw new Fault(400, 'Malformed receive-pack command');
    commands.push({ old: fields[0]!.toLowerCase(), next: fields[1]!.toLowerCase(), ref: fields[2]! });
    offset += length;
  }
  throw new Fault(400, 'Missing receive-pack command terminator');
}

// The walk only expands objects introduced by this push. An existing object was already
// checked when its ref was written, so a small push on deep history stays small.
function incomingClosure(root: string, repo: Repository, incoming: Map<string, GitObject>): Set<string> {
  const introduced = new Set<string>();
  const stack: Array<[string, GitObject['type']?]> = [[root]];
  while (stack.length) {
    const [oid, expected] = stack.pop()!;
    const known = repo.objects.get(oid);
    const object = known ?? incoming.get(oid);
    if (!object || (expected && object.type !== expected)) throw new Error('missing or wrong-type object');
    if (known || introduced.has(oid)) continue;
    introduced.add(oid);
    if (object.type === 'commit') {
      const commit = parseCommit(object.data);
      stack.push([commit.tree, 'tree']);
      for (const parent of commit.parents) stack.push([parent, 'commit']);
    } else if (object.type === 'tree') {
      for (const entry of parseTree(object.data)) {
        if (entry.mode === '160000') continue;
        stack.push([entry.oid, directory(entry.mode) ? 'tree' : 'blob']);
      }
    } else if (object.type === 'tag') {
      const tag = parseTag(object.data);
      stack.push([tag.object, tag.type]);
    }
  }
  return introduced;
}
function directory(mode: string): boolean { return mode === '40000' || mode === '040000'; }

export function receivePack(repo: Repository, body: Buffer): Buffer {
  const { commands, caps, pack } = parseUpdates(body);
  if (!commands.length) return EMPTY;
  let incoming = new Map<string, GitObject>();
  let unpack = 'ok';
  if (pack.length) {
    try {
      const result = readPack(pack, { resolveBase: oid => repo.objects.get(oid) });
      if (!result.checksumOk) throw new Error('Pack checksum mismatch');
      incoming = result.objects;
    } catch {
      unpack = 'invalid pack';
    }
  } else if (commands.some(command => command.next !== ZERO_OID)) {
    unpack = 'missing pack';
  }
  const reports = [encodePktLine(`unpack ${unpack}\n`)];
  const seen = new Set<string>();
  for (const { old, next, ref } of commands) {
    let error = '';
    if (unpack !== 'ok') error = 'unpacker error';
    else if (!refNameValid(ref) || seen.has(ref)) error = 'invalid ref';
    else if (old !== (repo.refs.get(ref) ?? ZERO_OID)) error = 'non-fast-forward';
    else if (next === ZERO_OID) repo.setRef(ref);
    else if (repo.refCollision(ref)) error = 'ref name conflict';
    else {
      const type = (repo.objects.get(next) ?? incoming.get(next))?.type;
      try {
        if (!type || (ref.startsWith('refs/heads/') && type !== 'commit')) throw new Error('invalid target');
        const added = incomingClosure(next, repo, incoming);
        for (const oid of added) repo.objects.set(oid, incoming.get(oid)!);
        if (old !== ZERO_OID && old !== next && (ref.startsWith('refs/tags/') || !repo.ancestor(old, next)))
          error = 'non-fast-forward';
        if (!error) repo.setRef(ref, next);
        else for (const oid of added) repo.objects.delete(oid);
      } catch {
        error = 'missing or invalid object';
      }
    }
    seen.add(ref);
    reports.push(encodePktLine(error ? `ng ${ref.replace(/[\r\n]/g, '?')} ${error}\n` : `ok ${ref}\n`));
  }
  if (!caps.has('report-status') && !caps.has('report-status-v2')) return caps.has('side-band-64k') ? FLUSH : EMPTY;
  const report = Buffer.concat([...reports, FLUSH]);
  return caps.has('side-band-64k') ? Buffer.concat([...sideband(report), FLUSH]) : report;
}

function lsRefs(repo: Repository, args: string[]): Buffer {
  const prefixes = args.filter(line => line.startsWith('ref-prefix ')).map(line => line.slice(11));
  const include = (ref: string) => !prefixes.length || prefixes.some(prefix => ref.startsWith(prefix));
  const peel = args.includes('peel');
  const symref = args.includes('symrefs') ? ` symref-target:${repo.headRef}` : '';
  const lines: Buffer[] = [];
  if (include('HEAD')) {
    if (repo.tip) lines.push(encodePktLine(`${repo.tip} HEAD${symref}\n`));
    else if (args.includes('unborn')) lines.push(encodePktLine(`unborn HEAD${symref}\n`));
  }
  for (const [ref, oid] of sortedRefs(repo)) {
    if (!include(ref)) continue;
    const target = peel && ref.startsWith('refs/tags/') ? repo.peel(oid) : oid;
    lines.push(encodePktLine(`${oid} ${ref}${target !== oid ? ' peeled:' + target : ''}\n`));
  }
  return Buffer.concat([...lines, FLUSH]);
}

type Fetch = { wants: string[]; haves: string[]; shallow: Set<string>; depth: number; since: number; omit: string[];
  relative: boolean; done: boolean; features: Set<string> };
function parseFetch(lines: string[]): Fetch {
  const request: Fetch = { wants: [], haves: [], shallow: new Set(), depth: 0, since: 0,
    omit: [], relative: false, done: false, features: new Set() };
  for (const line of lines) {
    const [cmd, arg, ...more] = line.split(' ');
    if (cmd === 'want' || cmd === 'have' || cmd === 'shallow') {
      if (!arg || !isObjectId(arg)) throw new Fault(400, 'Invalid fetch object ID');
      const oid = arg.toLowerCase();
      if (cmd === 'want') {
        request.wants.push(oid);
        for (const feature of more) request.features.add(feature);
      } else if (cmd === 'have') request.haves.push(oid);
      else request.shallow.add(oid);
    } else if (cmd === 'done') request.done = true;
    else if (cmd === 'deepen' || cmd === 'deepen-since') {
      const value = Number(arg);
      if (!Number.isSafeInteger(value) || value < 1) throw new Fault(400, 'Invalid depth');
      if (cmd === 'deepen') request.depth = value;
      else request.since = value;
    } else if (cmd === 'deepen-not' && arg) request.omit.push(arg);
    else if (cmd === 'deepen-relative') request.relative = true;
    else if (cmd) request.features.add(cmd);
  }
  request.wants = [...new Set(request.wants)];
  request.haves = [...new Set(request.haves)];
  return request;
}

type Selection = { objects: Set<string>; shallow: Set<string> };
function select(repo: Repository, roots: string[], request: Fetch, clientObjects: boolean): Selection {
  const objects = new Set<string>();
  const shallow = new Set<string>();
  const includedParents = new Set<string>();
  const depthSeen = new Map<string, number>();
  const stack = roots.map(oid => ({ oid, depth: 1, relative: 0 }));
  const expand = !clientObjects && (request.depth > 0 || request.since > 0 || request.omit.length > 0);
  const forbidden = new Set<string>();
  if (expand) for (const name of request.omit) for (const oid of repo.history(repo.resolve(name))) forbidden.add(oid);
  while (stack.length) {
    const { oid, depth, relative } = stack.pop()!;
    const object = repo.objects.get(oid);
    if (!object) continue;
    if (object.type === 'commit') {
      const distance = expand && request.relative ? relative : depth;
      if ((depthSeen.get(oid) ?? Infinity) <= distance) continue;
      depthSeen.set(oid, distance);
      objects.add(oid);
      const commit = repo.commit(oid);
      stack.push({ oid: commit.tree, depth, relative });
      let nextRelative = relative;
      if (request.shallow.has(oid) && request.relative && relative === 0) nextRelative = 1;
      const atBoundary = (!expand && request.shallow.has(oid)) ||
        (expand && request.depth > 0 && (request.relative ? nextRelative > request.depth : depth >= request.depth));
      let missingParent = false;
      for (const parent of commit.parents) {
        const parentCommit = request.since > 0 && expand ? repo.commit(parent) : undefined;
        const epoch = parentCommit && Number(parentCommit.committer.split(' ').at(-2));
        if (atBoundary || (expand && (forbidden.has(parent) || (epoch !== undefined && epoch < request.since)))) {
          missingParent = true;
        } else {
          stack.push({ oid: parent, depth: depth + 1, relative: nextRelative ? nextRelative + 1 : 0 });
        }
      }
      if (missingParent) shallow.add(oid);
      else includedParents.add(oid);
    } else if (!objects.has(oid)) {
      objects.add(oid);
      if (object.type === 'tree') {
        for (const entry of repo.tree(oid)) if (entry.mode !== '160000')
          stack.push({ oid: entry.oid, depth, relative });
      } else if (object.type === 'tag') {
        const target = repo.tagTarget(oid);
        if (target) stack.push({ oid: target, depth, relative });
      }
    }
  }
  for (const oid of includedParents) shallow.delete(oid);
  return { objects, shallow };
}

const PACK_CODE = { commit: 1, tree: 2, blob: 3, tag: 4 };
function objectHeader(type: number, size: number): Buffer {
  const bytes = [(type << 4) | (size & 15)];
  size = Math.floor(size / 16);
  while (size) {
    bytes[bytes.length - 1]! |= 128;
    bytes.push(size & 127);
    size = Math.floor(size / 128);
  }
  return Buffer.from(bytes);
}
function leb128(bytes: number[], value: number) {
  while (value >= 128) { bytes.push((value & 127) | 128); value >>>= 7; }
  bytes.push(value);
}
function distanceHeader(distance: number): Buffer {
  const out = [distance & 127];
  while ((distance = Math.floor(distance / 128)) > 0) out.push(128 | (--distance & 127));
  return Buffer.from(out.reverse());
}
function makeDelta(base: Buffer, next: Buffer): Buffer | undefined {
  if (base.length < 256 || next.length < 256 || base.length > 1024 * 1024 || next.length > 1024 * 1024) return;
  let first = 0;
  let tail = 0;
  const limit = Math.min(base.length, next.length);
  while (first < limit && base[first] === next[first]) first++;
  while (tail < limit - first && base[base.length - 1 - tail] === next[next.length - 1 - tail]) tail++;
  if (first + tail < next.length * 0.6) return;
  const result: number[] = [];
  leb128(result, base.length);
  leb128(result, next.length);
  const copy = (offset: number, size: number) => {
    while (size) {
      const take = Math.min(size, 0xffffff);
      const values: number[] = [];
      let opcode = 128;
      for (let i = 0; i < 7; i++) {
        const byte = i < 4 ? (offset >>> (i * 8)) & 255 : (take >>> ((i - 4) * 8)) & 255;
        if (byte) { opcode |= 1 << i; values.push(byte); }
      }
      result.push(opcode, ...values);
      offset += take;
      size -= take;
    }
  };
  copy(0, first);
  for (let offset = first; offset < next.length - tail;) {
    const end = Math.min(offset + 127, next.length - tail);
    result.push(end - offset, ...next.subarray(offset, end));
    offset = end;
  }
  copy(base.length - tail, tail);
  return Buffer.from(result);
}

function assemblePack(objects: GitObject[], deltas: boolean): Buffer {
  const pieces: Buffer[] = [Buffer.alloc(12)];
  const sum = createHash('sha1');
  pieces[0]!.write('PACK');
  pieces[0]!.writeUInt32BE(2, 4);
  pieces[0]!.writeUInt32BE(objects.length, 8);
  let offset = 12;
  let prior: { offset: number; data: Buffer; depth: number } | undefined;
  for (const object of objects) {
    const start = offset;
    const raw = object.data;
    let data = deflateSync(raw, { level: 1 });
    let header = objectHeader(PACK_CODE[object.type], raw.length);
    let depth = 0;
    if (deltas && object.type === 'blob' && prior && prior.depth < 8) {
      const delta = makeDelta(prior.data, raw);
      if (delta) {
        const packed = deflateSync(delta, { level: 1 });
        const info = Buffer.concat([objectHeader(6, delta.length), distanceHeader(start - prior.offset)]);
        if (packed.length + info.length < data.length + header.length) {
          depth = prior.depth + 1;
          data = packed;
          header = info;
        }
      }
    }
    if (object.type === 'blob') prior = { offset: start, data: raw, depth };
    offset += header.length + data.length;
    pieces.push(header, data);
  }
  for (const part of pieces) sum.update(part);
  pieces.push(sum.digest());
  return Buffer.concat(pieces);
}
const emptyPack = assemblePack([], false);

function fetchPack(repo: Repository, request: Fetch, v2: boolean): Buffer {
  for (const oid of request.wants) {
    if (!repo.objects.has(oid)) return Buffer.concat([encodePktLine(`ERR not our ref ${oid}\n`), FLUSH]);
  }
  const common = request.haves.filter(oid => repo.objects.get(oid)?.type === 'commit');
  if (v2 && !request.done) {
    return Buffer.concat([encodePktLine('acknowledgments\n'),
      ...(common.length ? common.map(oid => encodePktLine(`ACK ${oid}\n`)) : [encodePktLine('NAK\n')]), FLUSH]);
  }
  const band = v2 || request.features.has('side-band-64k');
  const hasDepth = Boolean(request.depth || request.since || request.omit.length);
  const cacheKey = request.done && !request.haves.length && !request.shallow.size && !request.since && !request.omit.length
    ? JSON.stringify([v2, band, request.depth, request.features.has('ofs-delta'), request.features.has('include-tag'), request.wants]) : '';
  const cached = cacheKey ? repo.packCache.get(cacheKey) : undefined;
  if (cached) return cached;
  const chosen = select(repo, request.wants, request, false);
  const extra: Buffer[] = [];
  if (hasDepth) {
    for (const oid of chosen.shallow) if (!request.shallow.has(oid)) extra.push(encodePktLine(`shallow ${oid}\n`));
    for (const oid of request.shallow)
      if (chosen.objects.has(oid) && !chosen.shallow.has(oid)) extra.push(encodePktLine(`unshallow ${oid}\n`));
  }
  const lines: Buffer[] = [];
  if (v2 && extra.length) lines.push(encodePktLine('shallow-info\n'), ...extra, DELIM);
  if (!v2 && hasDepth) {
    lines.push(...extra, FLUSH);
    if (!request.done && !request.haves.length) return Buffer.concat(lines);
  }
  if (!request.done) return Buffer.concat([...lines, encodePktLine('NAK\n')]);
  const held = common.length ? select(repo, common, request, true).objects : new Set<string>();
  if (request.features.has('include-tag'))
    for (const [ref, oid] of repo.refs) if (ref.startsWith('refs/tags/') && chosen.objects.has(repo.peel(oid))
      && !held.has(repo.peel(oid))) {
      let cursor: string | undefined = oid;
      while (cursor !== undefined && repo.objects.get(cursor)?.type === 'tag' && !chosen.objects.has(cursor)) {
        chosen.objects.add(cursor);
        cursor = repo.tagTarget(cursor);
      }
    }
  const objects: GitObject[] = [];
  for (const oid of chosen.objects) if (!held.has(oid)) objects.push(repo.objects.get(oid)!);
  const pack = objects.length ? assemblePack(objects, request.features.has('ofs-delta')) : emptyPack;
  if (v2) lines.push(encodePktLine('packfile\n'));
  else lines.push(encodePktLine(common.length ? `ACK ${common.at(-1)}\n` : 'NAK\n'));
  if (band) lines.push(...sideband(pack), FLUSH);
  else lines.push(pack);
  const response = Buffer.concat(lines);
  if (cacheKey) repo.cachePack(cacheKey, response);
  return response;
}

export function uploadPack(repo: Repository, body: Buffer, v2: boolean): Buffer {
  const packets = decodePktLines(body);
  const lines = packets.flatMap(packet => packet.kind === 'data'
    ? [packet.payload.toString('utf8').replace(/\n$/, '')] : []);
  if (!lines.length) return FLUSH;
  if (v2) {
    const command = lines[0];
    const separator = packets.findIndex(packet => packet.kind === 'delim');
    if (separator === -1) throw new Fault(400, 'Missing protocol v2 argument delimiter');
    const args = packets.slice(separator + 1).flatMap(packet => packet.kind === 'data'
      ? [packet.payload.toString('utf8').replace(/\n$/, '')] : []);
    if (command === 'command=ls-refs') return lsRefs(repo, args);
    if (command === 'command=fetch') return fetchPack(repo, parseFetch(args), true);
    throw new Fault(400, 'Unsupported upload-pack command');
  }
  return fetchPack(repo, parseFetch(lines), false);
}
