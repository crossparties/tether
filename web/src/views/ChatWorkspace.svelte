<script lang="ts">
  import ApprovalCard from '../components/ApprovalCard.svelte';
  import BtwDrawer from '../components/BtwDrawer.svelte';
  import ClarificationCard from '../components/ClarificationCard.svelte';
  import CrewPanel from '../components/CrewPanel.svelte';
  import WorkbenchDrawer from '../components/WorkbenchDrawer.svelte';
  import { renderMarkdown } from '../lib/markdown';
  import { app } from '../lib/store.svelte';

  let composer = $state('');
  let newThreadMode = $state(false);
  let newThreadTitle = $state('');
  let mentionQuery = $state<string | null>(null);
  let mentionIdx = $state(0);
  let feedEl: HTMLDivElement | undefined = $state();

  const activeCrews = $derived(app.crews.filter((c) => c.status === 'active'));
  const latestCrew = $derived(activeCrews[0] ?? app.crews[0] ?? null);
  const hasProviderReady = $derived(
    (app.setupStatus?.has_global_keys ?? false) ||
      (app.setupStatus?.keys?.length ?? 0) > 0 ||
      (app.project?.provider || app.setupStatus?.active_provider) === 'ollama',
  );

  const fileMatches = $derived.by(() => {
    if (mentionQuery === null) return [];
    const q = mentionQuery.toLowerCase();
    return (app.mounts?.files ?? [])
      .filter((f) => f.access_mode !== 'ignored' && (!q || f.rel_path.toLowerCase().includes(q)))
      .slice(0, 10);
  });

  function updateMention(val: string) {
    const m = val.match(/(?:^|\s)@([^\s@]*)$/);
    mentionQuery = m ? m[1] : null;
    mentionIdx = 0;
  }

  function insertMention(relPath: string) {
    composer = composer.replace(/@([^\s@]*)$/, `@${relPath} `);
    mentionQuery = null;
  }

  async function submitComposer() {
    const text = composer.trim();
    if (!text) return;
    composer = '';
    mentionQuery = null;
    await app.sendChat(text);
  }

  function onComposerKeydown(e: KeyboardEvent) {
    if (mentionQuery !== null && fileMatches.length > 0) {
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        mentionIdx = (mentionIdx + 1) % fileMatches.length;
        return;
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault();
        mentionIdx = (mentionIdx - 1 + fileMatches.length) % fileMatches.length;
        return;
      }
      if (e.key === 'Tab' || (e.key === 'Enter' && !e.shiftKey)) {
        e.preventDefault();
        insertMention(fileMatches[mentionIdx].rel_path);
        return;
      }
      if (e.key === 'Escape') {
        mentionQuery = null;
        return;
      }
    }

    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      submitComposer();
    }
  }

  $effect(() => {
    app.timeline.length;
    if (feedEl) {
      queueMicrotask(() => {
        if (feedEl) feedEl.scrollTop = feedEl.scrollHeight;
      });
    }
  });
</script>

<div class="ws-split">
  <div class="chat-pane">
    <!-- Thread + Crew Pill Bar -->
    <div class="chat-subbar">
      <div class="subbar-left">
        <button
          class="btn btn-sm"
          class:btn-primary={app.chatSubTab === 'main'}
          onclick={() => (app.chatSubTab = 'main')}
        >
          Main Chat
        </button>

        <select
          class="select thread-sel"
          value={app.activeThreadId}
          aria-label="Conversation Thread"
          onchange={(e) => app.selectThread((e.currentTarget as HTMLSelectElement).value)}
        >
          {#each app.threads as th (th.id)}
            <option value={th.id}>{th.title || th.id.slice(0, 8)}</option>
          {/each}
        </select>

        {#if newThreadMode}
          <form
            class="new-th-form"
            onsubmit={async (e) => {
              e.preventDefault();
              await app.createThread(newThreadTitle);
              newThreadTitle = '';
              newThreadMode = false;
            }}
          >
            <input class="input th-in" type="text" placeholder="Thread title…" bind:value={newThreadTitle} />
            <button class="btn btn-sm btn-primary" type="submit">Create</button>
            <button class="btn btn-sm" type="button" onclick={() => (newThreadMode = false)}>×</button>
          </form>
        {:else}
          <button class="btn btn-sm" onclick={() => (newThreadMode = true)}>+ Thread</button>
        {/if}

        <!-- Live Crew Pill -->
        {#if latestCrew}
          <button
            class="pill pill-crew crew-pill"
            class:active={app.chatSubTab === latestCrew.id}
            onclick={() => (app.chatSubTab = app.chatSubTab === latestCrew.id ? 'main' : latestCrew.id)}
            title="Inspect isolated Crew Session sub-tab"
          >
            <span>Crew [{latestCrew.name}]</span>
            {#if latestCrew.lease_files?.length}
              <span class="mono">• {latestCrew.lease_files[0]}</span>
            {/if}
            <span>• {latestCrew.status}</span>
          </button>
        {/if}
      </div>

      <div class="subbar-right">
        <button class="btn btn-sm" onclick={() => app.compactNow()}>Compact</button>
        <button class="btn btn-sm btn-warn" onclick={() => (app.btwOpen = !app.btwOpen)}>/btw</button>
        <button class="btn btn-sm" onclick={() => (app.drawerOpen = !app.drawerOpen)}>
          {app.drawerOpen ? 'Hide Workbench' : 'Show Workbench'}
        </button>
      </div>
    </div>

    {#if app.crews.length > 1 && app.chatSubTab !== 'main'}
      <div class="crew-switcher">
        {#each app.crews as c (c.id)}
          <button
            class="btn btn-sm"
            class:btn-crew={app.chatSubTab === c.id}
            onclick={() => (app.chatSubTab = c.id)}
          >
            {c.name} ({c.status})
          </button>
        {/each}
      </div>
    {/if}

    {#if app.chatSubTab !== 'main'}
      <CrewPanel crewId={app.chatSubTab} />
    {:else}
      <!-- Main Chat Timeline -->
      <div class="chat-feed pane-scroll" bind:this={feedEl}>
        {#if app.timeline.length === 0}
          <div class="empty-state welcome">
            <div class="welcome-head">
              <span class="pill pill-main">Tether v0.3.0 Ready</span>
              <span class="pill" class:pill-plan={app.mode === 'plan'} class:pill-rw={app.mode === 'code'}>
                Mode: {app.mode.toUpperCase()}
              </span>
            </div>

            {#if !(app.setupStatus?.has_global_keys) || !(app.setupStatus?.project_ready)}
              <div class="setup-nudge">
                <div>
                  <strong>Setup Pertama Kali Belum Lengkap</strong>
                  <p>Atur API Key AI (Stage 1) dan klik Initialize Project (Stage 2) terlebih dahulu.</p>
                </div>
                <button class="btn btn-sm btn-primary" onclick={() => (app.activeView = 'setup')}>
                  Buka Panduan Setup →
                </button>
              </div>
            {/if}

            <p class="welcome-sub">
              <strong>Pilih aksi pertama Anda</strong> (klik kartu di bawah untuk mengisi prompt otomatis, atau ketik <code>@</code> untuk melampirkan file):
            </p>

            <div class="starter-grid">
              <button
                type="button"
                class="starter-card"
                onclick={() => {
                  composer = 'Jelaskan arsitektur utama proyek ini berdasarkan RepoMap.';
                }}
              >
                <strong>1. Pahami Arsitektur Proyek</strong>
                <span>Analisis struktur kode lewat Pure-Go RepoMap tanpa boros token.</span>
              </button>

              <button
                type="button"
                class="starter-card"
                onclick={() => {
                  app.switchMode('plan');
                  composer = 'Buatkan rencana implementasi bertahap di .tether/PLAN.md untuk ';
                }}
              >
                <strong>2. Buat Rencana Fitur (Mode PLAN)</strong>
                <span>Beralih ke mode PLAN (read-only) untuk merancang fitur dengan aman.</span>
              </button>

              <button
                type="button"
                class="starter-card"
                onclick={() => {
                  composer = composer ? `${composer} @` : '@';
                  updateMention(composer);
                }}
              >
                <strong>3. Lampirkan File (@file)</strong>
                <span>Buka autocomplete file proyek (otomatis SHA-256 deduplication).</span>
              </button>

              <button
                type="button"
                class="starter-card"
                onclick={() => {
                  app.btwOpen = true;
                }}
              >
                <strong>4. Tanya Sela (/btw Side-Chat)</strong>
                <span>Tanya hal cepat dengan konteks penuh tanpa merusak KV Prompt Cache.</span>
              </button>
            </div>
          </div>
        {:else}
          {#each app.timeline as item (item.id)}
            {#if item.kind === 'notice' || item.kind === 'condensation'}
              <div class="divider-badge">
                <span class="pill" class:pill-main={item.kind === 'condensation'} class:pill-guarded={item.kind === 'notice'}>
                  {item.noticeKind}
                </span>
                {#if item.content}
                  <span class="div-text">{item.content}</span>
                {/if}
              </div>
            {:else if item.kind === 'error'}
              <div class="msg card msg-err">
                <strong>Error:</strong> {item.content}
              </div>
            {:else if item.kind === 'tool'}
              <details class="msg tool-msg">
                <summary class="tool-sum">
                  <span class="pill pill-crew">tool</span>
                  <span class="mono muted">{item.toolCallId || 'result'}</span>
                </summary>
                <pre class="tool-pre mono">{item.content}</pre>
              </details>
            {:else}
              <div class="msg" class:user={item.kind === 'user'} class:assistant={item.kind === 'assistant'}>
                <div class="msg-head">
                  <span class="pill" class:pill-main={item.kind === 'user'} class:pill-crew={item.kind === 'assistant'}>
                    {item.kind === 'user' ? 'You' : 'Main Agent'}
                  </span>
                  {#if item.streaming}
                    <span class="pill pill-rw">Streaming…</span>
                  {/if}
                </div>

                {#if item.reasoning}
                  <details class="thought" open={item.streaming}>
                    <summary>Reasoning / Thought</summary>
                    <div class="thought-body">{item.reasoning}</div>
                  </details>
                {/if}

                {#if item.content}
                  <div class="md-body">{@html renderMarkdown(item.content)}</div>
                {/if}

                {#if item.toolCalls && item.toolCalls.length > 0}
                  <div class="tc-row">
                    {#each item.toolCalls as tc (tc.id)}
                      <code class="pill pill-muted mono">{tc.function.name}</code>
                    {/each}
                  </div>
                {/if}

                {#if item.clarifications && item.clarifications.length > 0}
                  <ClarificationCard questions={item.clarifications} />
                {/if}
              </div>
            {/if}
          {/each}
        {/if}
      </div>

      <!-- Pending Visual Diff Approvals -->
      {#if app.pendingApprovals.length > 0}
        <div class="apr-wrap">
          <ApprovalCard item={app.pendingApprovals[0]} />
        </div>
      {/if}

      <!-- Composer -->
      <div class="composer-wrap">
        {#if app.queuedPrompts.length > 0}
          <div class="queue-bar">
            <span class="pill pill-plan num">Queued ({app.queuedPrompts.length})</span>
            {#each app.queuedPrompts as qp, idx}
              <span class="q-chip">
                <span class="q-txt">{qp}</span>
                <button onclick={() => app.removeQueuedPrompt(idx)} aria-label="Remove queued message">×</button>
              </span>
            {/each}
          </div>
        {/if}

        <div class="composer-box">
          {#if mentionQuery !== null && fileMatches.length > 0}
            <div class="mention-menu">
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

          <textarea
            class="textarea comp-input"
            rows={3}
            placeholder="Ask Tether… (@file to attach context, /btw for side-chat, Enter to send, Shift+Enter newline)"
            bind:value={composer}
            oninput={(e) => updateMention((e.currentTarget as HTMLTextAreaElement).value)}
            onkeydown={onComposerKeydown}
          ></textarea>

          <div class="comp-actions">
            <span class="muted">
              {#if app.turnActive}
                Agent running — typing queues a follow-up
              {:else}
                <code>@file</code> • <code>/compact</code> • <code>/mode plan|code</code>
              {/if}
            </span>
            <div class="comp-btns">
              {#if !hasProviderReady}
                <button class="btn btn-sm btn-warn" onclick={() => (app.activeView = 'setup')}>
                  ⚠ Setup API Key Dulu →
                </button>
              {/if}
              {#if app.turnActive}
                <button class="btn btn-sm btn-danger" onclick={() => app.abortChat()}>Abort Turn</button>
              {/if}
              <button class="btn btn-sm btn-primary" disabled={!composer.trim() || !hasProviderReady} onclick={submitComposer}>
                {app.turnActive ? 'Queue Follow-Up' : 'Send'}
              </button>
            </div>
          </div>
        </div>
      </div>
    {/if}
  </div>

  {#if app.drawerOpen}
    <aside class="workbench-pane">
      <WorkbenchDrawer />
    </aside>
  {/if}
</div>

<BtwDrawer />

<style>
  .ws-split {
    display: flex;
    height: 100%;
    min-height: 0;
    width: 100%;
    overflow: hidden;
  }
  .chat-pane {
    flex: 1 1 56%;
    min-width: 0;
    min-height: 0;
    display: flex;
    flex-direction: column;
    border-right: 1px solid var(--border-subtle);
  }
  .workbench-pane {
    flex: 0 0 44%;
    max-width: 680px;
    min-width: 340px;
    min-height: 0;
    display: flex;
    flex-direction: column;
  }
  @media (max-width: 980px) {
    .ws-split { flex-direction: column; }
    .chat-pane { border-right: none; border-bottom: 1px solid var(--border-subtle); }
    .workbench-pane { flex: 0 0 48%; max-width: none; min-width: 0; }
  }
  .chat-subbar {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
    padding: 8px 12px;
    background: var(--mantle);
    border-bottom: 1px solid var(--border-subtle);
  }
  .subbar-left,
  .subbar-right,
  .new-th-form {
    display: flex;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
  }
  .thread-sel {
    width: auto;
    max-width: 210px;
    padding: 4px 8px;
    font-size: 12px;
  }
  .th-in {
    width: 150px;
    padding: 4px 8px;
    font-size: 12px;
  }
  .crew-pill {
    cursor: pointer;
  }
  .crew-pill.active {
    border-color: var(--crew);
    background: rgba(137, 180, 250, 0.25);
  }
  .crew-switcher {
    display: flex;
    gap: 6px;
    padding: 6px 12px;
    background: var(--surface0);
    border-bottom: 1px solid var(--border-subtle);
    flex-wrap: wrap;
  }
  .chat-feed {
    flex: 1;
    padding: 12px 14px;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .welcome {
    margin: auto 0;
    display: flex;
    flex-direction: column;
    gap: 10px;
    padding: 16px;
    text-align: left;
  }
  .welcome-head {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .welcome-sub {
    margin: 0;
    font-size: 12.5px;
    color: var(--subtext);
  }
  .setup-nudge {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
    padding: 10px 12px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--main);
    background: rgba(203, 166, 247, 0.12);
    font-size: 12px;
  }
  .setup-nudge p {
    margin: 2px 0 0;
    color: var(--subtext);
  }
  .starter-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(210px, 1fr));
    gap: 8px;
  }
  .starter-card {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 4px;
    padding: 10px 12px;
    border-radius: var(--radius-sm);
    background: var(--mantle);
    border: 1px solid var(--border-subtle);
    color: var(--text);
    text-align: left;
    cursor: pointer;
    font-size: 12px;
    transition: border-color 0.15s, background 0.15s;
  }
  .starter-card:hover {
    border-color: var(--main);
    background: rgba(203, 166, 247, 0.09);
  }
  .starter-card span {
    font-size: 11.5px;
    color: var(--subtext);
    line-height: 1.35;
  }
  .msg {
    padding: 10px 12px;
    border-radius: var(--radius-md);
    border: 1px solid var(--border-subtle);
    background: var(--surface0);
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .msg.user {
    border-left: 3px solid var(--main);
    background: var(--overlay);
  }
  .msg.assistant {
    border-left: 3px solid var(--crew);
  }
  .msg-err {
    border-color: var(--danger);
    color: var(--danger);
  }
  .msg-head {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .tool-msg {
    background: var(--mantle);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm);
    padding: 6px 10px;
  }
  .tool-sum {
    cursor: pointer;
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 12px;
  }
  .tool-pre {
    margin: 6px 0 0;
    font-size: 11.5px;
    white-space: pre-wrap;
    max-height: 220px;
    overflow-y: auto;
    color: var(--subtext);
  }
  .thought {
    font-size: 12px;
    color: var(--subtext);
  }
  .thought-body {
    margin-top: 4px;
    padding: 6px 8px;
    background: var(--mantle);
    border-radius: var(--radius-sm);
    white-space: pre-wrap;
    font-size: 12px;
  }
  .tc-row {
    display: flex;
    flex-wrap: wrap;
    gap: 5px;
  }
  .divider-badge {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 4px 10px;
    border-radius: var(--radius-sm);
    background: var(--mantle);
    border: 1px dashed var(--border-subtle);
    font-size: 12px;
    color: var(--subtext);
  }
  .div-text {
    white-space: pre-wrap;
    word-break: break-word;
  }
  .apr-wrap {
    padding: 0 12px 8px;
  }
  .composer-wrap {
    padding: 8px 12px 10px;
    border-top: 1px solid var(--border-subtle);
    background: var(--mantle);
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .queue-bar {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    align-items: center;
  }
  .q-chip {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    padding: 2px 7px;
    border-radius: var(--radius-sm);
    background: var(--overlay);
    font-size: 11.5px;
    max-width: 240px;
  }
  .q-txt {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .q-chip button {
    border: none;
    background: transparent;
    color: var(--danger);
    padding: 0 2px;
  }
  .composer-box {
    position: relative;
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .comp-input {
    resize: vertical;
    min-height: 62px;
    max-height: 180px;
    background: var(--base);
  }
  .comp-actions {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 8px;
  }
  .comp-btns {
    display: flex;
    gap: 6px;
  }
  .mention-menu {
    position: absolute;
    left: 0;
    right: 0;
    bottom: 100%;
    margin-bottom: 4px;
    background: var(--overlay);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    max-height: 200px;
    overflow-y: auto;
    box-shadow: var(--shadow);
    z-index: 40;
  }
  .mention-row {
    width: 100%;
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 5px 10px;
    border: none;
    background: transparent;
    text-align: left;
    font-size: 12px;
  }
  .mention-row.active,
  .mention-row:hover {
    background: var(--surface1);
  }
  .muted {
    color: var(--muted);
    font-size: 11.5px;
  }
</style>
