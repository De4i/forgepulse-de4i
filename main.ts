import { once } from 'node:events';
import { setTimeout as delay } from 'node:timers/promises';
import { serve } from './server.ts';

async function startTrial() {
  const setting = process.env.MAKEFUN_ENV_URL;
  if (!setting) throw new Error('MAKEFUN_ENV_URL is required');
  const env = new URL(setting);
  if (env.protocol !== 'http:' && env.protocol !== 'https:') throw new Error('Invalid trial environment URL');
  // The published trial tenant is independent of the opaque authentication secret.
  const owner = process.env.FORGE_OWNER ?? 'adaiversen';
  const server = serve(owner, process.env.MAKEFUN_SHOP_TOKEN ?? '',
    process.env.MAKEFUN_TRIAL_NOW);
  server.listen(0, '0.0.0.0');
  await once(server, 'listening');
  try {
    const address = server.address();
    if (!address || typeof address === 'string') throw new Error('Forge has no TCP listener');
    const register = new URL('/register', env);
    const done = new URL('/done', env);
    let registered = false;
    for (let attempt = 0; attempt < 12 && !registered; attempt++) {
      let response: Response | undefined;
      try {
        response = await fetch(register, {
          method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ port: address.port }),
          redirect: 'error', signal: AbortSignal.timeout(1200),
        });
        await response.arrayBuffer();
      } catch { /* The suite may still be starting. */ }
      if (response?.status === 200 || response?.status === 409) registered = true;
      else if (response && response.status >= 400 && response.status < 500) throw new Error('Trial registration was refused');
      if (!registered) await delay(Math.min(500, 25 * 2 ** attempt));
    }
    if (!registered) throw new Error('Could not register with the trial environment');
    let interval = 100;
    for (;;) {
      await delay(interval);
      try {
        const response = await fetch(done, { redirect: 'error', signal: AbortSignal.timeout(1500) });
        if (response.ok) {
          const body = await response.json() as { done?: unknown };
          if (body.done === true) return;
        } else await response.arrayBuffer();
      } catch { /* A failed poll does not end a live trial. */ }
      interval = Math.min(1000, interval * 2);
    }
  } finally {
    await new Promise<void>(resolve => {
      server.close(() => resolve());
      server.closeAllConnections();
    });
  }
}

try { await startTrial(); }
catch (error) {
  process.stderr.write('forgepulse: ' + (error instanceof Error ? error.message : 'Trial startup failed') + '\n');
  process.exitCode = 1;
}
