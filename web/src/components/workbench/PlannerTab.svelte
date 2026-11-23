<script lang="ts">
  import { apiFetch } from '../../lib/api';
  import { app } from '../../lib/store.svelte';
  import CodeEditor from '../CodeEditor.svelte';

  let draft = $state('');
  let runCritic = $state(false);
  let saving = $state(false);
  let building = $state(false);
  let criticNotes = $state('');
  let err = $state('');

  $effect(() => {
    app.planVersion;
    draft = app.planContent;
  });

  async function savePlan(): Promise<boolean> {
    saving = true;
    err = '';
    try {
      const res = await apiFetch<{ ok: boolean; bytes: number; content: string }>('/api/v1/plan', {
        method: 'PUT',
        body: JSON.stringify({ content: draft }),
      });
      app.planContent = res.content;
      app.notify(`Saved .tether/PLAN.md (${res.bytes} bytes)`, 'success');
      return true;
    } catch (ex) {
      err = ex instanceof Error ? ex.message : 'Failed to save PLAN.md';
      return false;
    } finally {
      saving = false;
    }
  }

  async function startBuilding() {
    building = true;
    err = '';
    criticNotes = '';
    try {
      if (!(await savePlan())) {
        return;
      }
      const res = await apiFetch<{
        ready: boolean;
        mode: string;
        critic_ran?: boolean;
        critic_notes?: string;
        plan_bytes: number;
      }>('/api/v1/plan/start-building', {
        method: 'POST',
        body: JSON.stringify({ run_critic: runCritic }),
      });
      if (res.critic_notes) criticNotes = res.critic_notes;
      app.mode = res.mode === 'plan' || res.mode === 'review' ? res.mode : 'code';
      if (res.ready) {
        app.notify('Plan approved — switched to [CODE] mode!', 'success');
      } else {
        app.notify('Critic Crew blocked build — inspect notes below', 'warn');
      }
    } catch (ex) {
      err = ex instanceof Error ? ex.message : 'Start Building failed';
    } finally {
      building = false;
    }
  }
</script>

<div class="planner-tab">
  <div class="pt-head">
    <div class="pt-title">
      <code class="mono">.tether/PLAN.md</code>
      <span class="pill" class:pill-plan={app.mode === 'plan'} class:pill-rw={app.mode === 'code'}>
        {app.mode.toUpperCase()}
      </span>
    </div>
    <div class="pt-acts">
      <button class="btn btn-sm" onclick={() => app.refreshPlan()}>Reload</button>
      <button class="btn btn-sm btn-crew" disabled={saving} onclick={savePlan}>
        {saving ? 'Saving…' : 'Save'}
      </button>
      <button class="btn btn-sm btn-success" disabled={building} onclick={startBuilding}>
        {building ? 'Starting…' : 'Start Building'}
      </button>
    </div>
  </div>

  <label class="critic-row">
    <input type="checkbox" bind:checked={runCritic} />
    <span>Run read-only Critic Crew audit before unlocking <code>CODE</code> mode</span>
  </label>

  {#if err}
    <div class="err-box">{err}</div>
  {/if}
  {#if criticNotes}
    <div class="card critic-box">
      <strong>Critic Crew Audit:</strong>
      <pre class="critic-pre">{criticNotes}</pre>
    </div>
  {/if}

  <div class="pt-editor">
    <CodeEditor value={draft} filename=".tether/PLAN.md" minHeight="220px" onchange={(v) => (draft = v)} />
  </div>
</div>

<style>
  .planner-tab {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
    gap: 8px;
  }
  .pt-head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .pt-title,
  .pt-acts {
    display: flex;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
  }
  .critic-row {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 12px;
    color: var(--subtext);
  }
  .err-box {
    color: var(--danger);
    font-size: 12px;
  }
  .critic-box {
    border-color: var(--plan);
    font-size: 12px;
    max-height: 140px;
    overflow-y: auto;
  }
  .critic-pre {
    margin: 4px 0 0;
    white-space: pre-wrap;
    font-size: 11.5px;
  }
  .pt-editor {
    flex: 1;
    min-height: 0;
  }
</style>
