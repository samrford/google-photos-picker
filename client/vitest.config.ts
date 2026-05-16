import { defineConfig } from 'vitest/config';

// The core injects openWindow / messageTarget / fetchJson, so the smoke tests
// need no DOM — plain node is enough and keeps the suite fast.
export default defineConfig({
  test: {
    environment: 'node',
  },
});
