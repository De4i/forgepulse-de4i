import { createForgeServer } from './forge-core.ts';

const SERVICE_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
const REQUEST_TIMEOUT_MS = 30_000;
const INITIAL_RETRY_MS = 250;
const MAX_RETRY_MS = 2_000;
const USER_AGENT = 'forgepulse-agent/2';

if (SERVICE_URL.length === 0) {
  process.stderr.write('[forgepulse] MAKEFUN_ENV_URL is required\n');
  process.exit(1);
}

function pause(milliseconds: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}

function backoff(failures: number): number {
  const exponent = Math.min(failures, 8);
  return Math.min(MAX_RETRY_MS, INITIAL_RETRY_MS * 2 ** exponent);
}

async function serviceRequest(path: string, init: RequestInit = {}): Promise<Response> {
  return fetch(`${SERVICE_URL}${path}`, {
    ...init,
    headers: { 'user-agent': USER_AGENT, ...init.headers },
    signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
  });
}

async function announcePort(port: number): Promise<void> {
  for (let failures = 0; ; failures += 1) {
    try {
      const response = await serviceRequest('/register', {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
      });
      if (response.status === 200 || response.status === 409) return;
      if ([400, 401, 403, 405].includes(response.status)) {
        throw new Error(`port registration rejected with HTTP ${response.status}`);
      }
    } catch (error) {
      if (error instanceof Error && error.message.startsWith('port registration rejected')) throw error;
    }
    await pause(backoff(failures));
  }
}

async function pollForCompletion(): Promise<void> {
  for (let failures = 0; ; failures += 1) {
    try {
      const response = await serviceRequest('/done');
      if (response.ok) {
        const result = await response.json() as { done?: unknown };
        if (result.done === true) return;
      }
    } catch {
      // Retry: the trial service can restart between a timeout and its next health window.
    }
    await pause(backoff(failures));
  }
}

async function serveTrial(): Promise<void> {
  const server = createForgeServer();
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === 'string') {
    throw new Error('forge did not bind a TCP port');
  }

  process.stdout.write(`[forgepulse] bound ephemeral port ${address.port}\n`);
  try {
    await announcePort(address.port);
    process.stdout.write('[forgepulse] environment registration accepted\n');
    await pollForCompletion();
    process.stdout.write('[forgepulse] environment completed the trial\n');
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
}

try {
  await serveTrial();
  process.exit(0);
} catch (error) {
  const message = error instanceof Error ? error.message : String(error);
  process.stderr.write(`[forgepulse] fatal: ${message}\n`);
  process.exit(1);
}
