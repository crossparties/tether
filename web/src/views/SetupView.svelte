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
  let showGuide = $state(true);
  let manualStep = $state<number | null>(null);
  let apiKeyInputEl = $state<HTMLInputElement | null>(null);

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
  const hasSavedKey = $derived((status?.has_global_keys ?? false) || keys.length > 0);
  const isProjectReady = $derived(status?.project_ready ?? false);

  // Quick provider guide cards for first-time users
  const providerRecommendations = [
    {
      id: 'deepseek',
      title: 'DeepSeek',
      badge: 'Direkomendasikan',
      desc: 'Cepat, hemat biaya, & otomatis mendukung 90% diskon KV Prompt Cache.',
      keyHint: 'Butuh DEEPSEEK_API_KEY (sk-...)',
    },
    {
      id: 'openrouter',
      title: 'OpenRouter',
      badge: 'Multi-Model',
      desc: 'Satu API key untuk akses Claude, DeepSeek, GPT-4o, dan Gemini.',
      keyHint: 'Butuh OPENROUTER_API_KEY (sk-or-...)',
    },
    {
      id: 'openai',
      title: 'OpenAI / Gemini / Groq',
      badge: 'Cloud Populer',
      desc: 'Gunakan API key resmi dari OpenAI (gpt-4o), Google Gemini, atau Groq.',
      keyHint: 'Pilih provider di dropdown & isi API key',
    },
    {
      id: 'ollama',
      title: 'Ollama (Lokal)',
      badge: 'Gratis • Tanpa Key',
      desc: 'Jalankan LLM lokal di mesin Anda (localhost:11434) tanpa API key berbayar.',
      keyHint: 'Tidak wajib isi API Key — langsung klik Save',
    },
  ];

  // Auto-detect current onboarding step (1..4) unless user clicked a step pill
  const autoStep = $derived.by(() => {
    if (!hasSavedKey) {
      if (!apiKey.trim() && provider !== 'ollama') return 1;
      return 2;
    }
    if (!isProjectReady) return 3;
    return 4;
  });

  const activeStep = $derived(manualStep ?? autoStep);

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
    manualStep = null;
  }

  function pickRecommendedProvider(id: string) {
    onProviderChange(id);
    if (id !== 'ollama') {
      setTimeout(() => apiKeyInputEl?.focus(), 30);
    }
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

  async function saveKey(e?: Event) {
    if (e) e.preventDefault();
    saving = true;
    formErr = '';
    formMsg = '';
    try {
      await apiFetch('/api/v1/keys', {
        method: 'PUT',
        body: JSON.stringify({
          provider,
          api_key: apiKey.trim() || (provider === 'ollama' ? 'ollama-local' : ''),
          label: label.trim() || undefined,
          base_url: baseUrl.trim() || undefined,
          model: model.trim() || undefined,
          priority,
          enabled: true,
        }),
      });
      apiKey = '';
      manualStep = null;
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
      manualStep = null;
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
  <!-- Interactive First-Time Setup Guided Assistant -->
  <section class="card guide-banner">
    <div class="guide-top">
      <div class="guide-title-row">
        <span class="pill pill-main">Panduan Setup Pertama Kali</span>
        <h3>
          {#if activeStep === 1}
            Langkah 1 dari 4: Pilih Provider AI &amp; Masukkan API Key
          {:else if activeStep === 2}
            Langkah 2 dari 4: Uji &amp; Simpan API Key Anda
          {:else if activeStep === 3}
            Langkah 3 dari 4: Inisialisasi Folder Proyek (.tether/)
          {:else}
            Langkah 4 dari 4: Setup Selesai — Siap Mulai Coding!
          {/if}
        </h3>
      </div>
      <button class="btn btn-sm" onclick={() => (showGuide = !showGuide)}>
        {showGuide ? 'Sembunyikan Panduan' : 'Tampilkan Panduan'}
      </button>
    </div>

    <!-- 4-Step Interactive Stepper Bar -->
    <div class="stepper-row">
      <button
        type="button"
        class="step-chip"
        class:active={activeStep === 1}
        class:done={hasSavedKey || apiKey.trim().length > 0 || provider === 'ollama'}
        onclick={() => (manualStep = 1)}
      >
        <span class="step-num">{hasSavedKey ? '✓' : '1'}</span>
        <span>1. Pilih Provider AI</span>
      </button>

      <button
        type="button"
        class="step-chip"
        class:active={activeStep === 2}
        class:done={hasSavedKey}
        onclick={() => (manualStep = 2)}
      >
        <span class="step-num">{hasSavedKey ? '✓' : '2'}</span>
        <span>2. Simpan API Key</span>
      </button>

      <button
        type="button"
        class="step-chip"
        class:active={activeStep === 3}
        class:done={isProjectReady}
        onclick={() => (manualStep = 3)}
      >
        <span class="step-num">{isProjectReady ? '✓' : '3'}</span>
        <span>3. Initialize Project</span>
      </button>

      <button
        type="button"
        class="step-chip"
        class:active={activeStep === 4}
        class:done={hasSavedKey && isProjectReady}
        onclick={() => (manualStep = 4)}
      >
        <span class="step-num">{hasSavedKey && isProjectReady ? '✓' : '4'}</span>
        <span>4. Masuk Main Chat</span>
      </button>
    </div>

    {#if showGuide}
      <div class="guide-body">
        {#if activeStep === 1}
          <p class="guide-lead">
            <strong>Apa yang harus dipilih sekarang?</strong> Klik salah satu kartu provider AI di bawah sesuai akun yang Anda miliki, lalu ketik/tempel <strong>API Key</strong> di formulir <em>Stage 1</em>:
          </p>
          <div class="rec-grid">
            {#each providerRecommendations as rec (rec.id)}
              <button
                type="button"
                class="rec-card"
                class:selected={provider === rec.id}
                onclick={() => pickRecommendedProvider(rec.id)}
              >
                <div class="rec-head">
                  <strong>{rec.title}</strong>
                  <span class="pill" class:pill-main={provider === rec.id} class:pill-muted={provider !== rec.id}>
                    {rec.badge}
                  </span>
                </div>
                <p class="rec-desc">{rec.desc}</p>
                <span class="rec-hint mono">{rec.keyHint}</span>
              </button>
            {/each}
          </div>
        {:else if activeStep === 2}
          <div class="guide-action-box">
            <div>
              <strong>Apa yang harus diklik selanjutnya?</strong>
              <p class="guide-lead">
                Provider <code>{provider}</code> sudah dipilih. Klik tombol <strong>Test 0-Token Probe</strong> untuk memastikan key valid tanpa memotong kuota token, lalu klik tombol <strong>Save API Key</strong> untuk menyimpan ke <code>~/.config/tether/credentials.json</code>.
              </p>
            </div>
            <div class="guide-ctas">
              <button
                type="button"
                class="btn"
                disabled={probing || (!apiKey.trim() && provider !== 'ollama')}
                onclick={() => probeSingleOrAll(true)}
              >
                {probing ? 'Probing…' : '1. Test 0-Token Probe'}
              </button>
              <button
                type="button"
                class="btn btn-primary"
                disabled={saving || (!apiKey.trim() && provider !== 'ollama')}
                onclick={() => saveKey()}
              >
                {saving ? 'Saving…' : '2. Save API Key Sekarang →'}
              </button>
            </div>
          </div>
        {:else if activeStep === 3}
          <div class="guide-action-box">
            <div>
              <strong>✓ Stage 1 Selesai! Sekarang lanjut ke Stage 2 (Project Mount):</strong>
              <p class="guide-lead">
                Pastikan folder workspace Anda sudah sesuai (<code>{status?.workspace ?? '—'}</code>). Jika ingin ganti folder, klik <strong>Switch Project (Ctrl+O)</strong>. Setelah sesuai, klik tombol hijau <strong>Initialize Project (.tether/ + .gitignore)</strong> agar Tether menyiapkan database sesi, Pure-Go RepoMap, dan proteksi otomatis file rahasia (<code>.env</code>, <code>*.pem</code>).
              </p>
            </div>
            <div class="guide-ctas">
              <button type="button" class="btn" onclick={() => (app.projectPickerOpen = true)}>
                Pilih Folder Lain (Ctrl+O)
              </button>
              <button type="button" class="btn btn-success" disabled={initBusy} onclick={initProject}>
                {initBusy ? 'Initializing…' : 'Klik: Initialize Project (.tether/) Sekarang →'}
              </button>
            </div>
          </div>
        {:else}
          <div class="guide-action-box done-box">
            <div>
              <strong>🎉 Setup Pertama Kali Selesai! Semua Stage Siap Digunakan.</strong>
              <p class="guide-lead">
                API Key Pool sudah aktif dan proyek <code>{status?.workspace ?? ''}</code> sudah terinisialisasi. Klik <strong>Enter Main Chat →</strong> untuk mulai memberi instruksi ke AI, gunakan <code>@namafile</code> untuk melampirkan file, atau klik <code>/btw</code> untuk bertanya tanpa merusak cache.
              </p>
            </div>
            <div class="guide-ctas">
              <button type="button" class="btn btn-primary" onclick={() => (app.activeView = 'chat')}>
                Enter Main Chat Sekarang →
              </button>
            </div>
          </div>
        {/if}
      </div>
    {/if}
  </section>

  <div class="setup-grid">
    <!-- Stage 1: Global API Pool & 0-Token Probe -->
    <section class="card stage-card" class:guide-focus={activeStep === 1 || activeStep === 2}>
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
            <span>1. Pilih Provider AI</span>
            <select class="select" value={provider} onchange={(e) => onProviderChange((e.currentTarget as HTMLSelectElement).value)}>
              {#each presets as p (p.name)}
                <option value={p.name}>{p.name}</option>
              {/each}
            </select>
          </label>
          <label class="field">
            <span>2. Pilih Model (otomatis terisi default)</span>
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
            <span>3. Masukkan API Key {provider === 'ollama' ? '(opsional untuk Ollama lokal)' : '(Wajib diisi)'}</span>
            <input
              bind:this={apiKeyInputEl}
              class="input mono"
              class:input-highlight={activeStep === 1 && provider !== 'ollama'}
              type="password"
              bind:value={apiKey}
              placeholder={provider === 'ollama' ? 'kosongkan jika lokal' : 'sk-...'}
              autocomplete="off"
            />
          </label>
          <label class="field">
            <span>Base URL override (opsional — biarkan kosong untuk default)</span>
            <input class="input mono" type="text" bind:value={baseUrl} placeholder={selectedPreset?.base_url || 'https://...'} />
          </label>
        </div>

        <div class="frow2">
          <label class="field">
            <span>Label (opsional, misal: primary / backup)</span>
            <input class="input" type="text" bind:value={label} placeholder="primary / backup" />
          </label>
          <label class="field">
            <span>Priority (1 = utama, 2+ = cadangan failover)</span>
            <input class="input num" type="number" min="1" max="100" bind:value={priority} />
          </label>
        </div>

        {#if formErr}<div class="err-box">{formErr}</div>{/if}
        {#if formMsg}<div class="ok-box">{formMsg}</div>{/if}

        <div class="form-btns">
          <button type="button" class="btn" disabled={probing || (!apiKey.trim() && provider !== 'ollama')} onclick={() => probeSingleOrAll(true)}>
            {probing ? 'Probing…' : 'Test 0-Token Probe'}
          </button>
          <button
            type="submit"
            class="btn btn-primary"
            class:cta-pulse={activeStep === 2}
            disabled={saving || (!apiKey.trim() && provider !== 'ollama')}
          >
            {saving ? 'Saving…' : 'Save API Key'}
          </button>
        </div>
      </form>
    </section>

    <!-- Stage 2: Project Mount & .gitignore + Smart Serena -->
    <section class="card stage-card" class:guide-focus={activeStep === 3 || activeStep === 4}>
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
            {status?.has_tether_dir ? '✓ .tether/session.db' : '○ .tether/ belum diinisialisasi'}
          </span>
          <span class="pill" class:pill-rw={status?.has_ignore} class:pill-guarded={!status?.has_ignore}>
            {status?.has_ignore ? '✓ .tether/ignore' : '○ .tether/ignore belum dibuat'}
          </span>
          <span class="pill" class:pill-rw={status?.has_gitignore} class:pill-muted={!status?.has_gitignore}>
            {status?.has_gitignore ? '✓ .gitignore' : '○ no .gitignore'}
          </span>
        </div>
      </div>

      <div class="stage2-acts">
        <button
          class="btn btn-success"
          class:cta-pulse={activeStep === 3}
          disabled={initBusy}
          onclick={initProject}
        >
          {initBusy ? 'Initializing…' : status?.project_ready ? 'Re-Sync Project Mount (.tether/ + .gitignore)' : '1. Klik: Initialize Project (.tether/ + .gitignore)'}
        </button>
        <button
          class="btn btn-primary"
          class:cta-pulse={activeStep === 4}
          onclick={() => (app.activeView = 'chat')}
        >
          2. Enter Main Chat →
        </button>
      </div>

      {#if status?.serena_suggestion}
        {@const sug = status.serena_suggestion}
        <div class="card serena-card" class:recommended={sug.recommended}>
          <div class="serena-head">
            <span class="pill" class:pill-main={sug.recommended} class:pill-muted={!sug.recommended}>
              {sug.recommended ? '★ Smart Serena Recommendation (Opsional)' : 'Semantic MCP Status'}
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
    display: flex;
    flex-direction: column;
    gap: 14px;
  }
  .guide-banner {
    max-width: 1120px;
    width: 100%;
    margin: 0 auto;
    display: flex;
    flex-direction: column;
    gap: 10px;
    border-color: var(--main);
    background: linear-gradient(180deg, rgba(203, 166, 247, 0.09) 0%, var(--surface0) 100%);
  }
  .guide-top {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
  }
  .guide-title-row {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
  }
  .guide-title-row h3 {
    margin: 0;
    font-size: 15px;
    color: var(--text);
  }
  .stepper-row {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
    gap: 8px;
  }
  .step-chip {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 7px 10px;
    border-radius: var(--radius-sm);
    background: var(--mantle);
    border: 1px solid var(--border-subtle);
    color: var(--subtext);
    font-size: 12px;
    font-weight: 600;
    cursor: pointer;
    text-align: left;
    transition: border-color 0.15s, background 0.15s;
  }
  .step-chip.active {
    border-color: var(--main);
    color: var(--text);
    background: rgba(203, 166, 247, 0.14);
  }
  .step-chip.done {
    border-color: rgba(166, 227, 161, 0.5);
    color: var(--rw);
  }
  .step-num {
    width: 20px;
    height: 20px;
    border-radius: 50%;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    font-size: 11px;
    font-weight: 700;
    background: var(--surface0);
    color: var(--text);
    flex-shrink: 0;
  }
  .step-chip.done .step-num {
    background: rgba(166, 227, 161, 0.2);
    color: var(--rw);
  }
  .step-chip.active .step-num {
    background: var(--main);
    color: var(--crust);
  }
  .guide-body {
    display: flex;
    flex-direction: column;
    gap: 10px;
    padding-top: 4px;
    border-top: 1px solid var(--border-subtle);
  }
  .guide-lead {
    margin: 4px 0 0;
    font-size: 12.5px;
    color: var(--subtext);
    line-height: 1.45;
  }
  .rec-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
    gap: 8px;
  }
  .rec-card {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 5px;
    padding: 10px;
    border-radius: var(--radius-sm);
    background: var(--mantle);
    border: 1px solid var(--border-subtle);
    color: var(--text);
    text-align: left;
    cursor: pointer;
    transition: border-color 0.15s, transform 0.1s;
  }
  .rec-card:hover {
    border-color: var(--border);
  }
  .rec-card.selected {
    border-color: var(--main);
    background: rgba(203, 166, 247, 0.11);
  }
  .rec-head {
    width: 100%;
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 6px;
    font-size: 13px;
  }
  .rec-desc {
    margin: 0;
    font-size: 11.5px;
    color: var(--subtext);
    line-height: 1.35;
  }
  .rec-hint {
    font-size: 11px;
    color: var(--plan);
  }
  .guide-action-box {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 12px;
    flex-wrap: wrap;
    padding: 10px 12px;
    border-radius: var(--radius-sm);
    background: var(--mantle);
    border: 1px solid var(--border-subtle);
  }
  .guide-action-box.done-box {
    border-color: rgba(166, 227, 161, 0.55);
    background: rgba(166, 227, 161, 0.08);
  }
  .guide-ctas {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
    flex-shrink: 0;
  }
  .setup-grid {
    max-width: 1120px;
    width: 100%;
    margin: 0 auto;
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(400px, 1fr));
    gap: 14px;
  }
  .stage-card {
    display: flex;
    flex-direction: column;
    gap: 12px;
    transition: border-color 0.2s, box-shadow 0.2s;
  }
  .stage-card.guide-focus {
    border-color: var(--border);
    box-shadow: 0 0 0 1px rgba(180, 190, 254, 0.25);
  }
  .input-highlight {
    border-color: var(--main) !important;
    box-shadow: 0 0 0 2px rgba(203, 166, 247, 0.22);
  }
  .cta-pulse {
    box-shadow: 0 0 0 2px rgba(203, 166, 247, 0.35);
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
