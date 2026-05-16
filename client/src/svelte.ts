import { GooglePhotosFlow } from './flow';
import type { CompleteResult, FlowConfig, FlowState, StartOptions } from './types';

export interface SvelteGooglePhotosFlow {
  /** Svelte store contract — use with `$` auto-subscription: `$flow`. */
  subscribe: (run: (s: FlowState) => void) => () => void;
  connect: () => Promise<void>;
  start: (opts?: StartOptions) => Promise<CompleteResult>;
  disconnect: () => Promise<void>;
  refreshStatus: () => Promise<void>;
  cancel: () => void;
}

/**
 * Svelte binding. Returns a readable-store-shaped object (no `svelte`
 * dependency — works in Svelte 4 and 5): `const flow = createGooglePhotosFlow(cfg)`
 * then read state as `$flow`. Call `flow.refreshStatus()` in `onMount` and
 * `flow.cancel()` in `onDestroy`.
 *
 * `connect`/`start` must be called from a DOM event handler (popup-blocker
 * safety). Gate on `$flow.connected`: falsey → `connect()`; true → `start()`.
 */
export function createGooglePhotosFlow(config: FlowConfig): SvelteGooglePhotosFlow {
  const flow = new GooglePhotosFlow(config);
  return {
    subscribe(run) {
      run(flow.state); // store contract: emit current value immediately
      return flow.subscribe(run);
    },
    connect: () => flow.connect(),
    start: (opts?: StartOptions) => flow.start(opts),
    disconnect: () => flow.disconnect(),
    refreshStatus: () => flow.refreshStatus(),
    cancel: () => flow.cancel(),
  };
}
