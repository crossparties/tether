<script lang="ts">
  import { onMount } from 'svelte';
  import CodeEditor from '../components/CodeEditor.svelte';
  import { apiFetch } from '../lib/api';
  import { app } from '../lib/store.svelte';
  import type {
    DecisionItem,
    ExternalMCPConfig,
    PatternItem,
    SerenaSuggestion,
    ServerTestResult,
    SkillView,
  } from '../lib/types';

  const SECRET_MASK = '**********';

  // --- MCP State ---
  let servers = $state<ExternalMCPConfig[]>([]);
  let activeConns = $state<string[]>([]);
  let loadErrors = $state<Record<string, string>>({});
  let serenaSug = $state<SerenaSuggestion | null>(null);
  let importSource = $state('all');
  let importScope = $state('global');

  // MCP Add / Edit Modal/Form
  let editingName = $state<string | null>(null); // null = new
  let formName = $state('');
  let formScope = $state('project');
  let formTransport = $state('stdio');
  let formPreset = $state('lazy_readonly');
  let formCommand = $state('');
  let formArgs = $state('');
  let formURL = $state('');
  let formEnvRows = $state<Array<{ key: string; value: string }>>([]);
  let formHeaderRows = $state<Array<{ key: string; value: string }>>([]);
  let formEnabledTools = $state<string[]>([]);
  let formDisabledTools = $state<string[]>([]);
  let testResult = $state<ServerTestResult | null>(null);
  let testedSignature = $state('');
  let mcpBusy = $state(false);
  let mcpErr = $state('');

  // --- Skills State ---
  let skills = $state<SkillView[]>([]);
  let selectedSkill = $state('');
  let skillDraft = $state('');
  let newSkillName = $state('');
  let skillBusy = $state(false);

  // --- Memory State ---
  let decisions = $state<DecisionItem[]>([]);
  let patterns = $state<PatternItem[]>([]);
  let memKind = $state<'decision' | 'pattern'>('decision');
  let decTopic = $state('architecture');
  let decText = $state('');
  let decReason = $state('');
  let patType = $state('convention');
  let patDesc = $state('');

  async function loadMCP() {
    const res = await apiFetch<{
      servers: ExternalMCPConfig[];
      active_connections?: string[];
      load_errors?: Record<string, string>;
      serena_suggestion?: SerenaSuggestion;
    }>('/api/v1/mcp').catch(() => null);
    if (res) {
      servers = res.servers ?? [];
      activeConns = res.active_connections ?? [];
      loadErrors = res.load_errors ?? {};
      serenaSug = res.serena_suggestion ?? null;
    }
  }

  async function loadSkills() {
    const res = await apiFetch<{ skills: SkillView[] }>('/api/v1/skills').catch(() => null);
    if (res) {
      skills = res.skills ?? [];
      if (!selectedSkill && skills.length > 0) {
        selectedSkill = skills[0].name;
        skillDraft = skills[0].content;
      }
    }
  }

  async function loadMemory() {
    const res = await apiFetch<{ decisions: DecisionItem[]; patterns: PatternItem[] }>('/api/v1/memory').catch(() => null);
    if (res) {
      decisions = res.decisions ?? [];
      patterns = res.patterns ?? [];
    }
  }

  onMount(() => {
    loadMCP();
    loadSkills();
    loadMemory();
  });

  function currentFormSignature(): string {
    return JSON.stringify({
      name: formName.trim().toLowerCase(),
      scope: formScope,
      transport: formTransport,
      command: formCommand.trim(),
      args: formArgs.trim(),
      url: formURL.trim(),
      env: formEnvRows,
      headers: formHeaderRows,
    });
  }

  function resetMCPForm() {
    editingName = null;
    formName = '';
    formScope = 'project';
    formTransport = 'stdio';
    formPreset = 'lazy_readonly';
    formCommand = '';
    formArgs = '';
    formURL = '';
    formEnvRows = [];
    formHeaderRows = [];
    formEnabledTools = [];
    formDisabledTools = [];
    testResult = null;
    testedSignature = '';
    mcpErr = '';
  }

  function editServer(s: ExternalMCPConfig) {
    editingName = s.name ?? '';
    formName = s.name ?? '';
    formScope = s.scope || 'project';
    formTransport = s.transport || 'stdio';
    formPreset = s.preset || 'lazy_readonly';
    formCommand = s.command || '';
    formArgs = (s.args ?? []).join(' ');
    formURL = s.url || '';
    formEnvRows = Object.entries(s.env ?? {}).map(([key, value]) => ({ key, value }));
    formHeaderRows = Object.entries(s.headers ?? {}).map(([key, value]) => ({ key, value }));
    formEnabledTools = [...(s.enabled_tools ?? [])];
    formDisabledTools = [...(s.disabled_tools ?? [])];
    testResult = null;
    testedSignature = '';
    mcpErr = '';
  }

  function buildMCPPayload(enabled: boolean): ExternalMCPConfig & { replace_secrets?: boolean } {
    const env: Record<string, string> = {};
    for (const r of formEnvRows) {
      if (r.key.trim()) env[r.key.trim()] = r.value;
    }
    const headers: Record<string, string> = {};
    for (const r of formHeaderRows) {
      if (r.key.trim()) headers[r.key.trim()] = r.value;
    }
    const args = formArgs.trim() ? formArgs.trim().split(/\s+/) : [];
    const isEdit = editingName !== null;
    return {
      name: formName.trim(),
      scope: formScope,
      transport: formTransport,
      preset: formPreset,
      command: formTransport === 'stdio' ? formCommand.trim() : undefined,
      args: formTransport === 'stdio' ? args : undefined,
      env: isEdit || Object.keys(env).length > 0 ? env : undefined,
      url: formTransport !== 'stdio' ? formURL.trim() : undefined,
      headers: isEdit || Object.keys(headers).length > 0 ? headers : undefined,
      enabled,
      enabled_tools: isEdit || formEnabledTools.length > 0 ? formEnabledTools : undefined,
      disabled_tools: isEdit || formDisabledTools.length > 0 ? formDisabledTools : undefined,
      ...(isEdit ? { replace_secrets: true } : {}),
    };
  }

  const isTestedForCurrentForm = $derived(
    Boolean(testResult?.ok) && testedSignature === currentFormSignature()
  );

  async function testMCPForm() {
    mcpErr = '';
    const trimmed = formName.trim();
    if (!trimmed) {
      mcpErr = 'Server name is required';
      return;
    }
    if (!editingName && servers.some((s) => (s.name ?? '').toLowerCase() === trimmed.toLowerCase())) {
      mcpErr = `Server "${trimmed}" conflicts case-insensitively with an existing server`;
      return;
    }
    mcpBusy = true;
    try {
      const payload = buildMCPPayload(false);
      const res = await apiFetch<ServerTestResult>('/api/v1/mcp/test', {
        method: 'POST',
        body: JSON.stringify(payload),
      });
      testResult = res;
      if (res.ok) {
        testedSignature = currentFormSignature();
      } else {
        mcpErr = res.error || 'MCP test failed';
      }
    } catch (ex) {
      mcpErr = ex instanceof Error ? ex.message : 'MCP test failed';
    } finally {
      mcpBusy = false;
    }
  }

  async function saveMCP(enable: boolean) {
    mcpErr = '';
    const trimmed = formName.trim();
    if (!trimmed) {
      mcpErr = 'Server name is required';
      return;
    }
    if (!editingName && servers.some((s) => (s.name ?? '').toLowerCase() === trimmed.toLowerCase())) {
      mcpErr = `Server "${trimmed}" already exists (case-insensitive)`;
      return;
    }
    if (enable && !isTestedForCurrentForm) {
      mcpErr = 'Run Test Connection successfully before saving & enabling';
      return;
    }
    mcpBusy = true;
    try {
      const payload = buildMCPPayload(enable);
      if (editingName) {
        await apiFetch(`/api/v1/mcp/${encodeURIComponent(editingName)}`, {
          method: 'PATCH',
          body: JSON.stringify(payload),
        });
      } else {
        await apiFetch('/api/v1/mcp', {
          method: 'POST',
          body: JSON.stringify(payload),
        });
      }
      resetMCPForm();
      await loadMCP();
      app.notify(enable ? `Saved & enabled MCP "${trimmed}"` : `Saved MCP draft "${trimmed}"`, 'success');
    } catch (ex) {
      mcpErr = ex instanceof Error ? ex.message : 'Failed to save MCP server';
    } finally {
      mcpBusy = false;
    }
  }

  async function testExistingServer(name: string) {
    mcpBusy = true;
    mcpErr = '';
    try {
      const res = await apiFetch<ServerTestResult>('/api/v1/mcp/test', {
        method: 'POST',
        body: JSON.stringify({ name }),
      });
      testResult = res;
      if (res.ok) {
        if (editingName && editingName.toLowerCase() === name.toLowerCase()) {
          testedSignature = currentFormSignature();
        }
        app.notify(`MCP "${name}" passed (${res.filtered_tool_count}/${res.raw_tool_count} tools, ${res.latency_ms}ms)`, 'success');
      } else {
        mcpErr = res.error || `Test failed for ${name}`;
      }
    } catch (ex) {
      mcpErr = ex instanceof Error ? ex.message : `Test failed for ${name}`;
    } finally {
      mcpBusy = false;
    }
  }

  async function toggleServer(s: ExternalMCPConfig) {
    if (!s.name) return;
    mcpBusy = true;
    mcpErr = '';
    try {
      await apiFetch(`/api/v1/mcp/${encodeURIComponent(s.name)}/toggle`, {
        method: 'POST',
        body: JSON.stringify({ enabled: !s.enabled }),
      });
      await loadMCP();
    } catch (ex) {
      mcpErr = ex instanceof Error ? ex.message : 'Toggle failed (test connection first before enabling)';
    } finally {
      mcpBusy = false;
    }
  }

  async function deleteServer(s: ExternalMCPConfig) {
    if (!s.name) return;
    await apiFetch(`/api/v1/mcp/${encodeURIComponent(s.name)}?scope=${encodeURIComponent(s.scope || '')}`, {
      method: 'DELETE',
    });
    await loadMCP();
  }

  async function importIDEs() {
    mcpBusy = true;
    try {
      const res = await apiFetch<{ imported?: Array<{ name: string }> }>('/api/v1/mcp/import', {
        method: 'POST',
        body: JSON.stringify({ source: importSource, scope: importScope }),
      });
      await loadMCP();
      app.notify(`Imported ${res.imported?.length ?? 0} MCP draft(s)`, 'info');
    } catch (ex) {
      mcpErr = ex instanceof Error ? ex.message : 'IDE import failed';
    } finally {
      mcpBusy = false;
    }
  }

  function toggleToolAllow(toolName: string) {
    if (formDisabledTools.includes(toolName)) {
      formDisabledTools = formDisabledTools.filter((t) => t !== toolName);
    } else {
      formDisabledTools = [...formDisabledTools, toolName];
    }
  }

  // --- Skills handlers ---
  function selectSkill(name: string) {
    selectedSkill = name;
    const found = skills.find((s) => s.name === name);
    skillDraft = found?.content ?? '';
  }

  async function saveSkill() {
    if (!selectedSkill.trim()) return;
    skillBusy = true;
    try {
      await apiFetch('/api/v1/skills', {
        method: 'PUT',
        body: JSON.stringify({ name: selectedSkill.trim(), content: skillDraft }),
      });
      await loadSkills();
      app.notify(`Saved skill ${selectedSkill}`, 'success');
    } catch (ex) {
      app.notify(ex instanceof Error ? ex.message : 'Failed to save skill', 'error');
    } finally {
      skillBusy = false;
    }
  }

  function createSkillDraft() {
    const clean = newSkillName.trim();
    if (!clean) return;
    selectedSkill = clean;
    skillDraft = `---\nname: ${clean}\ndescription: Custom workflow skill\n---\n\n# ${clean}\n\nInstructions for Crew / Main Agent.\n`;
    newSkillName = '';
  }

  // --- Memory handlers ---
  async function addMemory(e: Event) {
    e.preventDefault();
    try {
      if (memKind === 'decision') {
        await apiFetch('/api/v1/memory', {
          method: 'POST',
          body: JSON.stringify({ kind: 'decision', topic: decTopic, decision: decText, reason: decReason }),
        });
        decText = '';
        decReason = '';
      } else {
        await apiFetch('/api/v1/memory', {
          method: 'POST',
          body: JSON.stringify({ kind: 'pattern', pattern_type: patType, description: patDesc }),
        });
        patDesc = '';
      }
      await loadMemory();
      app.notify('Saved memory entry', 'success');
    } catch (ex) {
      app.notify(ex instanceof Error ? ex.message : 'Failed to save memory', 'error');
    }
  }

  async function deleteMemory(kind: 'decision' | 'pattern', id: number) {
    await apiFetch(`/api/v1/memory?kind=${kind}&id=${id}`, { method: 'DELETE' });
    await loadMemory();
  }
</script>

<div class="cust-page">
  <div class="cust-tabs">
    <button class="btn btn-sm" class:btn-primary={app.customizeTab === 'mcp'} onclick={() => (app.customizeTab = 'mcp')}>
      /mcp (External Clients)
    </button>
    <button class="btn btn-sm" class:btn-primary={app.customizeTab === 'skills'} onclick={() => (app.customizeTab = 'skills')}>
      /skills (.agents/skills)
    </button>
    <button class="btn btn-sm" class:btn-primary={app.customizeTab === 'memory'} onclick={() => (app.customizeTab = 'memory')}>
      /memory (Decisions &amp; Patterns)
    </button>
  </div>

  <div class="cust-body pane-scroll">
    {#if app.customizeTab === 'mcp'}
      <div class="mcp-grid">
        <div class="card mcp-col">
          <div class="sec-head">
            <h3>External MCP Servers</h3>
            <div class="imp-row">
              <select class="select sm-sel" bind:value={importSource}>
                <option value="all">all IDEs</option>
                <option value="agy">agy</option>
                <option value="claude">claude</option>
                <option value="cursor">cursor</option>
              </select>
              <select class="select sm-sel" bind:value={importScope}>
                <option value="global">global</option>
                <option value="project">project</option>
              </select>
              <button class="btn btn-sm" disabled={mcpBusy} onclick={importIDEs}>Import IDE</button>
            </div>
          </div>

          {#if serenaSug?.recommended}
            <div class="serena-banner">
              <strong>Serena Suggestion:</strong> {serenaSug.recommendation}
            </div>
          {/if}

          {#if mcpErr}<div class="err-txt">{mcpErr}</div>{/if}

          {#if servers.length === 0}
            <div class="empty-state">No external MCP servers configured yet.</div>
          {:else}
            <div class="srv-list">
              {#each servers as s (`${s.scope}:${s.name}`)}
                <div class="card srv-item">
                  <div class="srv-top">
                    <div class="srv-id">
                      <strong class="mono">{s.name}</strong>
                      <span class="pill pill-muted">{s.transport || 'stdio'}</span>
                      <span class="pill pill-muted">{s.scope || 'project'}</span>
                      <span class="pill" class:pill-crew={s.preset === 'lazy_readonly'} class:pill-plan={s.preset !== 'lazy_readonly'}>
                        {s.preset || 'lazy_readonly'}
                      </span>
                      {#if activeConns.includes(s.name ?? '')}
                        <span class="pill pill-rw">Active</span>
                      {/if}
                    </div>
                    <div class="srv-btns">
                      <button class="btn btn-sm" disabled={mcpBusy} onclick={() => testExistingServer(s.name!)}>Test</button>
                      <button class="btn btn-sm" onclick={() => editServer(s)}>Edit</button>
                      <button class="btn btn-sm" class:btn-success={!s.enabled} class:btn-warn={s.enabled} disabled={mcpBusy} onclick={() => toggleServer(s)}>
                        {s.enabled ? 'Disable' : 'Enable'}
                      </button>
                      <button class="btn btn-sm btn-danger" onclick={() => deleteServer(s)}>Delete</button>
                    </div>
                  </div>
                  <div class="srv-cmd mono">
                    {s.transport === 'http' || s.transport === 'sse' ? s.url : `${s.command || ''} ${(s.args ?? []).join(' ')}`}
                  </div>
                  {#if loadErrors[s.name ?? '']}
                    <div class="err-txt">{loadErrors[s.name ?? '']}</div>
                  {/if}
                </div>
              {/each}
            </div>
          {/if}
        </div>

        <div class="card mcp-col">
          <div class="sec-head">
            <h3>{editingName ? `Edit "${editingName}"` : 'Add External MCP Server'}</h3>
            {#if editingName}<button class="btn btn-sm" onclick={resetMCPForm}>New Server</button>{/if}
          </div>

          <div class="frow2">
            <label class="field">
              <span>Name (unique case-insensitive)</span>
              <input class="input mono" bind:value={formName} disabled={Boolean(editingName)} placeholder="serena" />
            </label>
            <label class="field">
              <span>Scope</span>
              <select class="select" bind:value={formScope}>
                <option value="project">project (.tether/mcp.json)</option>
                <option value="global">global (~/.config/tether/mcp.json)</option>
              </select>
            </label>
          </div>

          <div class="frow2">
            <label class="field">
              <span>Transport</span>
              <select class="select" bind:value={formTransport}>
                <option value="stdio">stdio</option>
                <option value="http">http</option>
                <option value="sse">sse</option>
              </select>
            </label>
            <label class="field">
              <span>Semantic Preset</span>
              <select class="select" bind:value={formPreset}>
                <option value="lazy_readonly">lazy_readonly (recommended)</option>
                <option value="full">full</option>
                <option value="custom">custom</option>
              </select>
            </label>
          </div>

          {#if formTransport === 'stdio'}
            <div class="frow2">
              <label class="field">
                <span>Command</span>
                <input class="input mono" bind:value={formCommand} placeholder="uvx / npx / binary" />
              </label>
              <label class="field">
                <span>Args (space-separated)</span>
                <input class="input mono" bind:value={formArgs} placeholder="--arg1 val1" />
              </label>
            </div>
          {:else}
            <label class="field">
              <span>Endpoint URL</span>
              <input class="input mono" bind:value={formURL} placeholder="http://127.0.0.1:8000/mcp" />
            </label>
          {/if}

          <div class="kv-sec">
            <div class="kv-head">
              <span>Env / Headers (secrets masked as <code>{SECRET_MASK}</code>)</span>
              <div class="kv-adds">
                <button class="btn btn-sm" type="button" onclick={() => (formEnvRows = [...formEnvRows, { key: '', value: '' }])}>+ Env</button>
                <button class="btn btn-sm" type="button" onclick={() => (formHeaderRows = [...formHeaderRows, { key: '', value: '' }])}>+ Header</button>
              </div>
            </div>
            {#each formEnvRows as row, idx}
              <div class="kv-row">
                <input class="input mono" placeholder="ENV_KEY" bind:value={row.key} />
                <input class="input mono" placeholder="value or **********" bind:value={row.value} />
                <button class="btn btn-sm btn-danger" onclick={() => (formEnvRows = formEnvRows.filter((_, i) => i !== idx))}>×</button>
              </div>
            {/each}
            {#each formHeaderRows as row, idx}
              <div class="kv-row">
                <input class="input mono" placeholder="Authorization" bind:value={row.key} />
                <input class="input mono" placeholder="Bearer ... or **********" bind:value={row.value} />
                <button class="btn btn-sm btn-danger" onclick={() => (formHeaderRows = formHeaderRows.filter((_, i) => i !== idx))}>×</button>
              </div>
            {/each}
          </div>

          <div class="mcp-form-acts">
            <button class="btn btn-crew" disabled={mcpBusy} onclick={testMCPForm}>
              {mcpBusy ? 'Testing…' : '1. Test Connection'}
            </button>
            <button class="btn" disabled={mcpBusy} onclick={() => saveMCP(false)}>
              Save Disabled Draft
            </button>
            <button class="btn btn-primary" disabled={mcpBusy || !isTestedForCurrentForm} onclick={() => saveMCP(true)}>
              2. Save &amp; Enable
            </button>
          </div>

          {#if testResult}
            <div class="test-card">
              <div class="test-head">
                <span class="pill" class:pill-rw={testResult.ok} class:pill-danger={!testResult.ok}>
                  {testResult.ok ? `Connected (${testResult.latency_ms}ms)` : 'Test Failed'}
                </span>
                <span class="num muted">{testResult.filtered_tool_count}/{testResult.raw_tool_count} tools active</span>
              </div>
              {#if testResult.warnings?.length}
                <div class="warn-txt">{testResult.warnings.join('; ')}</div>
              {/if}
              {#if testResult.tools?.length}
                <div class="tool-grid pane-scroll">
                  {#each testResult.tools as t (t.name)}
                    {@const toolKey = t.original_name || t.name}
                    <label class="tool-chk" title={t.name}>
                      <input
                        type="checkbox"
                        checked={!formDisabledTools.includes(toolKey)}
                        onchange={() => toggleToolAllow(toolKey)}
                      />
                      <span class="mono">{toolKey}</span>
                    </label>
                  {/each}
                </div>
              {/if}
            </div>
          {/if}
        </div>
      </div>
    {:else if app.customizeTab === 'skills'}
      <div class="skills-split">
        <div class="card sk-list">
          <form
            class="sk-new"
            onsubmit={(e) => {
              e.preventDefault();
              createSkillDraft();
            }}
          >
            <input class="input mono" placeholder="new-skill-name" bind:value={newSkillName} />
            <button class="btn btn-sm btn-primary" type="submit">New</button>
          </form>
          <div class="sk-items pane-scroll">
            {#each skills as sk (sk.name)}
              <button class="sk-btn" class:active={selectedSkill === sk.name} onclick={() => selectSkill(sk.name)}>
                <strong class="mono">{sk.name}</strong>
                {#if sk.description}<span class="muted">{sk.description}</span>{/if}
              </button>
            {/each}
          </div>
        </div>
        <div class="card sk-edit">
          <div class="sec-head">
            <code class="mono">{selectedSkill ? `.agents/skills/${selectedSkill}/SKILL.md` : 'Select or create a skill'}</code>
            <button class="btn btn-sm btn-primary" disabled={skillBusy || !selectedSkill} onclick={saveSkill}>
              {skillBusy ? 'Saving…' : 'Save SKILL.md'}
            </button>
          </div>
          <div class="sk-cm">
            <CodeEditor value={skillDraft} filename="SKILL.md" minHeight="300px" onchange={(v) => (skillDraft = v)} />
          </div>
        </div>
      </div>
    {:else}
      <div class="mem-grid">
        <form class="card mem-form" onsubmit={addMemory}>
          <h3>Add Memory Entry</h3>
          <div class="seg">
            <button type="button" class="btn btn-sm" class:btn-primary={memKind === 'decision'} onclick={() => (memKind = 'decision')}>Decision</button>
            <button type="button" class="btn btn-sm" class:btn-primary={memKind === 'pattern'} onclick={() => (memKind = 'pattern')}>Learned Pattern</button>
          </div>
          {#if memKind === 'decision'}
            <input class="input" bind:value={decTopic} placeholder="Topic (e.g. architecture)" />
            <input class="input" bind:value={decText} placeholder="Decision statement" required />
            <input class="input" bind:value={decReason} placeholder="Rationale (optional)" />
          {:else}
            <input class="input" bind:value={patType} placeholder="Pattern type (e.g. convention)" />
            <input class="input" bind:value={patDesc} placeholder="Pattern description" required />
          {/if}
          <button class="btn btn-primary" type="submit">Save to .tether/session.db</button>
        </form>

        <div class="card mem-col">
          <h3>Decisions ({decisions.length})</h3>
          {#each decisions as d (d.id)}
            <div class="mem-item">
              <div>
                <span class="pill pill-main">{d.topic}</span>
                <strong>{d.decision}</strong>
                {#if d.reason}<div class="muted">{d.reason}</div>{/if}
              </div>
              <button class="btn btn-sm btn-danger" onclick={() => deleteMemory('decision', d.id)}>Delete</button>
            </div>
          {/each}
        </div>

        <div class="card mem-col">
          <h3>Learned Patterns ({patterns.length})</h3>
          {#each patterns as p (p.id)}
            <div class="mem-item">
              <div>
                <span class="pill pill-crew">{p.type}</span>
                <span>{p.description}</span>
              </div>
              <button class="btn btn-sm btn-danger" onclick={() => deleteMemory('pattern', p.id)}>Delete</button>
            </div>
          {/each}
        </div>
      </div>
    {/if}
  </div>
</div>

<style>
  .cust-page {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
  }
  .cust-tabs {
    display: flex;
    gap: 6px;
    padding: 10px 16px;
    background: var(--mantle);
    border-bottom: 1px solid var(--border-subtle);
  }
  .cust-body {
    flex: 1;
    padding: 16px;
  }
  .mcp-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(420px, 1fr));
    gap: 14px;
  }
  .mcp-col {
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .sec-head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .sec-head h3 { margin: 0; font-size: 14.5px; }
  .imp-row { display: flex; gap: 5px; align-items: center; }
  .sm-sel { width: auto; padding: 4px 6px; font-size: 12px; }
  .serena-banner {
    padding: 8px 10px;
    border-radius: var(--radius-sm);
    background: rgba(203, 166, 247, 0.12);
    border: 1px solid rgba(203, 166, 247, 0.4);
    font-size: 12px;
  }
  .srv-list { display: flex; flex-direction: column; gap: 8px; }
  .srv-item { background: var(--mantle); padding: 10px; display: flex; flex-direction: column; gap: 6px; }
  .srv-top { display: flex; justify-content: space-between; gap: 8px; flex-wrap: wrap; }
  .srv-id, .srv-btns { display: flex; align-items: center; gap: 5px; flex-wrap: wrap; }
  .srv-cmd { font-size: 11.5px; color: var(--subtext); word-break: break-all; }
  .frow2 { display: grid; grid-template-columns: 1fr 1fr; gap: 8px; }
  .field { display: flex; flex-direction: column; gap: 4px; font-size: 12px; color: var(--subtext); }
  .kv-sec { display: flex; flex-direction: column; gap: 6px; }
  .kv-head { display: flex; justify-content: space-between; align-items: center; font-size: 12px; color: var(--subtext); }
  .kv-adds { display: flex; gap: 4px; }
  .kv-row { display: grid; grid-template-columns: 1fr 1fr auto; gap: 6px; }
  .mcp-form-acts { display: flex; gap: 8px; flex-wrap: wrap; }
  .test-card { background: var(--mantle); border: 1px solid var(--border-subtle); border-radius: var(--radius-sm); padding: 8px; display: flex; flex-direction: column; gap: 6px; }
  .test-head { display: flex; justify-content: space-between; align-items: center; }
  .tool-grid { max-height: 150px; display: grid; grid-template-columns: repeat(auto-fill, minmax(180px, 1fr)); gap: 4px; }
  .tool-chk { display: flex; align-items: center; gap: 5px; font-size: 11.5px; }
  .skills-split { display: grid; grid-template-columns: 270px 1fr; gap: 12px; height: calc(100dvh - 140px); }
  .sk-list, .sk-edit { display: flex; flex-direction: column; gap: 8px; min-height: 0; }
  .sk-new { display: flex; gap: 6px; }
  .sk-items { flex: 1; display: flex; flex-direction: column; gap: 4px; }
  .sk-btn { text-align: left; padding: 7px 9px; border-radius: var(--radius-sm); border: 1px solid var(--border-subtle); background: var(--mantle); display: flex; flex-direction: column; gap: 2px; }
  .sk-btn.active { border-color: var(--main); background: var(--overlay); }
  .sk-cm { flex: 1; min-height: 0; }
  .mem-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(300px, 1fr)); gap: 12px; }
  .mem-form, .mem-col { display: flex; flex-direction: column; gap: 8px; }
  .mem-form h3, .mem-col h3 { margin: 0; font-size: 14.5px; }
  .seg { display: flex; gap: 6px; }
  .mem-item { display: flex; justify-content: space-between; align-items: flex-start; gap: 8px; padding: 8px; background: var(--mantle); border-radius: var(--radius-sm); border: 1px solid var(--border-subtle); }
  .err-txt { color: var(--danger); font-size: 12px; }
  .warn-txt { color: var(--guarded); font-size: 12px; }
  .muted { color: var(--muted); font-size: 11.5px; }
</style>
