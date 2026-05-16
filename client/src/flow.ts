import type {
  CompleteResult,
  CreateSessionResponse,
  FlowConfig,
  FlowState,
  GoogleStatus,
  ImportJob,
  SessionStatus,
  StartImportResponse,
  StartOptions,
} from './types';

/** Thrown internally when a run is superseded or cancelled. Not surfaced as a
 *  flow error — the state simply returns to idle. */
export class FlowCancelled extends Error {
  constructor() {
    super('google-photos flow cancelled');
    this.name = 'FlowCancelled';
  }
}

const DEFAULT_SESSION_POLL_MS = 2000;
const DEFAULT_JOB_POLL_MS = 1500;

const INITIAL: FlowState = {
  phase: 'idle',
  connected: null,
  progress: null,
  result: null,
  error: null,
  expired: false,
};

/**
 * GooglePhotosFlow drives the entire import flow framework-agnostically:
 * status, popup-safe OAuth, picker session, both poll loops, and an
 * exactly-once completion. It owns its timers and emits state changes to
 * subscribers; framework adapters are thin wrappers over `subscribe` + the
 * action methods.
 *
 * Popup-blocker safety: `connect()` and `start()` each open exactly one window
 * synchronously, before any `await` and before the first `setState()` (only a
 * cheap synchronous precheck may precede it), so they must be called from a
 * user-gesture handler. They never open a second window after an await (the
 * fragile pattern). If the user isn't connected, the UI flow is two
 * gestures: click → connect(), then click → start().
 */
export class GooglePhotosFlow {
  private readonly config: FlowConfig;
  private _state: FlowState = INITIAL;
  private readonly listeners = new Set<(s: FlowState) => void>();
  private readonly waiters = new Set<{ timer: ReturnType<typeof setTimeout>; reject: (e: unknown) => void }>();
  private popup: Window | null = null;
  /** Bumped by cancel() and by each new run; stale loops detect this and bail. */
  private runId = 0;

  constructor(config: FlowConfig) {
    this.config = config;
  }

  get state(): FlowState {
    return this._state;
  }

  /** Register a state listener. Does not emit the current value (framework
   *  adapters surface the initial value themselves). Returns an unsubscribe. */
  subscribe(fn: (s: FlowState) => void): () => void {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  private setState(patch: Partial<FlowState>): void {
    this._state = { ...this._state, ...patch };
    for (const fn of this.listeners) fn(this._state);
  }

  /** Abort any in-flight run, close popups, reset to idle (unless terminal). */
  cancel(): void {
    this.runId++;
    this.clearTimers();
    this.closePopup();
    if (this._state.phase !== 'done' && this._state.phase !== 'error') {
      this.setState({ phase: 'idle', progress: null });
    }
  }

  private clearTimers(): void {
    for (const w of this.waiters) {
      clearTimeout(w.timer);
      w.reject(new FlowCancelled());
    }
    this.waiters.clear();
  }

  private closePopup(): void {
    const p = this.popup;
    this.popup = null;
    if (p && !p.closed) {
      try {
        p.close();
      } catch {
        /* cross-origin window may refuse close() — best effort */
      }
    }
  }

  /** Fetch connection status. Safe to call on mount (not a user gesture). */
  async refreshStatus(): Promise<void> {
    try {
      const s = await this.config.fetchJson<GoogleStatus>(this.config.endpoints.status);
      this.setState({ connected: !!s.connected });
    } catch (e) {
      this.setState({ connected: false, error: messageOf(e) });
    }
  }

  /** Revoke the Google connection. */
  async disconnect(): Promise<void> {
    await this.config.fetchJson<void>(this.config.endpoints.disconnect, { method: 'DELETE' });
    this.setState({ connected: false });
  }

  /**
   * Run the OAuth dance. MUST be called from a user-gesture handler: a blank
   * popup is opened synchronously, then pointed at the consent URL once the
   * backend returns it, then resolved/rejected from the callback postMessage.
   */
  async connect(): Promise<void> {
    const win = this.openBlank('gpp-oauth');
    const myRun = ++this.runId;
    this.setState({ phase: 'connecting', error: null, expired: false });
    try {
      if (!win) throw new Error('Popup blocked — allow popups and retry.');
      this.popup = win;
      const { consentUrl } = await this.config.fetchJson<{ consentUrl: string }>(
        this.config.endpoints.connect,
      );
      this.ensureCurrent(myRun);
      if (win.closed) throw new Error('Authorisation window was closed.');
      win.location.href = consentUrl;
      await this.waitForOAuth(win, myRun);
      this.closePopup();
      this.setState({ phase: 'idle', connected: true });
    } catch (e) {
      this.closePopup();
      this.handleError(e, myRun);
      throw e;
    }
  }

  /**
   * Run create-session → pick → import to completion. MUST be called from a
   * user-gesture handler and only when `state.connected` is true (the UI
   * gates this; if called disconnected it errors fast). Resolves exactly once
   * with the final result; the same result is also placed on `state.result`.
   */
  async start(opts: StartOptions = {}): Promise<CompleteResult> {
    const myRun = ++this.runId;
    try {
      if (this._state.connected === false) {
        throw new Error('Not connected to Google Photos — connect first.');
      }
      const win = this.openBlank('gpp-picker');
      this.setState({
        phase: 'creating',
        error: null,
        expired: false,
        result: null,
        progress: null,
      });
      if (!win) throw new Error('Popup blocked — allow popups and retry.');
      this.popup = win;

      const session = await this.config.fetchJson<CreateSessionResponse>(
        this.config.endpoints.createSession,
        { method: 'POST' },
      );
      this.ensureCurrent(myRun);
      if (win.closed) throw new Error('Picker window was closed.');
      win.location.href = session.pickerUri;

      this.setState({ phase: 'picking' });
      await this.pollSession(session.sessionId, myRun);
      // User has confirmed a selection — the picker tab is done with.
      this.closePopup();

      this.setState({ phase: 'importing' });
      const { importJobId } = await this.config.fetchJson<StartImportResponse>(
        this.config.endpoints.startImport(session.sessionId),
        opts.metadata
          ? {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ metadata: opts.metadata }),
            }
          : { method: 'POST' },
      );
      this.ensureCurrent(myRun);

      const result = await this.pollJob(importJobId, myRun);
      this.setState({ phase: 'done', result });
      return result;
    } catch (e) {
      this.closePopup();
      this.handleError(e, myRun);
      throw e;
    }
  }

  // ─── internals ────────────────────────────────────────────────────────────

  private openBlank(name: string): Window | null {
    const open =
      this.config.openWindow ??
      ((u: string, n: string) => window.open(u, n));
    return open('about:blank', name);
  }

  private ensureCurrent(myRun: number): void {
    if (this.runId !== myRun) throw new FlowCancelled();
  }

  // Resolves after `ms`, unless the run is superseded/cancelled first - then it
  // rejects with FlowCancelled. Registered in `waiters` so clearTimers() can
  // interrupt an in-flight wait, which makes it abortable on demand.
  private cancellableDelay(ms: number, myRun: number): Promise<void> {
    return new Promise((resolve, reject) => {
      const w = {
        timer: setTimeout(() => {
          this.waiters.delete(w);
          this.runId === myRun ? resolve() : reject(new FlowCancelled());
        }, ms),
        reject,
      };
      this.waiters.add(w);
    });
  }

  private async pollSession(sessionId: string, myRun: number): Promise<void> {
    const interval = this.config.pollIntervalMs?.session ?? DEFAULT_SESSION_POLL_MS;
    for (;;) {
      const s = await this.config.fetchJson<SessionStatus>(
        this.config.endpoints.pollSession(sessionId),
      );
      this.ensureCurrent(myRun);
      if (s.status === 'ready') return;
      if (s.status === 'expired') {
        const err = new Error('The Google Photos session expired before you confirmed a selection.');
        (err as ExpiredError).expired = true;
        throw err;
      }
      await this.cancellableDelay(interval, myRun);
    }
  }

  private async pollJob(jobId: string, myRun: number): Promise<CompleteResult> {
    const interval = this.config.pollIntervalMs?.job ?? DEFAULT_JOB_POLL_MS;
    for (;;) {
      const job = await this.config.fetchJson<ImportJob>(this.config.endpoints.getImport(jobId));
      this.ensureCurrent(myRun);
      this.setState({
        progress: { total: job.total, completed: job.completed, failed: job.failed },
      });
      if (job.status === 'complete') {
        return {
          savedIds: job.savedIds ?? [],
          total: job.total,
          completed: job.completed,
          failed: job.failed,
        };
      }
      if (job.status === 'failed') {
        throw new Error(job.error || 'Import failed.');
      }
      await this.cancellableDelay(interval, myRun);
    }
  }

  // Resolves once the Go callback page (loaded in `popup` after Google's
  // redirect) posts `{ type, status, message }` back via window.opener. Settles
  // exactly once — via the matching postMessage (success → resolve, error →
  // reject with its message), the user manually closing the popup (no event
  // fires for that, hence the polling watchdog), or the run being superseded
  // (→ FlowCancelled). `finish()` tears down the listener + watchdog on
  // whichever happens first. Non-matching messages (wrong type, or wrong
  // origin when `expectedOrigin` is set) are ignored, not rejected — other
  // postMessages on the page must pass through untouched.
  private waitForOAuth(popup: Window, myRun: number): Promise<void> {
    const target = this.config.messageTarget ?? globalThis;
    const expectedOrigin = this.config.expectedOrigin;
    const type = this.config.postMessageType;
    return new Promise<void>((resolve, reject) => {
      let settled = false;
      const finish = () => {
        if (settled) return;
        settled = true;
        clearInterval(watchdog);
        target.removeEventListener('message', onMessage as EventListener);
      };
      const onMessage = (e: MessageEvent) => {
        if (this.runId !== myRun) {
          finish();
          reject(new FlowCancelled());
          return;
        }
        if (expectedOrigin !== undefined && e.origin !== expectedOrigin) return;
        const d = e.data as { type?: unknown; status?: unknown; message?: unknown } | null;
        if (!d || typeof d !== 'object' || d.type !== type) return;
        finish();
        if (d.status === 'success') resolve();
        else reject(new Error(typeof d.message === 'string' && d.message ? d.message : 'Google authorisation failed.'));
      };
      // Detect the user manually closing the consent window.
      const watchdog = setInterval(() => {
        if (this.runId !== myRun) {
          finish();
          reject(new FlowCancelled());
        } else if (popup.closed) {
          finish();
          reject(new Error('Authorisation window was closed.'));
        }
      }, 500);
      target.addEventListener('message', onMessage as EventListener);
    });
  }

  private handleError(e: unknown, myRun: number): void {
    if (e instanceof FlowCancelled || this.runId !== myRun) {
      // Superseded/cancelled: leave whatever the newer run set.
      return;
    }
    this.setState({
      phase: 'error',
      error: messageOf(e),
      expired: isExpired(e),
    });
  }
}

interface ExpiredError extends Error {
  expired?: boolean;
}

function isExpired(e: unknown): boolean {
  return !!(e && typeof e === 'object' && (e as ExpiredError).expired === true);
}

function messageOf(e: unknown): string {
  if (e instanceof Error) return e.message;
  if (typeof e === 'string') return e;
  return 'Something went wrong.';
}
