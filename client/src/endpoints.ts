import type { Endpoints } from './types';

/**
 * Conventional endpoint layout for the common case: all routes under a single
 * base path. Consumers whose routes diverge (e.g. status/connect under a
 * different prefix) build the `Endpoints` object by hand instead.
 *
 *   defaultEndpoints('/v1/google-photos')
 *     → POST   /v1/google-photos/sessions
 *       GET    /v1/google-photos/sessions/:id
 *       POST   /v1/google-photos/sessions/:id/import
 *       GET    /v1/google-photos/imports/:id
 *       GET    /v1/google-photos/status
 *       GET    /v1/google-photos/connect
 *       DELETE /v1/google-photos/disconnect
 */
export function defaultEndpoints(base: string): Endpoints {
  const b = base.replace(/\/$/, '');
  const enc = encodeURIComponent;
  return {
    status: `${b}/status`,
    connect: `${b}/connect`,
    disconnect: `${b}/disconnect`,
    createSession: `${b}/sessions`,
    pollSession: (id) => `${b}/sessions/${enc(id)}`,
    startImport: (id) => `${b}/sessions/${enc(id)}/import`,
    getImport: (id) => `${b}/imports/${enc(id)}`,
  };
}
