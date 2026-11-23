<script lang="ts">
  import { apiFetch } from '../../lib/api';
  import { app } from '../../lib/store.svelte';
  import type { AccessMode, IgnoreRule, SimulationResult } from '../../lib/types';
  import CodeEditor from '../CodeEditor.svelte';

  let searchQ = $state('');
  let selectedPath = $state('');
  let fileContent = $state('');
  let fileMode = $state<AccessMode>('rw');
  let fileSize = $state(0);
  let fileTruncated = $state(false);
  let fileErr = $state('');
  let loadingFile = $state(false);

  // Ignore Rules & Impact Simulator
  let showIgnorePanel = $state(false);
  let ignoreContent = $state('');
  let ignoreDirty = $state(false);
  let builtinPatterns = $state<string[]>([]);
  let customRules = $state<IgnoreRule[]>([]);
  let ignoreErr = $state('');
  let simResult = $state<SimulationResult | null>(null);
  let simBusy = $state(false);
  let savingIgnore = $state(false);

  async function loadIgnore() {
    try {
      const res = await apiFetch<{
        content: string;
        builtin_patterns: string[];
        custom_rules: IgnoreRule[];
        parse_errors?: string[];
      }>('/api/v1/ignore');
      ignoreContent = res.content ?? '';
      ignoreDirty = false;
      builtinPatterns = res.builtin_patterns ?? [];
      customRules = res.custom_rules ?? [];
      ignoreErr = (res.parse_errors ?? []).join('; ');
    } catch (ex) {
      ignoreErr = ex instanceof Error ? ex.message : 'Failed to load .tether/ignore';
    }
  }

  $effect(() => {
    app.mountsVersion;
    if (showIgnorePanel && !ignoreDirty) loadIgnore();
  });

  async function openFile(relPath: string) {
    selectedPath = relPath;
    loadingFile = true;
    fileErr = '';
    try {
      const res = await apiFetch<{
        path: string;
        access_mode: AccessMode;
        content: string;
        size_bytes: number;
        truncated: boolean;
      }>(`/api/v1/files/content?path=${encodeURIComponent(relPath)}`);
      fileContent = res.content;
      fileMode = res.access_mode;
      fileSize = res.size_bytes;
      fileTruncated = res.truncated;
    } catch (ex) {
      fileErr = ex instanceof Error ? ex.message : 'Blocked or unreadable file';
      fileContent = '';
    } finally {
      loadingFile = false;
    }
  }

  async function setMode(relPath: string, accessMode: AccessMode) {
    try {
      await apiFetch('/api/v1/mounts/mode', {
        method: 'PATCH',
        body: JSON.stringify({ path: relPath, access_mode: accessMode }),
      });
      await app.refreshMounts(searchQ);
      if (selectedPath === relPath) fileMode = accessMode;
      app.notify(`Mount mode for ${relPath} -> ${accessMode.toUpperCase()}`, 'success');
    } catch (ex) {
      app.notify(ex instanceof Error ? ex.message : 'Failed to update mount mode', 'error');
    }
  }

  async function releaseLease(crewId: string) {
    try {
      await apiFetch(`/api/v1/crews/${encodeURIComponent(crewId)}/release-lock`, {
        method: 'POST',
        body: '{}',
      });
      await Promise.all([app.refreshMounts(searchQ), app.refreshCrews()]);
      app.notify(`Released lease lock held by ${crewId}`, 'info');
    } catch (ex) {
      app.notify(ex instanceof Error ? ex.message : 'Failed to release lock', 'error');
    }
  }

  async function runSimulate() {
    simBusy = true;
    ignoreErr = '';
    try {
      simResult = await apiFetch<SimulationResult>('/api/v1/ignore/test', {
        method: 'POST',
        body: JSON.stringify({ content: ignoreContent }),
      });
    } catch (ex) {
      ignoreErr = ex instanceof Error ? ex.message : 'Simulation failed';
    } finally {
      simBusy = false;
    }
  }

  async function saveIgnore() {
    savingIgnore = true;
    ignoreErr = '';
    try {
      const res = await apiFetch<{ ok: boolean; purged_kb: number }>('/api/v1/ignore', {
        method: 'POST',
        body: JSON.stringify({ content: ignoreContent }),
      });
      ignoreDirty = false;
      await Promise.all([app.refreshMounts(searchQ), loadIgnore()]);
      app.notify(`Saved .tether/ignore (purged ${res.purged_kb} KB entries)`, 'success');
    } catch (ex) {
      ignoreErr = ex instanceof Error ? ex.message : 'Failed to save ignore rules';
    } finally {
      savingIgnore = false;
    }
  }

  async function convertGitignore() {
    try {
      const res = await apiFetch<{ converted: string; skipped?: string[] }>('/api/v1/ignore/convert-gitignore', {
        method: 'POST',
        body: JSON.stringify({ apply: false }),
      });
      ignoreContent = res.converted;
      ignoreDirty = true;
      await runSimulate();
      app.notify('Previewed .gitignore conversion — review impact before saving', 'info');
    } catch (ex) {
      ignoreErr = ex instanceof Error ? ex.message : 'No .gitignore found or conversion failed';
    }
  }

  const visibleFiles = $derived.by(() => {
    const q = searchQ.trim().toLowerCase();
    const list = app.mounts?.files ?? [];
    if (!q) return list;
    return list.filter((f) => f.rel_path.toLowerCase().includes(q));
  });

  function modePillClass(m: AccessMode) {
    if (m === 'rw') return 'pill-rw';
    if (m === 'guarded') return 'pill-guarded';
    if (m === 'ignored') return 'pill-danger';
    return 'pill-crew';
  }
</script>

<div class="files-tab">
  <div class="ft-top">
    <input
      class="input ft-search"
      type="text"
      placeholder="Filter mounted files…"
      bind:value={searchQ}
    />
    <button
      class="btn btn-sm"
      class:btn-primary={showIgnorePanel}
      onclick={() => {
        showIgnorePanel = !showIgnorePanel;
        if (showIgnorePanel) loadIgnore();
      }}
    >
      {showIgnorePanel ? 'Back to Viewer' : '.tether/ignore & Secrets'}
    </button>
  </div>

  {#if showIgnorePanel}
    <div class="ignore-panel pane-scroll">
      <div class="card sec-box">
        <div class="sec-head">
          <span class="pill pill-danger">Built-In Secret Protection (Always Active)</span>
          <span class="num muted">{builtinPatterns.length} rules</span>
        </div>
        <div class="chip-wrap">
          {#each builtinPatterns as pat (pat)}
            <code class="secret-chip">{pat}</code>
          {/each}
        </div>
      </div>

      <div class="card sec-box">
        <div class="sec-head">
          <span class="pill pill-guarded">User .tether/ignore Rules (.gitignore globs or re:regex)</span>
          <div class="ig-btns">
            <button class="btn btn-sm" onclick={convertGitignore}>Convert .gitignore</button>
            <button class="btn btn-sm btn-crew" onclick={runSimulate} disabled={simBusy}>
              {simBusy ? 'Testing…' : 'Test Impact'}
            </button>
            <button class="btn btn-sm btn-primary" onclick={saveIgnore} disabled={savingIgnore}>
              {savingIgnore ? 'Saving…' : 'Save & Reconcile KB'}
            </button>
          </div>
        </div>
        <textarea
          class="textarea mono ig-editor"
          rows={7}
          bind:value={ignoreContent}
          oninput={() => (ignoreDirty = true)}
          placeholder="node_modules/\ndist/\n*.log\nre:^tmp/.*$"
        ></textarea>
        {#if ignoreErr}
          <div class="err-txt">{ignoreErr}</div>
        {/if}

        {#if simResult}
          <div class="sim-card">
            <div class="sim-stats">
              <span class="num">Matches <strong>{simResult.matched_count}</strong> / {simResult.total_files} files</span>
              {#if simResult.by_extension}
                <div class="ext-chips">
                  {#each Object.entries(simResult.by_extension).slice(0, 8) as [ext, cnt]}
                    <span class="pill pill-muted num">{ext || '(no ext)'}: {cnt}</span>
                  {/each}
                </div>
              {/if}
            </div>
            {#if (simResult.warnings?.length ?? 0) > 0}
              <div class="warn-list">
                {#each simResult.warnings ?? [] as w}
                  <div class="warn-row">⚠ {w.message} (<code>{w.pattern}</code>)</div>
                {/each}
              </div>
            {/if}
            {#if (simResult.invalid_patterns?.length ?? 0) > 0}
              <div class="err-txt">Invalid patterns: {simResult.invalid_patterns?.join(', ')}</div>
            {/if}
            {#if (simResult.matched_samples?.length ?? 0) > 0}
              <div class="sample-list mono">{simResult.matched_samples?.slice(0, 12).join('\n')}</div>
            {/if}
          </div>
        {/if}

        {#if customRules.length > 0}
          <div class="muted">Active parsed rules: {customRules.length}</div>
        {/if}
      </div>
    </div>
  {:else}
    <div class="ft-split">
      <div class="ft-list pane-scroll">
        {#if visibleFiles.length === 0}
          <div class="empty-state">No mounted files match</div>
        {:else}
          {#each visibleFiles as f (f.rel_path)}
            <div class="frow" class:selected={selectedPath === f.rel_path}>
              <button class="fname mono" onclick={() => openFile(f.rel_path)} title={f.rel_path}>
                {f.rel_path}
              </button>
              <div class="fmeta">
                {#if f.lease_crew_id}
                  <button
                    class="pill pill-guarded"
                    title="Locked by Crew {f.lease_crew_id} — click to force release"
                    onclick={() => releaseLease(f.lease_crew_id!)}
                  >
                    🔒 Release
                  </button>
                {/if}
                <select
                  class="mode-sel {modePillClass(f.access_mode)}"
                  value={f.access_mode}
                  aria-label="Access mode for {f.rel_path}"
                  onchange={(e) => setMode(f.rel_path, (e.currentTarget as HTMLSelectElement).value as AccessMode)}
                >
                  <option value="rw">RW</option>
                  <option value="guarded">Guarded</option>
                  <option value="ro">RO</option>
                  <option value="ignored">Ignored</option>
                </select>
              </div>
            </div>
          {/each}
        {/if}
      </div>

      <div class="ft-viewer">
        {#if !selectedPath}
          <div class="empty-state">Select a file to inspect in CodeMirror 6 (readonly)</div>
        {:else if loadingFile}
          <div class="empty-state">Loading {selectedPath}…</div>
        {:else if fileErr}
          <div class="err-txt card">{fileErr}</div>
        {:else}
          <div class="viewer-head">
            <span class="mono vpath">{selectedPath}</span>
            <div class="vmeta">
              <span class="pill {modePillClass(fileMode)}">{fileMode.toUpperCase()}</span>
              <span class="num muted">{fileSize} B{fileTruncated ? ' (truncated)' : ''}</span>
            </div>
          </div>
          <div class="viewer-cm">
            <CodeEditor value={fileContent} filename={selectedPath} readonly={true} minHeight="180px" />
          </div>
        {/if}
      </div>
    </div>
  {/if}
</div>

<style>
  .files-tab {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
    gap: 8px;
  }
  .ft-top {
    display: flex;
    gap: 6px;
    align-items: center;
  }
  .ft-search {
    padding: 5px 8px;
    font-size: 12.5px;
  }
  .ft-split {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .ft-list {
    max-height: 44%;
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm);
    background: var(--mantle);
    padding: 4px;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .frow {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 6px;
    padding: 3px 6px;
    border-radius: 4px;
  }
  .frow.selected,
  .frow:hover {
    background: var(--overlay);
  }
  .fname {
    border: none;
    background: transparent;
    text-align: left;
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: 12px;
    color: var(--text);
  }
  .fmeta {
    display: flex;
    align-items: center;
    gap: 4px;
  }
  .mode-sel {
    border-radius: 999px;
    padding: 1px 6px;
    font-size: 10.5px;
    font-weight: 600;
    background: var(--surface0);
  }
  .ft-viewer {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .viewer-head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 8px;
  }
  .vpath {
    font-size: 12px;
    color: var(--plan);
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .vmeta {
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .viewer-cm {
    flex: 1;
    min-height: 0;
  }
  .ignore-panel {
    flex: 1;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .sec-box {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .sec-head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    flex-wrap: wrap;
    gap: 8px;
  }
  .ig-btns {
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
  }
  .chip-wrap {
    display: flex;
    flex-wrap: wrap;
    gap: 5px;
  }
  .secret-chip {
    font-size: 11px;
    padding: 2px 6px;
    border-radius: 4px;
    background: rgba(243, 139, 168, 0.12);
    color: var(--danger);
    border: 1px solid rgba(243, 139, 168, 0.35);
  }
  .ig-editor {
    font-size: 12px;
    resize: vertical;
  }
  .sim-card {
    background: var(--mantle);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm);
    padding: 8px;
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .sim-stats,
  .ext-chips {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    align-items: center;
  }
  .warn-row {
    color: var(--guarded);
    font-size: 12px;
  }
  .err-txt {
    color: var(--danger);
    font-size: 12px;
  }
  .sample-list {
    font-size: 11px;
    color: var(--subtext);
    white-space: pre-wrap;
    max-height: 110px;
    overflow-y: auto;
  }
  .muted {
    color: var(--muted);
    font-size: 11px;
  }
</style>
