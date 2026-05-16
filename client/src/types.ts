// Wire shapes — these MUST match google-photos-picker's HTTP handlers
// (handlers.go) and OAuth callback page (callback.go) for v0.3.0.

/** Response of the Status handler. */
export interface GoogleStatus {
  connected: boolean;
  scopes: string[] | null;
}

/** Response of the CreateSession handler. */
export interface CreateSessionResponse {
  sessionId: string;
  pickerUri: string;
}

/** Response of the PollSession handler. `expired` is new in v0.3.0. */
export interface SessionStatus {
  status: 'pending' | 'ready' | 'expired';
}

/** Response of the StartImport handler. */
export interface StartImportResponse {
  importJobId: string;
}

export type ImportJobStatus = 'pending' | 'running' | 'complete' | 'failed';

/** Response of the GetImport handler. */
export interface ImportJob {
  id: string;
  status: ImportJobStatus;
  total: number;
  completed: number;
  failed: number;
  savedIds: string[];
  error?: string;
}

/** Lifecycle phase of the end-to-end flow. */
export type FlowPhase =
  | 'idle'
  | 'connecting' // OAuth popup open
  | 'creating' // creating the picker session
  | 'picking' // picker open, polling the session
  | 'importing' // import job running, polling progress
  | 'done' // terminal success
  | 'error'; // terminal failure (incl. session expiry)

export interface ImportProgress {
  total: number;
  completed: number;
  failed: number;
}

export interface CompleteResult extends ImportProgress {
  savedIds: string[];
}

/**
 * Reactive flow state. Adapters expose this; UIs render from it.
 * `connected` is null until the first status check resolves. `expired` is true
 * only when the terminal error was a lapsed picker session (UI can offer a
 * one-click retry rather than a generic error).
 */
export interface FlowState {
  phase: FlowPhase;
  connected: boolean | null;
  progress: ImportProgress | null;
  result: CompleteResult | null;
  error: string | null;
  expired: boolean;
}

/**
 * Endpoint URLs. Paths are the consumer's choice,
 * so every endpoint is supplied explicitly. See `defaultEndpoints`.
 */
export interface Endpoints {
  status: string;
  connect: string;
  disconnect: string;
  createSession: string;
  pollSession: (sessionId: string) => string;
  startImport: (sessionId: string) => string;
  getImport: (jobId: string) => string;
}

export interface StartOptions {
  /**
   * Opaque metadata forwarded to the lib's StartImport body
   * (`{"metadata": …}`). Only for apps using the client-supplied path; apps
   * that derive the destination server-side leave this unset.
   */
  metadata?: Record<string, string>;
}

export interface FlowConfig {
  endpoints: Endpoints;
  /**
   * Performs an HTTP request and resolves parsed JSON. The consumer injects
   * auth (Authorization header) and base URL here, and MUST throw on a
   * non-2xx response. `void` responses (e.g. disconnect 204) may resolve
   * undefined.
   */
  fetchJson: <T>(url: string, init?: RequestInit) => Promise<T>;
  /** Must equal the Go-side CallbackPage.PostMessageType. */
  postMessageType: string;
  /**
   * If set, inbound OAuth messages whose `event.origin` differs are ignored
   * (defence-in-depth; this is the API origin that served the callback page,
   * not the app origin). Unset = no origin check, matching the Go side's own
   * targetOrigin restriction.
   */
  expectedOrigin?: string;
  /** Poll cadence; defaults: session 2000ms, job 1500ms. */
  pollIntervalMs?: { session?: number; job?: number };
  /** Test seam. Defaults to window.open. */
  openWindow?: (url: string, name: string) => Window | null;
  /** Test seam. Defaults to window. */
  messageTarget?: Pick<Window, 'addEventListener' | 'removeEventListener'>;
}
