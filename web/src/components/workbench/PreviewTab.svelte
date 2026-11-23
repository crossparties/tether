<script lang="ts">
  import { app } from '../../lib/store.svelte';

  let pathInput = $state('index.html');
  let nonce = $state(0);

  const htmlCandidates = $derived.by(() => {
    const files = app.mounts?.files ?? [];
    return files
      .filter((f) => f.access_mode !== 'ignored' && (f.rel_path.endsWith('.html') || f.rel_path.endsWith('.htm')))
      .slice(0, 20);
  });

  const iframeSrc = $derived.by(() => {
    const clean = pathInput.trim().replace(/^\/+/, '') || 'index.html';
    const encoded = clean.split('/').map(encodeURIComponent).join('/');
    return `/preview/${encoded}?_=${nonce}`;
  });
</script>

<div class="preview-tab">
  <form
    class="pv-bar"
    onsubmit={(e) => {
      e.preventDefault();
      nonce++;
    }}
  >
    <input class="input mono pv-in" type="text" bind:value={pathInput} placeholder="public/index.html" />
    {#if htmlCandidates.length > 0}
      <select
        class="select pv-sel"
        value={pathInput}
        onchange={(e) => {
          pathInput = (e.currentTarget as HTMLSelectElement).value;
          nonce++;
        }}
      >
        {#each htmlCandidates as c (c.rel_path)}
          <option value={c.rel_path}>{c.rel_path}</option>
        {/each}
      </select>
    {/if}
    <button class="btn btn-sm btn-crew" type="submit">Reload</button>
  </form>

  <div class="pv-note">
    Sandboxed iframe (<code>sandbox="allow-scripts allow-forms"</code>) — opaque origin &amp; <code>.tether/ignore</code> enforced.
  </div>

  <iframe
    class="pv-frame"
    title="Tether Sandboxed Preview"
    sandbox="allow-scripts allow-forms"
    src={iframeSrc}
  ></iframe>
</div>

<style>
  .preview-tab {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
    gap: 6px;
  }
  .pv-bar {
    display: flex;
    gap: 6px;
    align-items: center;
  }
  .pv-in {
    flex: 1;
    padding: 5px 8px;
    font-size: 12px;
  }
  .pv-sel {
    max-width: 170px;
    padding: 5px 6px;
    font-size: 12px;
  }
  .pv-note {
    font-size: 11px;
    color: var(--muted);
  }
  .pv-frame {
    flex: 1;
    width: 100%;
    min-height: 0;
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm);
    background: #ffffff;
  }
</style>
