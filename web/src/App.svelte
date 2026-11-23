<script lang="ts">
  import { onDestroy, onMount } from 'svelte';
  import AuthGate from './components/AuthGate.svelte';
  import CommandPalette from './components/CommandPalette.svelte';
  import ProjectPicker from './components/ProjectPicker.svelte';
  import { app } from './lib/store.svelte';
  import ChatWorkspace from './views/ChatWorkspace.svelte';
  import CustomizeView from './views/CustomizeView.svelte';
  import SettingsView from './views/SettingsView.svelte';
  import SetupView from './views/SetupView.svelte';

  function onGlobalKeydown(e: KeyboardEvent) {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') {
      e.preventDefault();
      app.commandPaletteOpen = !app.commandPaletteOpen;
    } else if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'o') {
      e.preventDefault();
      app.projectPickerOpen = !app.projectPickerOpen;
    }
  }

  onMount(() => {
    window.addEventListener('keydown', onGlobalKeydown);
    app.init();
  });

  onDestroy(() => {
    window.removeEventListener('keydown', onGlobalKeydown);
    app.disconnectSSE();
  });

  const wsName = $derived.by(() => {
    const w = app.project?.workspace ?? app.session?.workspace ?? '';
    if (!w) return 'workspace';
    const parts = w.split('/').filter(Boolean);
    return parts[parts.length - 1] || w;
  });

  const pressurePct = $derived(Math.round((app.tokens?.main?.context_pressure ?? 0) * 100));
  const cachePct = $derived(Math.round((app.tokens?.main?.cache_hit_ratio ?? 0) * 100));
</script>

{#if app.bootstrapping}
  <div class="boot">
    <span class="pill pill-main">TETHER v0.3.0</span>
    <span>Connecting to local substrate…</span>
  </div>
{:else if app.authRequired}
  <AuthGate />
{:else}
  <div class="shell">
    <header class="topbar">
      <div class="tb-left">
        <span class="brand">TETHER <span class="ver">v0.3.0</span></span>
        <button class="btn btn-sm ws-btn" onclick={() => (app.projectPickerOpen = true)} title={app.project?.workspace ?? ''}>
          <span class="mono">{wsName}</span>
          {#if app.project?.branch}
            <span class="branch">({app.project.branch})</span>
          {/if}
          <span class="kbd">Ctrl+O</span>
        </button>

        <select
          class="mode-pill"
          class:plan={app.mode === 'plan'}
          class:review={app.mode === 'review'}
          value={app.mode}
          aria-label="Governance Mode"
          onchange={(e) => app.switchMode((e.currentTarget as HTMLSelectElement).value as 'code' | 'plan' | 'review')}
        >
          <option value="code">CODE</option>
          <option value="plan">PLAN</option>
          <option value="review">REVIEW</option>
        </select>

        {#if app.project?.model || app.setupStatus?.active_model}
          <span class="model-chip mono">
            {app.project?.provider || app.setupStatus?.active_provider}/{app.project?.model || app.setupStatus?.active_model}
          </span>
        {/if}
      </div>

      <nav class="tb-nav">
        <button class="btn btn-sm" class:btn-primary={app.activeView === 'chat'} onclick={() => (app.activeView = 'chat')}>
          Chat
        </button>
        <button class="btn btn-sm" class:btn-primary={app.activeView === 'setup'} onclick={() => (app.activeView = 'setup')}>
          Setup
        </button>
        <button class="btn btn-sm" class:btn-primary={app.activeView === 'customize'} onclick={() => (app.activeView = 'customize')}>
          Customize
        </button>
        <button class="btn btn-sm" class:btn-primary={app.activeView === 'settings'} onclick={() => (app.activeView = 'settings')}>
          Settings
        </button>
      </nav>

      <div class="tb-right">
        <button
          class="btn btn-sm num"
          onclick={() => {
            app.activeView = 'chat';
            app.drawerOpen = true;
            app.workbenchTab = 'usage';
          }}
          title="Context Pressure & Prompt Cache Hit"
        >
          <span class:warn-pct={pressurePct >= 72}>{pressurePct}% ctx</span>
          <span>•</span>
          <span class="cache-pct">{cachePct}% hit</span>
        </button>

        <button class="btn btn-sm" onclick={() => app.toggleTheme()} title="Toggle Catppuccin Mocha / Latte">
          {app.theme === 'mocha' ? '☀ Latte' : '☾ Mocha'}
        </button>

        <button class="btn btn-sm" onclick={() => (app.commandPaletteOpen = true)}>
          Palette <span class="kbd">Ctrl+K</span>
        </button>

        <span
          class="sse-dot"
          class:online={app.sseConnected}
          title={app.sseConnected ? 'SSE live connected' : 'SSE reconnecting…'}
        ></span>
      </div>
    </header>

    <main class="main-area">
      {#if app.activeView === 'chat'}
        <ChatWorkspace />
      {:else if app.activeView === 'setup'}
        <SetupView />
      {:else if app.activeView === 'customize'}
        <CustomizeView />
      {:else}
        <SettingsView />
      {/if}
    </main>
  </div>

  <CommandPalette />
  <ProjectPicker />

  {#if app.toast}
    <div class="toast card" class:err={app.toast.kind === 'error'} class:warn={app.toast.kind === 'warn'} class:ok={app.toast.kind === 'success'}>
      {app.toast.text}
    </div>
  {/if}
{/if}

<style>
  .boot {
    height: 100dvh;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: 10px;
    color: var(--subtext);
  }
  .shell {
    height: 100dvh;
    width: 100%;
    display: flex;
    flex-direction: column;
    overflow: hidden;
  }
  .topbar {
    min-height: 44px;
    padding: 4px 12px;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
    flex-wrap: wrap;
    background: var(--crust);
    border-bottom: 1px solid var(--border-subtle);
  }
  .tb-left,
  .tb-nav,
  .tb-right {
    display: flex;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
  }
  .brand {
    font-weight: 800;
    letter-spacing: 0.04em;
    color: var(--main);
    font-size: 13px;
  }
  .ver {
    color: var(--muted);
    font-weight: 500;
    font-size: 11px;
  }
  .ws-btn {
    padding: 3px 8px;
  }
  .branch {
    color: var(--crew);
    font-size: 11.5px;
  }
  .mode-pill {
    border-radius: 999px;
    padding: 2px 8px;
    font-size: 11px;
    font-weight: 700;
    background: rgba(166, 227, 161, 0.14);
    color: var(--rw);
    border: 1px solid rgba(166, 227, 161, 0.45);
  }
  .mode-pill.plan {
    background: rgba(249, 226, 175, 0.14);
    color: var(--plan);
    border-color: rgba(249, 226, 175, 0.45);
  }
  .mode-pill.review {
    background: rgba(137, 180, 250, 0.14);
    color: var(--crew);
    border-color: rgba(137, 180, 250, 0.45);
  }
  .model-chip {
    font-size: 11.5px;
    color: var(--subtext);
    max-width: 200px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .warn-pct { color: var(--danger); font-weight: 700; }
  .cache-pct { color: var(--rw); font-weight: 600; }
  .sse-dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--danger);
    display: inline-block;
  }
  .sse-dot.online {
    background: var(--rw);
  }
  .main-area {
    flex: 1;
    min-height: 0;
    overflow: hidden;
  }
  .toast {
    position: fixed;
    left: 16px;
    bottom: 16px;
    z-index: 110;
    padding: 8px 14px;
    border-color: var(--border);
    box-shadow: var(--shadow);
    font-size: 12.5px;
    max-width: min(440px, calc(100vw - 32px));
  }
  .toast.ok { border-color: var(--rw); color: var(--rw); }
  .toast.warn { border-color: var(--guarded); color: var(--guarded); }
  .toast.err { border-color: var(--danger); color: var(--danger); }
</style>
