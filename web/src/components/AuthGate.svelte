<script lang="ts">
  import { redeemBootstrapToken } from '../lib/api';
  import { app } from '../lib/store.svelte';

  let tokenInput = $state('');
  let err = $state('');
  let busy = $state(false);

  async function submit(e: Event) {
    e.preventDefault();
    if (!tokenInput.trim() || busy) return;
    busy = true;
    err = '';
    try {
      await redeemBootstrapToken(tokenInput);
      await app.init();
    } catch (ex) {
      err = ex instanceof Error ? ex.message : 'Invalid or already redeemed bootstrap token';
    } finally {
      busy = false;
    }
  }
</script>

<div class="gate">
  <form class="card gate-card" onsubmit={submit}>
    <div class="gate-title">
      <span class="pill pill-main">TETHER v0.3.0</span>
      <h2>Authenticate Local Session</h2>
    </div>
    <p class="gate-desc">
      Paste the single-use bootstrap token or full <code>?token=…</code> URL printed by
      <code>tether web</code> to establish your local <code>HttpOnly; SameSite=Strict</code> session.
    </p>
    <input
      class="input mono"
      type="text"
      placeholder="Paste ?token=... or 64-char hex token"
      bind:value={tokenInput}
    />
    {#if err}
      <div class="gate-err">{err}</div>
    {/if}
    <div class="gate-actions">
      <button class="btn btn-primary" type="submit" disabled={busy || !tokenInput.trim()}>
        {busy ? 'Authenticating…' : 'Unlock Local Web UI'}
      </button>
    </div>
  </form>
</div>

<style>
  .gate {
    height: 100dvh;
    display: grid;
    place-items: center;
    padding: 20px;
    background: var(--base);
  }
  .gate-card {
    max-width: 480px;
    width: 100%;
    display: flex;
    flex-direction: column;
    gap: 12px;
    border-color: var(--border);
    box-shadow: var(--shadow);
  }
  .gate-title h2 {
    margin: 8px 0 0;
    font-size: 18px;
    color: var(--text);
  }
  .gate-desc {
    margin: 0;
    color: var(--subtext);
    font-size: 12.5px;
  }
  .gate-err {
    color: var(--danger);
    font-size: 12px;
    padding: 6px 10px;
    border-radius: var(--radius-sm);
    background: rgba(243, 139, 168, 0.12);
    border: 1px solid rgba(243, 139, 168, 0.4);
  }
  .gate-actions {
    display: flex;
    justify-content: flex-end;
  }
</style>
