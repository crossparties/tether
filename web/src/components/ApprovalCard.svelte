<script lang="ts">
  import { app } from '../lib/store.svelte';
  import type { PendingApproval } from '../lib/types';

  let { item }: { item: PendingApproval } = $props();
  let reason = $state('');
  let busy = $state(false);
  let err = $state('');

  const diffText = $derived(item.full_preview || item.preview || '');
  const diffLines = $derived(diffText ? diffText.split('\n').slice(0, 60) : []);

  async function decide(verdict: 'once' | 'folder' | 'session' | 'reject') {
    if (busy) return;
    busy = true;
    err = '';
    try {
      await app.submitApproval(item.id, verdict, verdict === 'reject' ? reason : '');
      reason = '';
    } catch (ex) {
      err = ex instanceof Error ? ex.message : 'Approval submission failed';
    } finally {
      busy = false;
    }
  }
</script>

<div class="approval card">
  <div class="apr-head">
    <div class="apr-meta">
      <span class="pill" class:pill-danger={item.risk === 'high'} class:pill-guarded={item.risk === 'medium'} class:pill-rw={item.risk === 'low'}>
        {(item.risk || 'guarded').toUpperCase()}
      </span>
      <code class="mono apr-tool">{item.tool}</code>
      <span class="mono apr-target">{item.path || item.command || item.label}</span>
    </div>
    {#if item.force_ask}
      <span class="pill pill-crew">Crew Escalation</span>
    {/if}
  </div>

  {#if item.reason || item.scope_hint}
    <div class="apr-why">
      {#if item.reason}<span>{item.reason}</span>{/if}
      {#if item.scope_hint}
        <span class="apr-scope">Scope grant: <code>{item.scope_hint}</code></span>
      {/if}
    </div>
  {/if}

  {#if diffLines.length > 0}
    <pre class="apr-diff pane-scroll">{#each diffLines as line}<span class="dline" class:dadd={line.startsWith('+') && !line.startsWith('+++')} class:ddel={line.startsWith('-') && !line.startsWith('---')} class:dhunk={line.startsWith('@@')}>{line + '\n'}</span>{/each}</pre>
  {/if}

  {#if err}
    <div class="apr-err">{err}</div>
  {/if}

  <div class="apr-actions">
    <div class="apr-allow">
      <button class="btn btn-sm btn-success" disabled={busy} onclick={() => decide('once')}>Once</button>
      <button class="btn btn-sm btn-crew" disabled={busy} onclick={() => decide('folder')}>Folder / Prefix</button>
      <button class="btn btn-sm btn-warn" disabled={busy} onclick={() => decide('session')}>Session</button>
    </div>
    <form
      class="apr-reject"
      onsubmit={(e) => {
        e.preventDefault();
        decide('reject');
      }}
    >
      <input
        class="input apr-reason"
        type="text"
        placeholder="Rejection guidance for agent / Crew (optional)…"
        bind:value={reason}
      />
      <button class="btn btn-sm btn-danger" type="submit" disabled={busy}>Reject</button>
    </form>
  </div>
</div>

<style>
  .approval {
    border-color: var(--danger);
    background: var(--surface0);
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 12px;
  }
  .apr-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    flex-wrap: wrap;
  }
  .apr-meta {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
    flex-wrap: wrap;
  }
  .apr-tool {
    color: var(--main);
    font-weight: 600;
  }
  .apr-target {
    color: var(--plan);
    word-break: break-all;
    font-size: 12px;
  }
  .apr-why {
    display: flex;
    flex-wrap: wrap;
    gap: 10px;
    font-size: 12px;
    color: var(--subtext);
  }
  .apr-scope code {
    color: var(--border);
  }
  .apr-diff {
    margin: 0;
    padding: 8px 10px;
    max-height: 180px;
    background: var(--mantle);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm);
    font-size: 11.5px;
    line-height: 1.45;
  }
  .dadd { color: var(--rw); }
  .ddel { color: var(--danger); }
  .dhunk { color: var(--border); }
  .apr-err {
    color: var(--danger);
    font-size: 12px;
  }
  .apr-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    align-items: center;
    justify-content: space-between;
  }
  .apr-allow {
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
  }
  .apr-reject {
    display: flex;
    gap: 6px;
    flex: 1;
    min-width: 240px;
  }
  .apr-reason {
    padding: 4px 8px;
    font-size: 12px;
  }
</style>
