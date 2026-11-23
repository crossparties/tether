<script lang="ts">
  import { app } from '../lib/store.svelte';
  import type { WorkbenchTab } from '../lib/types';

  interface PaletteItem {
    id: string;
    group: string;
    label: string;
    hint?: string;
    run: () => void | Promise<void>;
  }

  let query = $state('');
  let selectedIdx = $state(0);
  let inputEl: HTMLInputElement | undefined = $state();

  function openTab(tab: WorkbenchTab) {
    app.activeView = 'chat';
    app.drawerOpen = true;
    app.workbenchTab = tab;
    close();
  }

  function close() {
    app.commandPaletteOpen = false;
    query = '';
    selectedIdx = 0;
  }

  const allItems = $derived.by((): PaletteItem[] => [
    { id: 'view-chat', group: 'Navigate', label: 'Main Chat & Workbench', hint: 'chat', run: () => { app.activeView = 'chat'; close(); } },
    { id: 'view-setup', group: 'Navigate', label: 'Setup Wizard & API Key Pool', hint: 'setup', run: () => { app.activeView = 'setup'; close(); } },
    { id: 'view-mcp', group: 'Customize', label: 'External MCP Servers (/mcp)', hint: '/mcp', run: () => { app.activeView = 'customize'; app.customizeTab = 'mcp'; close(); } },
    { id: 'view-skills', group: 'Customize', label: 'Agent Skills Editor (/skills)', hint: '/skills', run: () => { app.activeView = 'customize'; app.customizeTab = 'skills'; close(); } },
    { id: 'view-memory', group: 'Customize', label: 'Substrate Decisions & Patterns (/memory)', hint: '/memory', run: () => { app.activeView = 'customize'; app.customizeTab = 'memory'; close(); } },
    { id: 'view-settings', group: 'Navigate', label: 'Settings & Interactive Doctor (/settings)', hint: '/settings', run: () => { app.activeView = 'settings'; close(); } },

    { id: 'tab-files', group: 'Workbench', label: 'Files & Mounts + Ignore Tester', hint: 'drawer', run: () => openTab('files') },
    { id: 'tab-diffs', group: 'Workbench', label: 'Diff & Safe 1-Click Undo', hint: 'drawer', run: () => openTab('diffs') },
    { id: 'tab-planner', group: 'Workbench', label: 'Planner (.tether/PLAN.md)', hint: 'drawer', run: () => openTab('planner') },
    { id: 'tab-todos', group: 'Workbench', label: 'Session Todos', hint: 'drawer', run: () => openTab('todos') },
    { id: 'tab-term', group: 'Workbench', label: 'Interactive Terminal (PTY)', hint: 'drawer', run: () => openTab('terminal') },
    { id: 'tab-usage', group: 'Workbench', label: 'Usage & Prompt Cache Waterfall', hint: 'drawer', run: () => openTab('usage') },
    { id: 'tab-preview', group: 'Workbench', label: 'Sandboxed Browser Preview', hint: 'drawer', run: () => openTab('preview') },

    { id: 'mode-code', group: 'Mode', label: 'Switch to CODE Mode', hint: '/mode code', run: async () => { await app.switchMode('code'); close(); } },
    { id: 'mode-plan', group: 'Mode', label: 'Switch to PLAN Mode', hint: '/mode plan', run: async () => { await app.switchMode('plan'); close(); } },
    { id: 'mode-review', group: 'Mode', label: 'Switch to REVIEW Mode', hint: '/mode review', run: async () => { await app.switchMode('review'); close(); } },

    { id: 'act-project', group: 'Action', label: 'Open / Switch Project Workspace', hint: 'Ctrl+O', run: () => { close(); app.projectPickerOpen = true; } },
    { id: 'act-thread', group: 'Action', label: 'New Discussion Thread', hint: 'thread', run: async () => { await app.createThread(); close(); } },
    { id: 'act-btw', group: 'Action', label: 'Toggle Floating /btw Side-Chat', hint: '/btw', run: () => { app.btwOpen = !app.btwOpen; close(); } },
    { id: 'act-compact', group: 'Action', label: 'Compact Now (EpisodicCondenser)', hint: '/compact', run: async () => { close(); await app.compactNow(); } },
    { id: 'act-theme', group: 'Action', label: `Switch Theme (${app.theme === 'mocha' ? 'Latte Light' : 'Mocha Dark'})`, hint: 'theme', run: () => { app.toggleTheme(); close(); } },
  ]);

  const filtered = $derived.by(() => {
    const q = query.trim().toLowerCase();
    if (!q) return allItems;
    return allItems.filter(
      (i) => i.label.toLowerCase().includes(q) || (i.hint ?? '').toLowerCase().includes(q) || i.group.toLowerCase().includes(q)
    );
  });

  $effect(() => {
    if (app.commandPaletteOpen) {
      queueMicrotask(() => inputEl?.focus());
    }
  });

  $effect(() => {
    if (selectedIdx >= filtered.length) {
      selectedIdx = Math.max(0, filtered.length - 1);
    }
  });

  function onKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.preventDefault();
      close();
    } else if (e.key === 'ArrowDown') {
      e.preventDefault();
      if (filtered.length > 0) selectedIdx = (selectedIdx + 1) % filtered.length;
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      if (filtered.length > 0) selectedIdx = (selectedIdx - 1 + filtered.length) % filtered.length;
    } else if (e.key === 'Enter') {
      e.preventDefault();
      filtered[selectedIdx]?.run();
    }
  }
</script>

{#if app.commandPaletteOpen}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="backdrop" onclick={close} onkeydown={onKeydown}>
    <!-- svelte-ignore a11y_click_events_have_key_events -->
    <div class="palette card" onclick={(e) => e.stopPropagation()}>
      <div class="pal-head">
        <input
          bind:this={inputEl}
          class="input pal-input"
          type="text"
          placeholder="Jump to view, workbench tab, mode, or /command…"
          bind:value={query}
        />
        <span class="kbd">ESC</span>
      </div>
      <div class="pal-list pane-scroll">
        {#if filtered.length === 0}
          <div class="pal-empty">No matching command</div>
        {:else}
          {#each filtered as item, i (item.id)}
            <button
              class="pal-item"
              class:selected={i === selectedIdx}
              onclick={() => item.run()}
              onmouseenter={() => (selectedIdx = i)}
            >
              <span class="pal-group">{item.group}</span>
              <span class="pal-label">{item.label}</span>
              {#if item.hint}
                <span class="kbd">{item.hint}</span>
              {/if}
            </button>
          {/each}
        {/if}
      </div>
    </div>
  </div>
{/if}

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    z-index: 90;
    background: rgba(17, 17, 27, 0.65);
    display: flex;
    justify-content: center;
    align-items: flex-start;
    padding-top: 12vh;
  }
  .palette {
    width: min(600px, calc(100% - 24px));
    max-height: 70dvh;
    display: flex;
    flex-direction: column;
    padding: 0;
    border-color: var(--border);
    box-shadow: var(--shadow);
    overflow: hidden;
  }
  .pal-head {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 10px 12px;
    border-bottom: 1px solid var(--border-subtle);
    background: var(--mantle);
  }
  .pal-input {
    border: none;
    background: transparent;
    padding: 4px;
    font-size: 14px;
  }
  .pal-list {
    padding: 6px;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .pal-item {
    display: flex;
    align-items: center;
    gap: 10px;
    width: 100%;
    text-align: left;
    padding: 8px 10px;
    border-radius: var(--radius-sm);
    border: 1px solid transparent;
    background: transparent;
  }
  .pal-item.selected {
    background: var(--overlay);
    border-color: var(--border-subtle);
  }
  .pal-group {
    font-size: 10.5px;
    text-transform: uppercase;
    color: var(--main);
    min-width: 74px;
    font-weight: 600;
  }
  .pal-label {
    flex: 1;
    color: var(--text);
  }
  .pal-empty {
    padding: 20px;
    text-align: center;
    color: var(--muted);
  }
</style>
