import { defineConfig } from 'tsup';

export default defineConfig({
  entry: {
    index: 'src/index.ts',
    react: 'src/react.ts',
    svelte: 'src/svelte.ts',
  },
  format: ['esm', 'cjs'],
  dts: true,
  clean: true,
  treeshake: true,
  // React is a peer dep of the /react entry only; never bundle it.
  external: ['react'],
});
