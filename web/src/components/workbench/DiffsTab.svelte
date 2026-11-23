<script lang="ts">
  import { onMount, untrack } from 'svelte';
  import { ApiError, apiFetch } from '../../lib/api';
  import { app } from '../../lib/store.svelte';

  let mode = $state<'snapshots' | 'git'>('snapshots');
  let busy = $state(false);
  let lastSeenVersion = app.diffsVersion;
  let conflictModal = $state<{
    action: 'turn' | 'file' | 'crew';
    turn?: string;
    path?: string;
    crewId?: string;
    message: string;
    expectedHash?: string;
    actualHash?: string;
  } | null>(null);

  onMount(() => {
    app.refreshDiffs();
  });

  $effect(() => {
    const ver = app.diffsVersion;
    if (ver !== lastSeenVersion) {
      lastSeenVersion = ver;
      untrack(() => app.refreshDiffs());
    }
  });

  async function undoTurn(turn: string, force = false) {
    busy = true;
    try {
      await apiFetch('/api/v1/diffs/undo-turn', {
        method: 'POST',
        body: JSON.stringify({ turn, force }),
      });
      conflictModal = null;
      await app.refreshDiffs();
      app.notify(`Restored turn ${turn}`, 'success');
    } catch (ex) {
      if (ex instanceof ApiError && ex.status === 409) {
        conflictModal = { action: 'turn', turn, message: ex.message };
      } else {
        app.notify(ex instanceof Error ? ex.message : 'Undo turn failed', 'error');
      }
    } finally {
      busy = false;
    }
  }

  async function undoFile(turn: string, path: string, force = false) {
    busy = true;
    try {
      await apiFetch('/api/v1/diffs/undo-file', {
        method: 'POST',
        body: JSON.stringify({ turn, path, force }),
      });
      conflictModal = null;
      await app.refreshDiffs();
      app.notify(`Restored ${path}`, 'success');
    } catch (ex) {
      if (ex instanceof ApiError && ex.status === 409) {
        const resObj = (ex.data?.result as { expected_hash?: string; actual_hash?: string } | undefined) ?? {};
        conflictModal = {
          action: 'file',
          turn,
          path,
          message: ex.message,
          expectedHash: resObj.expected_hash,
          actualHash: resObj.actual_hash,
        };
      } else {
        app.notify(ex instanceof Error ? ex.message : 'Undo file failed', 'error');
      }
    } finally {
      busy = false;
    }
  }

  async function undoCrew(crewId: string, force = false) {
    busy = true;
    try {
      await apiFetch('/api/v1/diffs/undo-crew', {
        method: 'POST',
        body: JSON.stringify({ crew_id: crewId, force }),
      });
      conflictModal = null;
      await app.refreshDiffs();
      app.notify(`Undid crew ${crewId} edits`, 'success');
    } catch (ex) {
      if (ex instanceof ApiError && ex.status === 409) {
        conflictModal = { action: 'crew', crewId, message: ex.message };
      } else {
        app.notify(ex instanceof Error ? ex.message : 'Undo crew failed', 'error');
      }
    } finally {
      busy = false;
    }
  }

  async function forceConfirm() {
    if (!conflictModal) return;
    const c = conflictModal;
    if (c.action === 'turn' && c.turn) await undoTurn(c.turn, true);
    else if (c.action === 'file' && c.turn && c.path) await undoFile(c.turn, c.path, true);
    else if (c.action === 'crew' && c.crewId) await undoCrew(c.crewId, true);
  }
</script>

<div class="diffs-tab">
  <div class="dt-head">
    <div class="seg">
      <button class="btn btn-sm" class:btn-primary={mode === 'snapshots'} onclick={() => (mode = 'snapshots')}>
        Tether Snapshots
      </button>
      <button class="btn btn-sm" class:btn-primary={mode === 'git'} onclick={() => (mode = 'git')}>
        Git Diff
      </button>
    </div>
    <button class="btn btn-sm" onclick={() => app.refreshDiffs()}>Refresh</button>
  </div>

  <div class="dt-body pane-scroll">
    {#if mode === 'git'}
      {#if app.diffs?.git_status}
        <pre class="git-box mono">{app.diffs.git_status}</pre>
      {/if}
      {#if app.diffs?.git_diff}
        <pre class="diff-pre mono">{#each app.diffs.git_diff.split('\n') as line}<span class:dadd={line.startsWith('+') && !line.startsWith('+++')} class:ddel={line.startsWith('-') && !line.startsWith('---')} class:dhunk={line.startsWith('@@')}>{line + '\n'}</span>{/each}</pre>
      {:else}
        <div class="empty-state">Clean git working tree (or no .git directory).</div>
      {/if}
    {:else if (app.diffs?.turns?.length ?? 0) === 0}
      <div class="empty-state">No Tether write snapshots recorded yet.</div>
    {:else}
      {#each app.diffs?.turns ?? [] as turn (turn.turn)}
        <div class="card turn-card">
          <div class="turn-head">
            <div class="turn-meta">
              <code class="mono">{turn.turn}</code>
              {#if turn.crew_id}
                <span class="pill pill-crew">crew: {turn.crew_id}</span>
                <button class="btn btn-sm btn-warn" disabled={busy} onclick={() => undoCrew(turn.crew_id!)}>
                  Undo Crew
                </button>
              {/if}
            </div>
            <button class="btn btn-sm btn-danger" disabled={busy} onclick={() => undoTurn(turn.turn)}>
              Undo Turn
            </button>
          </div>

          {#each turn.entries as entry (entry.path)}
            <div class="entry-box">
              <div class="entry-head">
                <span class="mono epath">{entry.path}</span>
                <div class="entry-acts">
                  <span class="pill pill-muted">{entry.state}</span>
                  {#if entry.externally_modified}
                    <span class="pill pill-guarded">Modified Externally</span>
                  {/if}
                  {#if !entry.redacted}
                    <button class="btn btn-sm" disabled={busy} onclick={() => undoFile(turn.turn, entry.path)}>
                      Undo File
                    </button>
                  {/if}
                </div>
              </div>
              {#if entry.redacted}
                <div class="muted">Redacted by .tether/ignore</div>
              {:else if entry.diff}
                <pre class="diff-pre mono">{#each entry.diff.split('\n') as line}<span class:dadd={line.startsWith('+') && !line.startsWith('+++')} class:ddel={line.startsWith('-') && !line.startsWith('---')} class:dhunk={line.startsWith('@@')}>{line + '\n'}</span>{/each}</pre>
              {/if}
            </div>
          {/each}
        </div>
      {/each}
    {/if}
  </div>
</div>

{#if conflictModal}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="backdrop" onclick={() => (conflictModal = null)} onkeydown={(e) => e.key === 'Escape' && (conflictModal = null)}>
    <!-- svelte-ignore a11y_click_events_have_key_events -->
    <div class="modal card" onclick={(e) => e.stopPropagation()}>
      <div class="modal-head">
        <span class="pill pill-danger">409 Conflict — External Modification Detected</span>
      </div>
      <p class="modal-msg">{conflictModal.message}</p>
      {#if conflictModal.path}
        <div class="mono muted">File: {conflictModal.path}</div>
      {/if}
      {#if conflictModal.expectedHash || conflictModal.actualHash}
        <div class="hash-box mono">
          <div>PostHash: {conflictModal.expectedHash || '—'}</div>
          <div>DiskHash: {conflictModal.actualHash || '—'}</div>
        </div>
      {/if}
      <p class="modal-warn">
        Overwriting will discard manual disk edits made after Tether's last write.
      </p>
      <div class="modal-acts">
        <button class="btn" onclick={() => (conflictModal = null)}>Keep Disk Edits</button>
        <button class="btn btn-danger" disabled={busy} onclick={forceConfirm}>Force Overwrite</button>
      </div>
    </div>
  </div>
{/if}

<style>
  .diffs-tab {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
    gap: 8px;
  }
  .dt-head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 8px;
  }
  .seg {
    display: flex;
    gap: 4px;
  }
  .dt-body {
    flex: 1;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .turn-card {
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 10px;
  }
  .turn-head,
  .turn-meta,
  .entry-head,
  .entry-acts {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
  }
  .entry-box {
    border-top: 1px solid var(--border-subtle);
    padding-top: 6px;
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .epath {
    font-size: 12px;
    color: var(--plan);
  }
  .git-box,
  .diff-pre {
    margin: 0;
    padding: 8px;
    background: var(--mantle);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm);
    font-size: 11.5px;
    overflow-x: auto;
    max-height: 240px;
  }
  .dadd { color: var(--rw); }
  .ddel { color: var(--danger); }
  .dhunk { color: var(--border); }
  .muted { color: var(--muted); font-size: 11.5px; }
  .backdrop {
    position: fixed;
    inset: 0;
    z-index: 95;
    background: rgba(17, 17, 27, 0.72);
    display: grid;
    place-items: center;
    padding: 16px;
  }
  .modal {
    max-width: 480px;
    width: 100%;
    border-color: var(--danger);
    box-shadow: var(--shadow);
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .modal-msg { margin: 0; font-size: 13px; }
  .modal-warn { margin: 0; font-size: 12px; color: var(--guarded); }
  .hash-box {
    background: var(--mantle);
    padding: 8px;
    border-radius: var(--radius-sm);
    font-size: 11px;
    word-break: break-all;
    color: var(--subtext);
  }
  .modal-acts {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
  }
</style>
