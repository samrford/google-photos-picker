import { describe, expect, it, vi } from 'vitest';
import { FlowCancelled, GooglePhotosFlow } from './flow';
import type { FlowConfig } from './types';

function fakeWindow() {
  return {
    closed: false,
    location: { href: '' },
    close() {
      (this as { closed: boolean }).closed = true;
    },
  };
}

function makeBus() {
  const set = new Set<(e: unknown) => void>();
  return {
    addEventListener: (_t: string, fn: (e: unknown) => void) => set.add(fn),
    removeEventListener: (_t: string, fn: (e: unknown) => void) => set.delete(fn),
    emit(data: unknown, origin = 'https://api.example') {
      for (const fn of [...set]) fn({ data, origin });
    },
    size: () => set.size,
  };
}

type FetchJson = FlowConfig['fetchJson'];

function setup(fetchJson: FetchJson, over: Partial<FlowConfig> = {}) {
  const bus = makeBus();
  const win = fakeWindow();
  const cfg: FlowConfig = {
    endpoints: {
      status: '/status',
      connect: '/connect',
      disconnect: '/disconnect',
      createSession: '/sessions',
      pollSession: (id) => `/sessions/${id}`,
      startImport: (id) => `/sessions/${id}/import`,
      getImport: (id) => `/imports/${id}`,
    },
    postMessageType: 'test:oauth',
    pollIntervalMs: { session: 1, job: 1 },
    openWindow: () => win as unknown as Window,
    messageTarget: bus as unknown as Window,
    fetchJson,
    ...over,
  };
  return { flow: new GooglePhotosFlow(cfg), bus, win };
}

describe('connect()', () => {
  it('opens a popup synchronously, navigates to consent, resolves on success', async () => {
    const { flow, bus, win } = setup(async (url) => {
      if (url === '/connect') return { consentUrl: 'https://accounts.google/x' } as never;
      throw new Error('unexpected ' + url);
    });
    const p = flow.connect();
    expect(flow.state.phase).toBe('connecting'); // set before any await

    await vi.waitFor(() => expect(win.location.href).toBe('https://accounts.google/x'));
    bus.emit({ type: 'test:oauth', status: 'success' });

    await p;
    expect(flow.state.connected).toBe(true);
    expect(flow.state.phase).toBe('idle');
    expect(win.closed).toBe(true);
    expect(bus.size()).toBe(0); // listener + watchdog torn down
  });

  it('rejects with the callback message on error', async () => {
    const { flow, bus, win } = setup(async () => ({ consentUrl: 'u' }) as never);
    const p = flow.connect();
    await vi.waitFor(() => expect(win.location.href).toBe('u'));
    bus.emit({ type: 'test:oauth', status: 'error', message: 'access denied' });
    await expect(p).rejects.toThrow('access denied');
    expect(flow.state.phase).toBe('error');
  });

  it('errors when the popup is blocked', async () => {
    const { flow } = setup(async () => ({}) as never, { openWindow: () => null });
    await expect(flow.connect()).rejects.toThrow(/popup blocked/i);
    expect(flow.state.phase).toBe('error');
  });
});

describe('start()', () => {
  it('runs create→pick→import, resolves exactly once, forwards metadata', async () => {
    let sessionPolls = 0;
    let jobPolls = 0;
    const bodies: string[] = [];
    const { flow, win } = setup(async (url, init) => {
      if (url === '/sessions' && init?.method === 'POST') {
        return { sessionId: 's1', pickerUri: 'https://picker' } as never;
      }
      if (url === '/sessions/s1') {
        sessionPolls++;
        return { status: sessionPolls < 2 ? 'pending' : 'ready' } as never;
      }
      if (url === '/sessions/s1/import') {
        bodies.push(String(init?.body));
        return { importJobId: 'j1' } as never;
      }
      if (url === '/imports/j1') {
        jobPolls++;
        return (
          jobPolls < 2
            ? { id: 'j1', status: 'running', total: 3, completed: 1, failed: 0, savedIds: [] }
            : { id: 'j1', status: 'complete', total: 3, completed: 3, failed: 0, savedIds: ['a', 'b', 'c'] }
        ) as never;
      }
      throw new Error('unmapped ' + url);
    });

    const onResolve = vi.fn();
    const result = await flow.start({ metadata: { item_id: 'it-1' } }).then((r) => {
      onResolve(r);
      return r;
    });

    expect(result.savedIds).toEqual(['a', 'b', 'c']);
    expect(flow.state.phase).toBe('done');
    expect(flow.state.result?.savedIds).toEqual(['a', 'b', 'c']);
    expect(flow.state.progress).toEqual({ total: 3, completed: 3, failed: 0 });
    expect(win.location.href).toBe('https://picker');
    expect(JSON.parse(bodies[0]!)).toEqual({ metadata: { item_id: 'it-1' } });
    expect(onResolve).toHaveBeenCalledTimes(1);
  });

  it('surfaces an expired session as a flagged error', async () => {
    const { flow } = setup(async (url, init) => {
      if (url === '/sessions' && init?.method === 'POST') {
        return { sessionId: 's', pickerUri: 'p' } as never;
      }
      if (url === '/sessions/s') return { status: 'expired' } as never;
      throw new Error('unmapped ' + url);
    });
    await expect(flow.start()).rejects.toThrow(/expired/i);
    expect(flow.state.phase).toBe('error');
    expect(flow.state.expired).toBe(true);
  });

  it('refuses fast when known-disconnected', async () => {
    const { flow } = setup(async (url) => {
      if (url === '/status') return { connected: false, scopes: null } as never;
      throw new Error('unmapped ' + url);
    });
    await flow.refreshStatus();
    expect(flow.state.connected).toBe(false);
    await expect(flow.start()).rejects.toThrow(/not connected/i);
  });

  it('cancel() aborts an in-flight run with FlowCancelled', async () => {
    let polls = 0;
    const { flow } = setup(async (url, init) => {
      if (url === '/sessions' && init?.method === 'POST') {
        return { sessionId: 's', pickerUri: 'p' } as never;
      }
      if (url === '/sessions/s') {
        polls++;
        return { status: 'pending' } as never;
      }
      throw new Error('unmapped ' + url);
    });
    const p = flow.start();
    await vi.waitFor(() => expect(polls).toBeGreaterThan(0));
    flow.cancel();
    await expect(p).rejects.toBeInstanceOf(FlowCancelled);
    expect(flow.state.phase).toBe('idle');
  });
});

describe('poll resilience', () => {
  it('tolerates a transient import-poll failure, then completes', async () => {
    let jobPolls = 0;
    const { flow } = setup(async (url, init) => {
      if (url === '/sessions' && init?.method === 'POST') return { sessionId: 's', pickerUri: 'p' } as never;
      if (url === '/sessions/s') return { status: 'ready' } as never;
      if (url === '/sessions/s/import') return { importJobId: 'j' } as never;
      if (url === '/imports/j') {
        jobPolls++;
        if (jobPolls === 1) throw new Error('transient 500');
        return { id: 'j', status: 'complete', total: 1, completed: 1, failed: 0, savedIds: ['x'] } as never;
      }
      throw new Error('unmapped ' + url);
    });
    const r = await flow.start();
    expect(r.savedIds).toEqual(['x']);
    expect(flow.state.phase).toBe('done');
    expect(jobPolls).toBe(2); // failed once, retried, succeeded
  });

  it('gives up after the consecutive-failure budget is exhausted', async () => {
    let jobPolls = 0;
    const { flow } = setup(async (url, init) => {
      if (url === '/sessions' && init?.method === 'POST') return { sessionId: 's', pickerUri: 'p' } as never;
      if (url === '/sessions/s') return { status: 'ready' } as never;
      if (url === '/sessions/s/import') return { importJobId: 'j' } as never;
      if (url === '/imports/j') {
        jobPolls++;
        throw new Error('boom');
      }
      throw new Error('unmapped ' + url);
    });
    await expect(flow.start()).rejects.toThrow('boom');
    expect(flow.state.phase).toBe('error');
    expect(jobPolls).toBe(5); // initial attempt + MAX_POLL_FAILURES (4) retries
  });
});

describe('refreshStatus()', () => {
  it('clears a prior error once status resolves', async () => {
    const { flow } = setup(
      async (url) => {
        if (url === '/status') return { connected: true, scopes: null } as never;
        throw new Error('unmapped ' + url);
      },
      { openWindow: () => null },
    );
    await expect(flow.connect()).rejects.toThrow(/popup blocked/i);
    expect(flow.state.error).toBeTruthy();
    await flow.refreshStatus();
    expect(flow.state.connected).toBe(true);
    expect(flow.state.error).toBeNull();
  });
});
