<script lang="ts">
  import { onDestroy, onMount } from 'svelte';
  import { Terminal } from '@xterm/xterm';
  import { FitAddon } from '@xterm/addon-fit';
  import '@xterm/xterm/css/xterm.css';
  import { app } from '../../lib/store.svelte';

  let host: HTMLDivElement | undefined = $state();
  let status = $state<'connecting' | 'connected' | 'closed'>('connecting');
  let term: Terminal | null = null;
  let fit: FitAddon | null = null;
  let ws: WebSocket | null = null;
  let observer: ResizeObserver | null = null;

  function terminalTheme(mode: 'mocha' | 'latte') {
    if (mode === 'latte') {
      return {
        background: '#EFF1F5',
        foreground: '#4C4F69',
        cursor: '#8839EF',
        selectionBackground: 'rgba(114, 135, 253, 0.25)',
        black: '#5C5F77',
        red: '#D20F39',
        green: '#40A02B',
        yellow: '#DF8E1D',
        blue: '#1E66F5',
        magenta: '#8839EF',
        cyan: '#179299',
        white: '#ACB0BE',
      };
    }
    return {
      background: '#1E1E2E',
      foreground: '#CDD6F4',
      cursor: '#CBA6F7',
      selectionBackground: 'rgba(180, 190, 254, 0.25)',
      black: '#45475A',
      red: '#F38BA8',
      green: '#A6E3A1',
      yellow: '#F9E2AF',
      blue: '#89B4FA',
      magenta: '#CBA6F7',
      cyan: '#94E2D5',
      white: '#BAC2DE',
    };
  }

  function sendResize() {
    if (!term || !ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
  }

  function connect() {
    if (!term) return;
    if (ws) {
      ws.close();
      ws = null;
    }
    status = 'connecting';
    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const sock = new WebSocket(`${proto}//${window.location.host}/api/v1/terminal/ws`);
    sock.binaryType = 'arraybuffer';
    ws = sock;

    sock.onopen = () => {
      if (ws !== sock) return;
      status = 'connected';
      fit?.fit();
      sendResize();
      term?.focus();
    };
    sock.onmessage = (ev) => {
      if (ws !== sock || !term) return;
      if (typeof ev.data === 'string') {
        term.write(ev.data);
      } else if (ev.data instanceof ArrayBuffer) {
        term.write(new Uint8Array(ev.data));
      }
    };
    sock.onclose = () => {
      if (ws !== sock) return;
      status = 'closed';
    };
    sock.onerror = () => {
      if (ws !== sock) return;
      status = 'closed';
    };
  }

  onMount(() => {
    if (!host) return;
    const t = new Terminal({
      cursorBlink: true,
      fontFamily: '"JetBrains Mono", ui-monospace, monospace',
      fontSize: 12.5,
      theme: terminalTheme(app.theme),
    });
    const f = new FitAddon();
    t.loadAddon(f);
    t.open(host);
    f.fit();

    t.onData((data) => {
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(data);
      }
    });
    t.onResize(() => sendResize());

    term = t;
    fit = f;

    observer = new ResizeObserver(() => {
      f.fit();
      sendResize();
    });
    observer.observe(host);

    connect();
  });

  $effect(() => {
    if (term) {
      term.options.theme = terminalTheme(app.theme);
    }
  });

  onDestroy(() => {
    observer?.disconnect();
    ws?.close();
    term?.dispose();
  });
</script>

<div class="term-tab">
  <div class="term-bar">
    <div class="term-meta">
      <span class="pill" class:pill-rw={status === 'connected'} class:pill-guarded={status === 'connecting'} class:pill-danger={status === 'closed'}>
        PTY {status}
      </span>
      <span class="muted mono">{app.project?.workspace ?? ''}</span>
    </div>
    <div class="term-acts">
      <button class="btn btn-sm" onclick={() => term?.clear()}>Clear</button>
      <button class="btn btn-sm btn-crew" onclick={connect}>Reconnect</button>
    </div>
  </div>
  <div class="term-host" bind:this={host}></div>
</div>

<style>
  .term-tab {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
    gap: 8px;
  }
  .term-bar,
  .term-meta,
  .term-acts {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
  }
  .muted {
    color: var(--muted);
    font-size: 11px;
  }
  .term-host {
    flex: 1;
    min-height: 0;
    padding: 6px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border-subtle);
    background: var(--base);
    overflow: hidden;
  }
</style>
