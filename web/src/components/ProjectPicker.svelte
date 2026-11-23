<script lang="ts">
  import { apiFetch } from '../lib/api';
  import { app } from '../lib/store.svelte';
  import type { ProjectBrowseEntry, ProjectBrowseResponse } from '../lib/types';

  let currentDir = $state('');
  let parentDir = $state('');
  let entries = $state<ProjectBrowseEntry[]>([]);
  let initOnOpen = $state(true);
  let loading = $state(false);
  let opening = $state(false);
  let err = $state('');

  async function browse(target = '') {
    loading = true;
    err = '';
    try {
      const qs = target ? `?path=${encodeURIComponent(target)}` : '';
      const res = await apiFetch<ProjectBrowseResponse>(`/api/v1/project/browse${qs}`);
      currentDir = res.current;
      parentDir = res.parent;
      entries = res.entries ?? [];
    } catch (ex) {
      err = ex instanceof Error ? ex.message : 'Failed to browse directory';
    } finally {
      loading = false;
    }
  }

  async function openDir(path: string) {
    if (!path.trim() || opening) return;
    opening = true;
    err = '';
    try {
      await app.openProject(path.trim(), initOnOpen);
      app.projectPickerOpen = false;
    } catch (ex) {
      err = ex instanceof Error ? ex.message : 'Failed to open workspace';
    } finally {
      opening = false;
    }
  }

  $effect(() => {
    if (app.projectPickerOpen) {
      browse(app.project?.workspace ?? '');
    }
  });
</script>

{#if app.projectPickerOpen}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="backdrop" onclick={() => (app.projectPickerOpen = false)} onkeydown={(e) => e.key === 'Escape' && (app.projectPickerOpen = false)}>
    <!-- svelte-ignore a11y_click_events_have_key_events -->
    <div class="picker card" onclick={(e) => e.stopPropagation()}>
      <div class="picker-head">
        <div>
          <h3>Open Project Workspace</h3>
          <span class="muted">Active: <code class="mono">{app.project?.workspace ?? '—'}</code></span>
        </div>
        <button class="btn btn-sm" onclick={() => (app.projectPickerOpen = false)}>Close</button>
      </div>

      <form
        class="path-bar"
        onsubmit={(e) => {
          e.preventDefault();
          browse(currentDir);
        }}
      >
        {#if parentDir}
          <button type="button" class="btn btn-sm" onclick={() => browse(parentDir)}>↑ Up</button>
        {/if}
        <input class="input mono" type="text" bind:value={currentDir} placeholder="/path/to/workspace" />
        <button type="submit" class="btn btn-sm" disabled={loading}>Browse</button>
        <button type="button" class="btn btn-sm btn-primary" disabled={opening || !currentDir.trim()} onclick={() => openDir(currentDir)}>
          {opening ? 'Opening…' : 'Open This Folder'}
        </button>
      </form>

      <label class="init-row">
        <input type="checkbox" bind:checked={initOnOpen} />
        <span>Initialize <code>.tether/</code>, <code>.tether/ignore</code>, and update <code>.gitignore</code> if missing</span>
      </label>

      {#if err}
        <div class="err-box">{err}</div>
      {/if}

      <div class="dir-list pane-scroll">
        {#if loading}
          <div class="empty-state">Scanning directories…</div>
        {:else if entries.length === 0}
          <div class="empty-state">No subdirectories</div>
        {:else}
          {#each entries as entry (entry.path)}
            <div class="dir-row">
              <button class="dir-name" onclick={() => browse(entry.path)}>
                <span class="mono">{entry.name}/</span>
              </button>
              <div class="dir-badges">
                {#if entry.is_tether_project}
                  <span class="pill pill-main">.tether</span>
                {/if}
                {#if entry.is_git_repo}
                  <span class="pill pill-rw">.git</span>
                {/if}
                <button class="btn btn-sm" onclick={() => openDir(entry.path)} disabled={opening}>Open</button>
              </div>
            </div>
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
    z-index: 95;
    background: rgba(17, 17, 27, 0.65);
    display: grid;
    place-items: center;
    padding: 16px;
  }
  .picker {
    width: min(680px, 100%);
    max-height: 80dvh;
    display: flex;
    flex-direction: column;
    gap: 10px;
    border-color: var(--border);
    box-shadow: var(--shadow);
  }
  .picker-head {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 12px;
  }
  .picker-head h3 {
    margin: 0;
    font-size: 15px;
    color: var(--main);
  }
  .muted {
    font-size: 12px;
    color: var(--subtext);
  }
  .path-bar {
    display: flex;
    gap: 6px;
    align-items: center;
  }
  .init-row {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 12px;
    color: var(--subtext);
  }
  .err-box {
    color: var(--danger);
    font-size: 12px;
    padding: 6px 10px;
    border: 1px solid rgba(243, 139, 168, 0.45);
    border-radius: var(--radius-sm);
  }
  .dir-list {
    display: flex;
    flex-direction: column;
    gap: 4px;
    max-height: 46dvh;
  }
  .dir-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 6px 8px;
    border-radius: var(--radius-sm);
    background: var(--mantle);
    border: 1px solid var(--border-subtle);
  }
  .dir-name {
    border: none;
    background: transparent;
    text-align: left;
    flex: 1;
    color: var(--crew);
    font-weight: 500;
  }
  .dir-name:hover {
    text-decoration: underline;
  }
  .dir-badges {
    display: flex;
    align-items: center;
    gap: 6px;
  }
</style>
