import { useEffect, useMemo, useRef, useSyncExternalStore } from 'react';
import { GooglePhotosFlow } from './flow';
import type { CompleteResult, FlowConfig, FlowState, StartOptions } from './types';

export interface UseGooglePhotosFlow {
  state: FlowState;
  connect: () => Promise<void>;
  start: (opts?: StartOptions) => Promise<CompleteResult>;
  disconnect: () => Promise<void>;
  refreshStatus: () => Promise<void>;
  cancel: () => void;
}

/**
 * React binding. The flow instance is created once and kept for the
 * component's lifetime — `config` is read only on first render, so memoise it
 * (or define it module-scope). Status is fetched on mount; the flow is
 * cancelled on unmount.
 *
 * `connect`/`start` must be called from an event handler (popup-blocker
 * safety). Gate on `state.connected`: falsey → call `connect()`; true →
 * `start()`.
 */
export function useGooglePhotosFlow(config: FlowConfig): UseGooglePhotosFlow {
  const ref = useRef<GooglePhotosFlow | null>(null);
  if (ref.current === null) ref.current = new GooglePhotosFlow(config);
  const flow = ref.current;

  const state = useSyncExternalStore(
    (cb) => flow.subscribe(cb),
    () => flow.state,
    () => flow.state,
  );

  useEffect(() => {
    void flow.refreshStatus();
    return () => flow.cancel();
  }, [flow]);

  return useMemo(
    () => ({
      state,
      connect: () => flow.connect(),
      start: (opts?: StartOptions) => flow.start(opts),
      disconnect: () => flow.disconnect(),
      refreshStatus: () => flow.refreshStatus(),
      cancel: () => flow.cancel(),
    }),
    [flow, state],
  );
}
