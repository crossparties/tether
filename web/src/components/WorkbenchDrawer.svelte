<script lang="ts">
  import { app } from '../lib/store.svelte';
  import type { WorkbenchTab } from '../lib/types';
  import FilesTab from './workbench/FilesTab.svelte';
  import DiffsTab from './workbench/DiffsTab.svelte';
  import PlannerTab from './workbench/PlannerTab.svelte';
  import TodosTab from './workbench/TodosTab.svelte';
  import TerminalTab from './workbench/TerminalTab.svelte';
  import UsageTab from './workbench/UsageTab.svelte';
  import PreviewTab from './workbench/PreviewTab.svelte';

  const tabs: Array<{ id: WorkbenchTab; label: string }> = [
    { id: 'files', label: 'Files & Mounts' },
    { id: 'diffs', label: 'Diff & Undo' },
    { id: 'planner', label: 'Planner' },
    { id: 'todos', label: 'Todos' },
    { id: 'terminal', label: 'Terminal' },
    { id: 'usage', label: 'Usage & Cache' },
    { id: 'preview', label: 'Preview' },
  ];

  const pendingTodos = $derived(app.todos.filter((t) => t.Status !== 'completed').length);
</script>

<section class="drawer">
  <div class="tab-rail" role="tablist">
    {#each tabs as tab (tab.id)}
      <button
        role="tab"
        aria-selected={app.workbenchTab === tab.id}
        class="tab-btn"
        class:active={app.workbenchTab === tab.id}
        onclick={() => (app.workbenchTab = tab.id)}
      >
        <span>{tab.label}</span>
        {#if tab.id === 'todos' && pendingTodos > 0}
          <span class="badge num">{pendingTodos}</span>
        {/if}
      </button>
    {/each}
  </div>

  <div class="tab-body">
    {#if app.workbenchTab === 'files'}
      <FilesTab />
    {:else if app.workbenchTab === 'diffs'}
      <DiffsTab />
    {:else if app.workbenchTab === 'planner'}
      <PlannerTab />
    {:else if app.workbenchTab === 'todos'}
      <TodosTab />
    {:else if app.workbenchTab === 'terminal'}
      <TerminalTab />
    {:else if app.workbenchTab === 'usage'}
      <UsageTab />
    {:else}
      <PreviewTab />
    {/if}
  </div>
</section>

<style>
  .drawer {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
    background: var(--base);
  }
  .tab-rail {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
    padding: 8px 10px;
    background: var(--mantle);
    border-bottom: 1px solid var(--border-subtle);
  }
  .tab-btn {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    padding: 5px 9px;
    border-radius: var(--radius-sm);
    border: 1px solid transparent;
    background: transparent;
    color: var(--subtext);
    font-size: 12px;
    font-weight: 500;
  }
  .tab-btn:hover {
    background: var(--overlay);
    color: var(--text);
  }
  .tab-btn.active {
    background: var(--overlay);
    border-color: var(--border);
    color: var(--main);
    font-weight: 600;
  }
  .badge {
    padding: 0 5px;
    border-radius: 999px;
    background: rgba(249, 226, 175, 0.2);
    color: var(--plan);
    font-size: 10.5px;
  }
  .tab-body {
    flex: 1;
    min-height: 0;
    padding: 10px;
    display: flex;
    flex-direction: column;
  }
</style>
