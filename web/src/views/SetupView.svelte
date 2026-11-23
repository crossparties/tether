<script lang="ts">
  import { onMount } from 'svelte';
  import { apiFetch } from '../lib/api';
  import { app } from '../lib/store.svelte';
  import type { KeyHealthEntry, MaskedKeyView, SetupStatusResponse } from '../lib/types';

  let status = $state<SetupStatusResponse | null>(null);
  let loading = $state(false);
  let probing = $state(false);
  let saving = $state(false);
  let initBusy = $state(false);
  let showAllPool = $state(false);

  // Stage 1 key form
  let provider = $state('deepseek');
  let apiKey = $state('');
  let baseUrl = $state('');
  let model = $state('');
  let label = $state('');
  let priority = $state(1);
  let probeResults = $state<KeyHealthEntry[]>([]);
  let formMsg = $state('');
  let formErr = $state('');

  const presets = $derived(status?.presets ?? []);
  const selectedPreset = $derived(presets.find((p) => p.name === provider));
  const keys = $derived(status?.keys ?? []);
  const multiKey = $derived(keys.length > 1);

  async function loadStatus() {
    loading = true;
    try {
      const res = await apiFetch<SetupStatusResponse>('/api/v1/setup/status');
      status = res;
      app.setupStatus = res;
      if (res.active_provider) provider = res.active_provider;
      if (res.active_model && !model) model = res.active_model;
      if (res.keys && res.keys.length > 1) showAllPool = true;
    } catch (ex) {
      formErr = ex instanceof Error ? ex.message : 'Failed to load setup status';
    } finally {
      loading = false;
    }
  }

  onMount(loadStatus);

  function onProviderChange(nextProv: string) {
    provider = nextProv;
    const preset = presets.find((p) => p.name === nextProv);
    baseUrl = '';
    model = preset?.default_model ?? '';
  }

  async function probeSingleOrAll(single = false) {
    probing = true;
    formErr = '';
    formMsg = '';
    try {
      const body = single
        ? { provider, api_key: apiKey.trim(), base_url: baseUrl.trim(), model: model.trim() }
        : {};
      const res = await apiFetch<{ results: KeyHealthEntry[]; summary?: string }>('/api/v1/keys/probe', {
        method: 'POST',
        body: JSON.stringify(body),
      });
      probeResults = res.results ?? [];
      await loadStatus();
      formMsg = probeResults.length > 0
        ? `Probed ${probeResults.length} key(s) cleanly (0-token)`
        : 'No enabled keys to probe yet';
    } catch (ex) {
      formErr = ex instanceof Error ? ex.message : 'Key probe failed';
    } finally {
      probing = false;
    }
  }

  async function saveKey(e: Event) {
    e.preventDefault();
    saving = true;
    formErr = '';
    formMsg = '';
    try {
      await apiFetch('/api/v1/keys', {
        method: 'PUT',
        body: JSON.stringify({
          provider,
          api_key: apiKey.trim(),
          label: label.trim() || undefined,
          base_url: baseUrl.trim() || undefined,
          model: model.trim() || undefined,
          priority,
          enabled: true,
        }),
      });
      apiKey = '';
      await Promise.all([loadStatus(), app.refreshAll()]);
      formMsg = `Saved ${provider} key to ~/.config/tether/credentials.json (0600)`;
      await probeSingleOrAll(false);
    } catch (ex) {
      formErr = ex instanceof Error ? ex.message : 'Failed to save key';
    } finally {
      saving = false;
    }
  }

  async function toggleKeyEnabled(k: MaskedKeyView) {
    const sameProv = keys
      .filter((x) => x.provider === k.provider)
      .map((x) => ({
        id: x.id,
        label: x.label,
        api_key: x.masked_key || '',
        base_url: x.base_url,
        model: x.model,
        priority: x.priority,
        enabled: x.id === k.id ? !x.enabled : x.enabled,
      }));
    try {
      await apiFetch('/api/v1/keys', {
        method: 'PUT',
        body: JSON.stringify({ provider: k.provider, entries: sameProv }),
      });
      await loadStatus();
    } catch (ex) {
      formErr = ex instanceof Error ? ex.message : 'Failed to toggle key';
    }
  }

  async function initProject() {
    if (!status?.workspace) return;
    initBusy = true;
    try {
      await app.openProject(status.workspace, true);
      await loadStatus();
    } catch (ex) {
      formErr = ex instanceof Error ? ex.message : 'Project init failed';
    } finally {
      initBusy = false;
    }
  }

  async function enableSerenaOneClick() {
    const sug = status?.serena_suggestion;
    if (!sug) return;
    if (sug.serena_status === 'disabled_draft') {
      const targetName = sug.serena_name || 'serena';
      try {
        const testRes = await apiFetch<{ ok: boolean; error?: string }>('/api/v1/mcp/test', {
          method: 'POST',
          body: JSON.stringify({ name: targetName }),
        });
        if (!testRes.ok) {
          app.notify(testRes.error || `${targetName} test failed — inspect in /mcp`, 'warn');
          app.activeView = 'customize';
          app.customizeTab = 'mcp';
          return;
        }
        await apiFetch(`/api/v1/mcp/${encodeURIComponent(targetName)}/toggle`, {
          method: 'POST',
          body: JSON.stringify({ enabled: true }),
        });
        await loadStatus();
        app.notify(`Enabled ${targetName} (Lazy Read-Only Semantic Preset)`, 'success');
      } catch (ex) {
        app.notify(ex instanceof Error ? ex.message : `Failed to test ${targetName}`, 'error');
        app.activeView = 'customize';
        app.customizeTab = 'mcp';
      }
      return;
    }
    app.activeView = 'customize';
    app.customizeTab = 'mcp';
  }
</script>

<div class="setup-page pane-scroll">
  <div class="setup-grid">
    <!-- Stage 1: Global API Pool & 0-Token Probe -->
    <section class="card stage-card">
      <div class="stage-head">
        <div>
          <span class="pill pill-main">Stage 1 • Global API Pool</span>
          <h2>Provider Keys &amp; 0-Token Probe</h2>
        </div>
        <div class="stage-acts">
          {#if keys.length > 0}
            <button class="btn btn-sm btn-crew" disabled={probing} onclick={() => probeSingleOrAll(false)}>
              {probing ? 'Probing…' : 'Probe Pool'}
            </button>
          {/if}
          {#if multiKey}
            <button class="btn btn-sm" onclick={() => (showAllPool = !showAllPool)}>
              {showAllPool ? 'Compact View' : `Show Ranking (${keys.length})`}
            </button>
          {/if}
        </div>
      </div>

      {#if keys.length === 1 && !showAllPool}
        {@const only = keys[0]}
        <div class="single-key-bar">
          <div>
            <strong>{only.provider}</strong>
            <span class="mono muted">{only.id}</span>
            {#if only.pinned}<span class="pill pill-main">Pinned</span>{/if}
          </div>
          {#if only.health}
            <span class="pill num" class:pill-rw={only.health.healthy} class:pill-danger={!only.health.healthy}>
              {only.health.healthy ? 'Healthy' : 'Unhealthy'} • {only.health.latency_ms}ms
            </span>
          {/if}
        </div>
      {:else if keys.length > 0 && showAllPool}
        <div class="tbl-wrap">
          <table class="ktbl">
            <thead>
              <tr>
                <th>ID</th>
                <th>Provider</th>
                <th>Model</th>
                <th class="num">Prio</th>
                <th>Health</th>
                <th class="num">TTFT/Probe</th>
                <th>State</th>
              </tr>
            </thead>
            <tbody>
              {#each keys as k (k.id)}
                <tr>
                  <td class="mono">{k.id}{#if k.pinned} <span class="pill pill-main">Pinned</span>{/if}</td>
                  <td>{k.provider}</td>
                  <td class="mono">{k.model || 'preset'}</td>
                  <td class="num">{k.priority}</td>
                  <td>
                    {#if k.health}
                      <span class="pill" class:pill-rw={k.health.healthy} class:pill-danger={!k.health.healthy}>
                        {k.health.healthy ? `OK (${k.health.status_code})` : `ERR ${k.health.status_code}`}
                      </span>
                    {:else}
                      <span class="pill pill-muted">Unprobed</span>
                    {/if}
                  </td>
                  <td class="num">{k.health ? `${k.health.latency_ms} ms` : '—'}</td>
                  <td>
                    <button class="btn btn-sm" onclick={() => toggleKeyEnabled(k)}>
                      {k.enabled ? 'Enabled' : 'Disabled'}
                    </button>
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}

      <form class="key-form" onsubmit={saveKey}>
        <div class="frow2">
          <label class="field">
            <span>Provider</span>
            <select class="select" value={provider} onchange={(e) => onProviderChange((e.currentTarget as HTMLSelectElement).value)}>
              {#each presets as p (p.name)}
                <option value={p.name}>{p.name}</option>
              {/each}
            </select>
          </label>
          <label class="field">
            <span>Model</span>
            <input class="input mono" list="preset-models" bind:value={model} placeholder={selectedPreset?.default_model || 'model'} />
            <datalist id="preset-models">
              {#each selectedPreset?.models ?? [] as m (m.Label)}
                <option value={m.Label}>{m.Detail}</option>
              {/each}
            </datalist>
          </label>
        </div>

        <div class="frow2">
          <label class="field">
            <span>API Key {provider === 'ollama' ? '(optional for local Ollama)' : ''}</span>
            <input class="input mono" type="password" bind:value={apiKey} placeholder="sk-..." autocomplete="off" />
          </label>
          <label class="field">
            <span>Base URL override (optional)</span>
            <input class="input mono" type="text" bind:value={baseUrl} placeholder={selectedPreset?.base_url || 'https://...'} />
          </label>
        </div>

        <div class="frow2">
          <label class="field">
            <span>Label (optional)</span>
            <input class="input" type="text" bind:value={label} placeholder="primary / backup" />
          </label>
          <label class="field">
            <span>Priority (1 = highest)</span>
            <input class="input num" type="number" min="1" max="100" bind:value={priority} />
          </label>
        </div>

        {#if formErr}<div class="err-box">{formErr}</div>{/if}
        {#if formMsg}<div class="ok-box">{formMsg}</div>{/if}

        <div class="form-btns">
          <button type="button" class="btn" disabled={probing || (!apiKey.trim() && provider !== 'ollama')} onclick={() => probeSingleOrAll(true)}>
            {probing ? 'Probing…' : 'Test 0-Token Probe'}
          </button>
          <button type="submit" class="btn btn-primary" disabled={saving || (!apiKey.trim() && provider !== 'ollama')}>
            {saving ? 'Saving…' : 'Save API Key'}
          </button>
        </div>
      </form>
    </section>

    <!-- Stage 2: Project Mount & .gitignore + Smart Serena -->
    <section class="card stage-card">
      <div class="stage-head">
        <div>
          <span class="pill pill-rw">Stage 2 • Project Mount</span>
          <h2>Workspace &amp; Substrate Readiness</h2>
        </div>
        <button class="btn btn-sm" onclick={() => (app.projectPickerOpen = true)}>
          Switch Project <span class="kbd">Ctrl+O</span>
        </button>
      </div>

      <div class="ws-meta">
        <div><strong>Workspace:</strong> <code class="mono">{status?.workspace ?? '—'}</code></div>
        <div><strong>Git Branch:</strong> <code class="mono">{status?.branch ?? '—'}</code></div>
        <div class="checks-row">
          <span class="pill" class:pill-rw={status?.has_tether_dir} class:pill-guarded={!status?.has_tether_dir}>
            {status?.has_tether_dir ? '✓ .tether/session.db' : '○ .tether/ pending'}
          </span>
          <span class="pill" class:pill-rw={status?.has_ignore} class:pill-guarded={!status?.has_ignore}>
            {status?.has_ignore ? '✓ .tether/ignore' : '○ .tether/ignore pending'}
          </span>
          <span class="pill" class:pill-rw={status?.has_gitignore} class:pill-muted={!status?.has_gitignore}>
            {status?.has_gitignore ? '✓ .gitignore' : '○ no .gitignore'}
          </span>
        </div>
      </div>

      <div class="stage2-acts">
        <button class="btn btn-success" disabled={initBusy} onclick={initProject}>
          {initBusy ? 'Initializing…' : status?.project_ready ? 'Re-Sync Project Mount (.tether/ + .gitignore)' : 'Initialize Project (.tether/ + .gitignore)'}
        </button>
        <button class="btn btn-primary" onclick={() => (app.activeView = 'chat')}>
          Enter Main Chat →
        </button>
      </div>

      {#if status?.serena_suggestion}
        {@const sug = status.serena_suggestion}
        <div class="card serena-card" class:recommended={sug.recommended}>
          <div class="serena-head">
            <span class="pill" class:pill-main={sug.recommended} class:pill-muted={!sug.recommended}>
              {sug.recommended ? '★ Smart Serena Recommendation' : 'Semantic MCP Status'}
            </span>
            <span class="num muted">{sug.file_count} files • {sug.dominant_stack || 'mixed'}</span>
          </div>
          <p class="serena-txt">
            {sug.recommendation || `Status: ${sug.serena_status} (preset: ${sug.suggested_preset || 'lazy_readonly'})`}
          </p>
          {#if (sug.reasons?.length ?? 0) > 0}
            <ul class="serena-reasons">
              {#each sug.reasons ?? [] as r}<li>{r}</li>{/each}
            </ul>
          {/if}
          {#if sug.recommended || sug.serena_status !== 'enabled'}
            <div class="serena-acts">
              <button class="btn btn-sm btn-crew" onclick={enableSerenaOneClick}>
                {sug.serena_status === 'disabled_draft' ? '1-Click Test & Enable Serena (Lazy Read-Only)' : 'Configure Serena in /mcp'}
              </button>
            </div>
          {/if}
        </div>
      {/if}

      {#if loading}
        <div class="muted">Refreshing status…</div>
      {/if}
    </section>
  </div>
</div>

<style>
  .setup-page {
    height: 100%;
    padding: 16px;
  }
  .setup-grid {
    max-width: 1120px;
    margin: 0 auto;
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(400px, 1fr));
    gap: 14px;
  }
  .stage-card {
    display: flex;
    flex-direction: column;
    gap: 12px;
  }
  .stage-head {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 10px;
  }
  .stage-head h2 {
    margin: 6px 0 0;
    font-size: 16px;
  }
  .stage-acts {
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
  }
  .single-key-bar {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 8px 10px;
    background: var(--mantle);
    border-radius: var(--radius-sm);
    border: 1px solid var(--border-subtle);
  }
  .tbl-wrap {
    overflow-x: auto;
  }
  .ktbl {
    width: 100%;
    border-collapse: collapse;
    font-size: 12px;
  }
  .ktbl th,
  .ktbl td {
    padding: 5px 6px;
    border-bottom: 1px solid var(--border-subtle);
    text-align: left;
  }
  .key-form {
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .frow2 {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 8px;
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: 4px;
    font-size: 12px;
    color: var(--subtext);
  }
  .form-btns,
  .stage2-acts {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
    justify-content: flex-end;
  }
  .stage2-acts {
    justify-content: flex-start;
  }
  .ws-meta {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 10px;
    background: var(--mantle);
    border-radius: var(--radius-sm);
    border: 1px solid var(--border-subtle);
    font-size: 12.5px;
  }
  .checks-row {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    margin-top: 2px;
  }
  .serena-card {
    background: var(--mantle);
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .serena-card.recommended {
    border-color: var(--main);
  }
  .serena-head {
    display: flex;
    justify-content: space-between;
    align-items: center;
  }
  .serena-txt {
    margin: 0;
    font-size: 12.5px;
  }
  .serena-reasons {
    margin: 0;
    padding-left: 18px;
    font-size: 12px;
    color: var(--subtext);
  }
  .err-box {
    color: var(--danger);
    font-size: 12px;
  }
  .ok-box {
    color: var(--rw);
    font-size: 12px;
  }
  .muted {
    color: var(--muted);
    font-size: 11.5px;
  }
</style>
