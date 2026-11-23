<script lang="ts">
  import { renderMarkdown } from '../lib/markdown';
  import { app } from '../lib/store.svelte';

  let question = $state('');
  let pinTopic = $state('architecture');
  let mentionQuery = $state<string | null>(null);
  let mentionIdx = $state(0);

  const fileMatches = $derived.by(() => {
    if (mentionQuery === null) return [];
    const q = mentionQuery.toLowerCase();
    const files = app.mounts?.files ?? [];
    return files
      .filter((f) => f.access_mode !== 'ignored' && (!q || f.rel_path.toLowerCase().includes(q)))
      .slice(0, 8);
  });

  function updateMention(val: string) {
    const m = val.match(/(?:^|\s)@([^\s@]*)$/);
    mentionQuery = m ? m[1] : null;
    mentionIdx = 0;
  }

  function insertMention(relPath: string) {
    question = question.replace(/@([^\s@]*)$/, `@${relPath} `);
    mentionQuery = null;
  }

  async function submit(e: Event) {
    e.preventDefault();
    if (!question.trim() || app.btwBusy) return;
    const q = question;
    question = '';
    mentionQuery = null;
    await app.sendBtw(q);
  }
</script>

{#if app.btwOpen}
  <aside class="btw card">
    <div class="btw-head">
      <div class="btw-title">
        <span class="pill pill-guarded">/btw</span>
        <span>Ephemeral Read-Only Side-Chat</span>
      </div>
      <div class="btw-right">
        {#if app.btwUsage}
          <span class="num muted">{app.btwUsage.prompt_tokens + app.btwUsage.completion_tokens} tok</span>
        {/if}
        <button class="btn btn-sm" onclick={() => (app.btwOpen = false)}>Close</button>
      </div>
    </div>

    <div class="btw-note">
      Reads Main snapshot &amp; <code>@file</code> with read-only tools (<code>usage_id: "btw"</code>). Never mutates Main KV-cache prefix.
    </div>

    <div class="btw-list pane-scroll">
      {#if app.btwHistory.length === 0}
        <div class="empty-state">Ask an architecture or file question using <code>@file</code>…</div>
      {:else}
        {#each app.btwHistory as entry (entry.id)}
          <div class="btw-item">
            <div class="btw-q"><strong>You:</strong> {entry.question}</div>
            {#if entry.error}
              <div class="btw-err">{entry.error}</div>
            {:else if !entry.answer}
              <div class="muted">Inspecting read-only context…</div>
            {:else}
              <div class="md-body btw-a">{@html renderMarkdown(entry.answer)}</div>
              <div class="btw-actions">
                <button class="btn btn-sm btn-crew" onclick={() => app.promoteBtw(entry.question, entry.answer)}>
                  Promote to Main
                </button>
                <input class="input topic-in" type="text" bind:value={pinTopic} placeholder="Topic" />
                <button class="btn btn-sm btn-warn" onclick={() => app.pinBtw(pinTopic, entry.answer, entry.question)}>
                  Pin as Decision
                </button>
              </div>
            {/if}
          </div>
        {/each}
      {/if}
    </div>

    <form class="btw-form" onsubmit={submit}>
      {#if mentionQuery !== null && fileMatches.length > 0}
        <div class="mention-pop">
          {#each fileMatches as f, i (f.rel_path)}
            <button
              type="button"
              class="mention-row"
              class:active={i === mentionIdx}
              onclick={() => insertMention(f.rel_path)}
            >
              <span class="mono">{f.rel_path}</span>
              <span class="pill pill-muted">{f.access_mode}</span>
            </button>
          {/each}
        </div>
      {/if}
      <div class="btw-input-row">
        <input
          class="input"
          type="text"
          placeholder="Ask /btw (supports @file)…"
          bind:value={question}
          oninput={(e) => updateMention((e.currentTarget as HTMLInputElement).value)}
          onkeydown={(e) => {
            if (mentionQuery !== null && fileMatches.length > 0) {
              if (e.key === 'ArrowDown') {
                e.preventDefault();
                mentionIdx = (mentionIdx + 1) % fileMatches.length;
              } else if (e.key === 'ArrowUp') {
                e.preventDefault();
                mentionIdx = (mentionIdx - 1 + fileMatches.length) % fileMatches.length;
              } else if (e.key === 'Tab') {
                e.preventDefault();
                insertMention(fileMatches[mentionIdx].rel_path);
              }
            }
          }}
        />
        <button class="btn btn-warn" type="submit" disabled={app.btwBusy || !question.trim()}>
          {app.btwBusy ? '…' : 'Ask'}
        </button>
      </div>
    </form>
  </aside>
{/if}

<style>
  .btw {
    position: fixed;
    right: 16px;
    bottom: 16px;
    width: min(440px, calc(100vw - 32px));
    height: min(500px, calc(100dvh - 80px));
    z-index: 65;
    display: flex;
    flex-direction: column;
    gap: 8px;
    border-color: var(--guarded);
    box-shadow: var(--shadow);
  }
  .btw-head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 8px;
  }
  .btw-title {
    display: flex;
    align-items: center;
    gap: 8px;
    font-weight: 600;
  }
  .btw-right {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .muted {
    color: var(--muted);
    font-size: 11.5px;
  }
  .btw-note {
    font-size: 11.5px;
    color: var(--subtext);
    background: var(--mantle);
    padding: 5px 8px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border-subtle);
  }
  .btw-list {
    flex: 1;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .btw-item {
    background: var(--mantle);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm);
    padding: 8px 10px;
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .btw-q {
    font-size: 12.5px;
    color: var(--guarded);
  }
  .btw-a {
    font-size: 12.5px;
  }
  .btw-err {
    color: var(--danger);
    font-size: 12px;
  }
  .btw-actions {
    display: flex;
    gap: 6px;
    align-items: center;
    flex-wrap: wrap;
    padding-top: 4px;
    border-top: 1px solid var(--border-subtle);
  }
  .topic-in {
    width: 96px;
    padding: 3px 6px;
    font-size: 11.5px;
  }
  .btw-form {
    position: relative;
  }
  .btw-input-row {
    display: flex;
    gap: 6px;
  }
  .mention-pop {
    position: absolute;
    left: 0;
    right: 0;
    bottom: 100%;
    margin-bottom: 4px;
    background: var(--overlay);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    max-height: 180px;
    overflow-y: auto;
    box-shadow: var(--shadow);
  }
  .mention-row {
    width: 100%;
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 5px 8px;
    border: none;
    background: transparent;
    text-align: left;
    font-size: 12px;
  }
  .mention-row.active,
  .mention-row:hover {
    background: var(--surface1);
  }
</style>
