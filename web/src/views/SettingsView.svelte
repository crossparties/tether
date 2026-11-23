<script lang="ts">
  import { onMount } from 'svelte';
  import { apiFetch } from '../lib/api';
  import { app } from '../lib/store.svelte';
  import type {
    ApprovalGrant,
    DoctorReport,
    GovernanceMode,
    SettingsResponse,
  } from '../lib/types';

  let mode = $state<GovernanceMode>('code');
  let provider = $state('');
  let model = $state('');
  let contextWindow = $state('128000');
  let condenseRatio = $state('0.72');
  let condenseMaxEvents = $state('40');
  let maxSteps = $state('35');
  let delegateCriticalDefault = $state(false);
  let saving = $state(false);

  let grants = $state<ApprovalGrant[]>([]);
  let doctorReport = $state<DoctorReport | null>(null);
  let doctorBusy = $state(false);

  async function loadAll() {
    const [setRes, grantsRes] = await Promise.all([
      apiFetch<SettingsResponse>('/api/v1/settings').catch(() => null),
      apiFetch<{ grants: ApprovalGrant[] }>('/api/v1/grants').catch(() => null),
    ]);
    if (setRes) {
      mode = setRes.mode;
      provider = setRes.provider ?? '';
      model = setRes.model ?? '';
      contextWindow = setRes.project_settings?.context_window ?? String(setRes.context_window ?? 128000);
      condenseRatio = setRes.project_settings?.condense_threshold_ratio ?? '0.72';
      condenseMaxEvents = setRes.project_settings?.condense_max_events ?? '40';
      maxSteps = setRes.project_settings?.max_steps ?? '35';
      delegateCriticalDefault =
        Boolean(setRes.global?.delegate_critical_default) ||
        setRes.project_settings?.delegate_critical === 'true';
    }
    if (grantsRes) {
      grants = grantsRes.grants ?? [];
    }
  }

  onMount(loadAll);

  async function saveSettings(e: Event) {
    e.preventDefault();
    saving = true;
    try {
      await apiFetch('/api/v1/settings', {
        method: 'PUT',
        body: JSON.stringify({
          mode,
          provider: provider || undefined,
          model: model || undefined,
          project_settings: {
            context_window: contextWindow,
            condense_threshold_ratio: condenseRatio,
            condense_max_events: condenseMaxEvents,
            max_steps: maxSteps,
            delegate_critical: delegateCriticalDefault ? 'true' : 'false',
          },
          global: {
            theme: app.theme,
            delegate_critical_default: delegateCriticalDefault,
          },
        }),
      });
      app.mode = mode;
      await app.refreshAll();
      app.notify('Saved settings & EpisodicCondenser thresholds', 'success');
    } catch (ex) {
      app.notify(ex instanceof Error ? ex.message : 'Failed to save settings', 'error');
    } finally {
      saving = false;
    }
  }

  async function setTheme(next: 'mocha' | 'latte') {
    app.applyTheme(next);
    await apiFetch('/api/v1/settings', {
      method: 'PUT',
      body: JSON.stringify({ global: { theme: next } }),
    }).catch(() => {});
  }

  async function revokeGrants() {
    const res = await apiFetch<{ revoked: number }>('/api/v1/grants', { method: 'DELETE' });
    await loadAll();
    app.notify(`Revoked ${res.revoked} approval grant(s)`, 'info');
  }

  async function runDoctor(fix: boolean) {
    doctorBusy = true;
    try {
      doctorReport = await apiFetch<DoctorReport>('/api/v1/doctor', {
        method: 'POST',
        body: JSON.stringify({ fix }),
      });
    } catch (ex) {
      app.notify(ex instanceof Error ? ex.message : 'Doctor check failed', 'error');
    } finally {
      doctorBusy = false;
    }
  }
</script>

<div class="settings-page pane-scroll">
  <div class="settings-grid">
    <form class="card s-card" onsubmit={saveSettings}>
      <h2>Theme, Condenser &amp; Delegation</h2>

      <div class="theme-row">
        <span>UI Theme</span>
        <div class="seg">
          <button type="button" class="btn btn-sm" class:btn-primary={app.theme === 'mocha'} onclick={() => setTheme('mocha')}>
            Catppuccin Mocha (Dark)
          </button>
          <button type="button" class="btn btn-sm" class:btn-primary={app.theme === 'latte'} onclick={() => setTheme('latte')}>
            Catppuccin Latte (Light)
          </button>
        </div>
      </div>

      <div class="frow2">
        <label class="field">
          <span>Governance Mode</span>
          <select class="select" bind:value={mode}>
            <option value="code">code (unrestricted workspace RW)</option>
            <option value="plan">plan (.tether/PLAN.md + docs only)</option>
            <option value="review">review (read-only audit)</option>
          </select>
        </label>
        <label class="field">
          <span>Active Provider</span>
          <select
            class="select"
            value={provider}
            onchange={(e) => {
              const nextProv = (e.currentTarget as HTMLSelectElement).value;
              provider = nextProv;
              const preset = (app.setupStatus?.presets ?? []).find((p) => p.name === nextProv);
              if (preset?.default_model) {
                model = preset.default_model;
              }
            }}
          >
            {#each app.setupStatus?.presets ?? [] as p (p.name)}
              <option value={p.name}>{p.name}</option>
            {/each}
          </select>
        </label>
      </div>

      <div class="frow2">
        <label class="field">
          <span>Active Model</span>
          <input class="input mono" list="settings-models" bind:value={model} />
          <datalist id="settings-models">
            {#each (app.setupStatus?.presets ?? []).find((p) => p.name === provider)?.models ?? [] as m (m.Label)}
              <option value={m.Label}>{m.Detail}</option>
            {/each}
          </datalist>
        </label>
        <label class="field">
          <span>Context Window (tokens)</span>
          <input class="input num" bind:value={contextWindow} />
        </label>
      </div>

      <div class="frow2">
        <label class="field">
          <span>Condenser Threshold Ratio (0–1)</span>
          <input class="input num" bind:value={condenseRatio} />
        </label>
        <label class="field">
          <span>Condenser Max Events</span>
          <input class="input num" bind:value={condenseMaxEvents} />
        </label>
      </div>

      <div class="frow2">
        <label class="field">
          <span>Max Turn Steps</span>
          <input class="input num" bind:value={maxSteps} />
        </label>
        <label class="chk-field">
          <input type="checkbox" bind:checked={delegateCriticalDefault} />
          <span>Default Crew delegation for routine non-guarded writes</span>
        </label>
      </div>

      <div class="acts">
        <button class="btn btn-primary" type="submit" disabled={saving}>
          {saving ? 'Saving…' : 'Save Settings'}
        </button>
      </div>
    </form>

    <div class="right-col">
      <section class="card s-card">
        <div class="sec-head">
          <h2>Active Approval Grants ({grants.length})</h2>
          <button class="btn btn-sm btn-danger" disabled={grants.length === 0} onclick={revokeGrants}>
            Revoke All Grants
          </button>
        </div>
        {#if grants.length === 0}
          <div class="empty-state">No active session/folder approval grants.</div>
        {:else}
          <div class="grant-list">
            {#each grants as g (g.id)}
              <div class="grant-row">
                <span class="pill pill-guarded">{g.kind}</span>
                <span class="mono">{g.path_scope || 'all paths'}</span>
                <span class="num muted">exp {g.expires_at}</span>
              </div>
            {/each}
          </div>
        {/if}
      </section>

      <section class="card s-card">
        <div class="sec-head">
          <h2>Substrate Doctor</h2>
          <div class="seg">
            <button class="btn btn-sm btn-crew" disabled={doctorBusy} onclick={() => runDoctor(false)}>
              {doctorBusy ? 'Checking…' : 'Run Check'}
            </button>
            <button class="btn btn-sm btn-warn" disabled={doctorBusy} onclick={() => runDoctor(true)}>
              Auto-Fix
            </button>
          </div>
        </div>

        {#if !doctorReport}
          <div class="empty-state">Run Check (100% read-only) or Auto-Fix to inspect DB WAL, .tether/ignore, and credentials.</div>
        {:else}
          <div class="doc-counts">
            <span class="pill pill-rw num">{doctorReport.passed} passed</span>
            <span class="pill pill-guarded num">{doctorReport.warnings} warnings</span>
            <span class="pill pill-danger num">{doctorReport.failed} failed</span>
          </div>
          <div class="doc-list">
            {#each doctorReport.checks as ch, idx (idx)}
              <div class="doc-row">
                <span
                  class="pill"
                  class:pill-rw={ch.status === 'pass' || ch.status === 'fixed'}
                  class:pill-guarded={ch.status === 'warn'}
                  class:pill-danger={ch.status === 'fail'}
                >
                  {ch.status}
                </span>
                <strong>{ch.label}</strong>
                <span class="mono muted">{ch.value}</span>
              </div>
            {/each}
          </div>
        {/if}
      </section>
    </div>
  </div>
</div>

<style>
  .settings-page { height: 100%; padding: 16px; }
  .settings-grid { max-width: 1120px; margin: 0 auto; display: grid; grid-template-columns: repeat(auto-fit, minmax(400px, 1fr)); gap: 14px; }
  .right-col { display: flex; flex-direction: column; gap: 14px; }
  .s-card { display: flex; flex-direction: column; gap: 10px; }
  .s-card h2 { margin: 0; font-size: 15.5px; }
  .sec-head { display: flex; justify-content: space-between; align-items: center; gap: 8px; flex-wrap: wrap; }
  .theme-row { display: flex; justify-content: space-between; align-items: center; flex-wrap: wrap; gap: 8px; }
  .seg { display: flex; gap: 6px; }
  .frow2 { display: grid; grid-template-columns: 1fr 1fr; gap: 8px; }
  .field { display: flex; flex-direction: column; gap: 4px; font-size: 12px; color: var(--subtext); }
  .chk-field { display: flex; align-items: center; gap: 6px; font-size: 12px; color: var(--subtext); align-self: end; padding-bottom: 6px; }
  .acts { display: flex; justify-content: flex-end; }
  .grant-list, .doc-list { display: flex; flex-direction: column; gap: 5px; }
  .grant-row, .doc-row { display: flex; align-items: center; gap: 8px; padding: 6px 8px; background: var(--mantle); border-radius: var(--radius-sm); font-size: 12px; flex-wrap: wrap; }
  .doc-counts { display: flex; gap: 6px; }
  .muted { color: var(--muted); font-size: 11.5px; }
</style>
