<script lang="ts">
  import { app } from '../../lib/store.svelte';

  let compacting = $state(false);

  const mainM = $derived(app.tokens?.main);
  const ctxWin = $derived(app.tokens?.context_window || 128000);
  const pressurePct = $derived(Math.min(100, Math.round((mainM?.context_pressure ?? 0) * 1000) / 10));
  const cachePct = $derived(Math.min(100, Math.round((mainM?.cache_hit_ratio ?? 0) * 1000) / 10));
  const entries = $derived(app.tokens?.entries ?? []);

  async function compact() {
    compacting = true;
    try {
      await app.compactNow();
    } finally {
      compacting = false;
    }
  }
</script>

<div class="usage-tab pane-scroll">
  <div class="card meter-card">
    <div class="meter-head">
      <div>
        <span class="pill pill-main">Main Context Pressure</span>
        <div class="muted">Per-turn tokens vs {ctxWin.toLocaleString()} window (72% triggers EpisodicCondenser)</div>
      </div>
      <div class="meter-right">
        <span class="num big">{pressurePct}%</span>
        <button class="btn btn-sm btn-warn" disabled={compacting} onclick={compact}>
          {compacting ? 'Compacting…' : 'Compact Now'}
        </button>
      </div>
    </div>
    <div class="bar-track">
      <div
        class="bar-fill"
        class:warn={pressurePct >= 72}
        style:width={`${pressurePct}%`}
      ></div>
    </div>
    <div class="num sub-line">
      Last turn: {(mainM?.last_turn_tokens ?? 0).toLocaleString()} / {ctxWin.toLocaleString()} tok
      {#if mainM?.last_ttft_ms}
        • TTFT {mainM.last_ttft_ms} ms
      {/if}
    </div>
  </div>

  <div class="card meter-card">
    <div class="meter-head">
      <div>
        <span class="pill pill-rw">Cumulative KV-Cache Hit</span>
        <div class="muted">Prefix cache hits vs cold prompt miss tokens</div>
      </div>
      <span class="num big green">{cachePct}%</span>
    </div>
    <div class="bar-track">
      <div class="bar-fill cache" style:width={`${cachePct}%`}></div>
    </div>
    <div class="num sub-line">
      Cached: {(mainM?.cached_tokens ?? 0).toLocaleString()} • Miss: {(mainM?.miss_tokens ?? 0).toLocaleString()} • Out: {(mainM?.completion_tokens ?? 0).toLocaleString()}
    </div>
  </div>

  <div class="card">
    <div class="sec-title">
      <span>Prompt Cache Waterfall</span>
      <div class="legend">
        <span class="dot cached"></span><span class="muted">Cached</span>
        <span class="dot miss"></span><span class="muted">Miss</span>
        <span class="dot comp"></span><span class="muted">Out</span>
      </div>
    </div>
    {#if app.waterfall.length === 0}
      <div class="empty-state">Send a turn to visualize per-call KV-cache prefix vs miss tokens.</div>
    {:else}
      <div class="wf-list">
        {#each app.waterfall as s (s.calls)}
          {@const total = Math.max(1, s.cached + s.miss + s.completion)}
          <div class="wf-row">
            <span class="num wf-lbl">{s.turnLabel}</span>
            <div class="wf-bar" title="Cached {s.cached} / Miss {s.miss} / Out {s.completion}">
              <div class="seg-cached" style:width={`${(s.cached / total) * 100}%`}></div>
              <div class="seg-miss" style:width={`${(s.miss / total) * 100}%`}></div>
              <div class="seg-comp" style:width={`${(s.completion / total) * 100}%`}></div>
            </div>
            <span class="num wf-num">{Math.round((s.cached / Math.max(1, s.cached + s.miss)) * 100)}%</span>
          </div>
        {/each}
      </div>
    {/if}
  </div>

  <div class="card">
    <div class="sec-title">
      <span>Usage Breakdown by <code>usage_id</code></span>
      <button class="btn btn-sm" onclick={() => app.refreshTokens()}>Refresh</button>
    </div>
    {#if entries.length === 0}
      <div class="empty-state">No token usage recorded yet.</div>
    {:else}
      <div class="tbl-wrap">
        <table class="utbl">
          <thead>
            <tr>
              <th>usage_id</th>
              <th class="num">Calls</th>
              <th class="num">Prompt</th>
              <th class="num">Cached</th>
              <th class="num">Cache %</th>
              <th class="num">TTFT</th>
            </tr>
          </thead>
          <tbody>
            {#each entries as e (e.usage_id)}
              <tr>
                <td class="mono">{e.usage_id}</td>
                <td class="num">{e.calls}</td>
                <td class="num">{e.prompt_tokens.toLocaleString()}</td>
                <td class="num">{e.cached_tokens.toLocaleString()}</td>
                <td class="num">{(e.cache_hit_ratio * 100).toFixed(1)}%</td>
                <td class="num">{e.last_ttft_ms ? `${e.last_ttft_ms}ms` : '—'}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </div>
</div>

<style>
  .usage-tab {
    height: 100%;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .meter-card {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .meter-head {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 8px;
  }
  .meter-right {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .big {
    font-size: 16px;
    font-weight: 700;
  }
  .green { color: var(--rw); }
  .muted { color: var(--muted); font-size: 11.5px; }
  .sub-line { font-size: 11.5px; color: var(--subtext); }
  .bar-track {
    height: 8px;
    background: var(--mantle);
    border-radius: 999px;
    overflow: hidden;
    border: 1px solid var(--border-subtle);
  }
  .bar-fill {
    height: 100%;
    background: var(--main);
    transition: width 200ms ease;
  }
  .bar-fill.warn { background: var(--danger); }
  .bar-fill.cache { background: var(--rw); }
  .sec-title {
    display: flex;
    justify-content: space-between;
    align-items: center;
    margin-bottom: 8px;
    font-weight: 600;
    font-size: 12.5px;
  }
  .legend {
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .dot {
    width: 8px;
    height: 8px;
    border-radius: 2px;
    display: inline-block;
  }
  .dot.cached, .seg-cached { background: var(--rw); }
  .dot.miss, .seg-miss { background: var(--guarded); }
  .dot.comp, .seg-comp { background: var(--crew); }
  .wf-list {
    display: flex;
    flex-direction: column;
    gap: 5px;
  }
  .wf-row {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .wf-lbl {
    width: 58px;
    font-size: 11px;
    color: var(--subtext);
  }
  .wf-bar {
    flex: 1;
    height: 12px;
    display: flex;
    border-radius: 4px;
    overflow: hidden;
    background: var(--mantle);
  }
  .wf-num {
    width: 38px;
    text-align: right;
    font-size: 11px;
  }
  .tbl-wrap {
    overflow-x: auto;
  }
  .utbl {
    width: 100%;
    border-collapse: collapse;
    font-size: 12px;
  }
  .utbl th,
  .utbl td {
    padding: 5px 6px;
    text-align: left;
    border-bottom: 1px solid var(--border-subtle);
  }
  .utbl th.num,
  .utbl td.num {
    text-align: right;
  }
  .utbl th {
    color: var(--subtext);
    font-weight: 600;
  }
</style>
