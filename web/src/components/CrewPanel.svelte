<script lang="ts">
  import { untrack } from 'svelte';
  import { apiFetch } from '../lib/api';
  import { renderMarkdown } from '../lib/markdown';
  import { app } from '../lib/store.svelte';
  import type { ConversationEvent, CrewSession } from '../lib/types';

  let { crewId }: { crewId: string } = $props();

  let crewDetail = $state<CrewSession | null>(null);
  let messages = $state<ConversationEvent[]>([]);
  let loading = $state(false);
  let busy = $state(false);
  let prevCrewId = '';
  let prevStatus = '';

  const liveCrew = $derived(app.crews.find((c) => c.id === crewId) ?? crewDetail);

  async function loadCrew() {
    if (!crewId) return;
    loading = true;
    try {
      const res = await apiFetch<{ crew: CrewSession; messages: ConversationEvent[] }>(
        `/api/v1/crews/${encodeURIComponent(crewId)}/messages`
      );
      crewDetail = res.crew;
      messages = res.messages ?? [];
    } catch {
      crewDetail = app.crews.find((c) => c.id === crewId) ?? null;
    } finally {
      loading = false;
    }
  }

  $effect(() => {
    const id = crewId;
    const status = app.crews.find((c) => c.id === id)?.status ?? '';
    if (id !== prevCrewId || (prevStatus === 'active' && status !== '' && status !== 'active')) {
      prevCrewId = id;
      prevStatus = status;
      untrack(() => loadCrew());
    } else if (status) {
      prevStatus = status;
    }
  });

  const liveStream = $derived(app.crewStreams[crewId]);
  const activeLease = $derived(app.leases.find((l) => l.crew_id === crewId));

  async function abortCrew() {
    busy = true;
    try {
      await apiFetch(`/api/v1/crews/${encodeURIComponent(crewId)}/abort`, { method: 'POST', body: '{}' });
      await app.refreshCrews();
      await loadCrew();
      app.notify(`Aborted crew ${crewDetail?.name ?? crewId}`, 'warn');
    } catch (ex) {
      app.notify(ex instanceof Error ? ex.message : 'Abort failed', 'error');
    } finally {
      busy = false;
    }
  }

  async function releaseLock() {
    busy = true;
    try {
      await apiFetch(`/api/v1/crews/${encodeURIComponent(crewId)}/release-lock`, { method: 'POST', body: '{}' });
      await Promise.all([app.refreshCrews(), app.refreshMounts()]);
      await loadCrew();
      app.notify(`Released lease lock for ${crewDetail?.name ?? crewId}`, 'info');
    } catch (ex) {
      app.notify(ex instanceof Error ? ex.message : 'Release lock failed', 'error');
    } finally {
      busy = false;
    }
  }

  function extractPayload(ev: ConversationEvent): { text: string; reasoning: string } {
    try {
      const p = JSON.parse(ev.payload_json) as { content?: string; reasoning_content?: string; tool_calls?: Array<{ function: { name: string } }> };
      let text = p.content ?? '';
      if (!text && Array.isArray(p.tool_calls) && p.tool_calls.length > 0) {
        text = `tool_calls: ${p.tool_calls.map((t) => t.function?.name).join(', ')}`;
      }
      return { text, reasoning: p.reasoning_content ?? '' };
    } catch {
      return { text: ev.payload_json, reasoning: '' };
    }
  }
</script>

<div class="crew-panel">
  <div class="crew-bar">
    <div class="crew-id">
      <span class="pill pill-crew">Crew</span>
      <strong>{liveCrew?.name ?? crewId}</strong>
      <span class="pill" class:pill-rw={liveCrew?.status === 'active'} class:pill-danger={liveCrew?.status === 'aborted'}>
        {liveCrew?.status ?? 'active'}
      </span>
      {#if liveCrew?.skill_name}
        <span class="pill pill-main">skill: {liveCrew.skill_name}</span>
      {/if}
    </div>
    <div class="crew-btns">
      <button class="btn btn-sm" onclick={loadCrew} disabled={loading}>Refresh</button>
      {#if activeLease || (liveCrew?.lease_files?.length ?? 0) > 0}
        <button class="btn btn-sm btn-warn" onclick={releaseLock} disabled={busy}>Release Lease Lock</button>
      {/if}
      {#if liveCrew?.status === 'active'}
        <button class="btn btn-sm btn-danger" onclick={abortCrew} disabled={busy}>Abort Crew</button>
      {/if}
    </div>
  </div>

  {#if activeLease}
    <div class="lease-banner">
      <span class="pill pill-guarded">Lease Lock</span>
      <span class="mono">{activeLease.files.join(', ')}</span>
      <span class="num muted">expires {new Date(activeLease.expires_at).toLocaleTimeString()}</span>
    </div>
  {/if}

  <div class="crew-feed pane-scroll">
    {#if messages.length === 0 && !liveStream?.content && !liveStream?.reasoning}
      <div class="empty-state">
        {loading ? 'Loading Crew transcript…' : 'Crew session initialized; awaiting output…'}
      </div>
    {/if}

    {#each messages as msg (msg.id)}
      {@const parsed = extractPayload(msg)}
      <div class="cmsg" class:user={msg.role === 'user'} class:tool={msg.role === 'tool'}>
        <div class="cmsg-role">
          <span>{msg.role}</span>
          <span class="num muted">{msg.token_est} tok</span>
        </div>
        {#if parsed.reasoning}
          <details class="thought">
            <summary>Thought</summary>
            <div class="thought-body">{parsed.reasoning}</div>
          </details>
        {/if}
        {#if parsed.text}
          <div class="md-body">{@html renderMarkdown(parsed.text)}</div>
        {/if}
      </div>
    {/each}

    {#if liveStream && (liveStream.content || liveStream.reasoning)}
      <div class="cmsg live">
        <div class="cmsg-role"><span>assistant (live)</span></div>
        {#if liveStream.reasoning}
          <details open class="thought">
            <summary>Streaming Thought</summary>
            <div class="thought-body">{liveStream.reasoning}</div>
          </details>
        {/if}
        {#if liveStream.content}
          <div class="md-body">{@html renderMarkdown(liveStream.content)}</div>
        {/if}
      </div>
    {/if}

    {#if liveCrew?.summary}
      <div class="crew-summary card">
        <strong>Summary:</strong> {liveCrew.summary}
      </div>
    {/if}
  </div>
</div>

<style>
  .crew-panel {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
    gap: 8px;
  }
  .crew-bar {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
    padding: 8px 12px;
    border-bottom: 1px solid var(--border-subtle);
  }
  .crew-id,
  .crew-btns {
    display: flex;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
  }
  .lease-banner {
    margin: 0 12px;
    padding: 6px 10px;
    border-radius: var(--radius-sm);
    background: rgba(250, 179, 135, 0.1);
    border: 1px solid rgba(250, 179, 135, 0.35);
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 12px;
    flex-wrap: wrap;
  }
  .muted {
    color: var(--muted);
    font-size: 11px;
  }
  .crew-feed {
    flex: 1;
    padding: 0 12px 12px;
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .cmsg {
    padding: 8px 10px;
    border-radius: var(--radius-sm);
    background: var(--surface0);
    border: 1px solid var(--border-subtle);
  }
  .cmsg.user {
    border-left: 3px solid var(--main);
  }
  .cmsg.tool {
    background: var(--mantle);
    font-size: 12px;
  }
  .cmsg.live {
    border-color: var(--crew);
  }
  .cmsg-role {
    display: flex;
    justify-content: space-between;
    font-size: 11px;
    text-transform: uppercase;
    color: var(--crew);
    margin-bottom: 4px;
    font-weight: 600;
  }
  .thought {
    margin-bottom: 6px;
    font-size: 12px;
    color: var(--subtext);
  }
  .thought-body {
    white-space: pre-wrap;
    padding: 6px;
    background: var(--mantle);
    border-radius: var(--radius-sm);
    margin-top: 4px;
  }
  .crew-summary {
    border-color: var(--crew);
    font-size: 12.5px;
  }
</style>
