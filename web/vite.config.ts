import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

export default defineConfig({
  plugins: [svelte()],
  base: '/',
  build: {
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
    sourcemap: false,
    chunkSizeWarningLimit: 900,
    rollupOptions: {
      output: {
        manualChunks: {
          codemirror: [
            '@codemirror/state',
            '@codemirror/view',
            '@codemirror/commands',
            '@codemirror/language',
            '@codemirror/lang-javascript',
            '@codemirror/lang-json',
            '@codemirror/lang-markdown',
            '@codemirror/lang-python',
            '@codemirror/theme-one-dark',
            '@lezer/highlight',
          ],
          terminal: ['@xterm/xterm', '@xterm/addon-fit'],
        },
      },
    },
  },
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:8080',
      '/preview': 'http://127.0.0.1:8080',
    },
  },
});
