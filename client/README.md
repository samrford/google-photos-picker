# google-photos-picker-client

Framework-agnostic browser client for the
[`google-photos-picker`](https://github.com/samrford/google-photos-picker) Go
library's import flow: connection status, **popup-blocker-safe OAuth**, picker
session + import polling collapsed into one state machine with an
**exactly-once** completion, and `expired`-session handling. Optional React and
Svelte adapters.

```sh
bun add google-photos-picker-client      # or npm / pnpm
```

## The one rule: call `connect()` / `start()` from a click

Browsers only allow `window.open` during a user gesture. Each method opens
**one** popup synchronously before any `await`, so they must be invoked
directly from an event handler. The flow is **two gestures** when the user
isn't connected yet — gate on `state.connected`:

- falsy → `connect()` (OAuth popup)
- `true` → `start()` (picker → import)

## Core (vanilla)

```ts
import { GooglePhotosFlow, defaultEndpoints } from 'google-photos-picker-client';

const flow = new GooglePhotosFlow({
  endpoints: defaultEndpoints('/v1/google-photos'), // or hand-build Endpoints
  postMessageType: 'myapp:google-oauth',            // === Go CallbackPage.PostMessageType
  fetchJson: (url, init) => api(url, init),         // you inject auth + base URL; throw on non-2xx
});

flow.subscribe((s) => render(s));   // { phase, connected, progress, result, error, expired }
await flow.refreshStatus();         // safe outside a gesture

button.onclick = () =>
  flow.state.connected ? flow.start({ /* metadata? */ }) : flow.connect();
```

`start()` resolves exactly once with `{ savedIds, total, completed, failed }`
(also placed on `state.result`). `metadata` is only for the lib's
client-supplied path; apps deriving the destination server-side omit it.

## React

```tsx
import { useGooglePhotosFlow } from 'google-photos-picker-client/react';

const cfg = useMemo(() => ({ endpoints: …, postMessageType: …, fetchJson: … }), []);
const { state, connect, start } = useGooglePhotosFlow(cfg);
```

Status is fetched on mount, the flow cancelled on unmount. Memoise `cfg` — it's
read once.

## Svelte

```svelte
<script lang="ts">
  import { createGooglePhotosFlow } from 'google-photos-picker-client/svelte';
  const flow = createGooglePhotosFlow({ endpoints: …, postMessageType: …, fetchJson: … });
  onMount(flow.refreshStatus); onDestroy(flow.cancel);
</script>

{#if $flow.phase === 'importing'}{$flow.progress?.completed}/{$flow.progress?.total}{/if}
```

`flow` is a readable store (`$flow` = state) plus `connect` / `start` /
`disconnect` / `refreshStatus` / `cancel`. No `svelte` dependency.

## Security: pin the OAuth origin in production

The OAuth result is delivered to the opener via `window.postMessage`. In
production **set `expectedOrigin`** to the origin that served the callback page
(your API origin) so a message forged by another window can't spoof a
successful connect — and set the Go side's `CallbackPage.TargetOrigin` to your
frontend origin instead of the `"*"` default. Leaving both at their permissive
defaults is acceptable for local dev only.

## Config notes

- **`fetchJson`** is the single HTTP seam — inject the `Authorization` header
  and base URL here, parse JSON, **throw on non-2xx**.
- **`postMessageType`** must equal the Go side's
  `CallbackPage.PostMessageType`.
- **`expectedOrigin`** — the API origin that served the callback page. Optional
  but **strongly recommended in production** (see Security above); unset = no
  origin check, relying solely on the Go side's `targetOrigin`.
- **`endpoints`** are explicit because paths are the consumer's choice;
  `defaultEndpoints(base)` covers the all-under-one-prefix case.

## License

MIT © Sam Ford
