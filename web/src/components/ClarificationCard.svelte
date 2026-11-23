<script lang="ts">
  import { untrack } from 'svelte';
  import { apiFetch } from '../lib/api';
  import { app } from '../lib/store.svelte';
  import type { ClarificationQuestion } from '../lib/types';

  let { questions }: { questions: ClarificationQuestion[] } = $props();

  let selections = $state<Record<string, { selectedId: string; freeform: string }>>({});
  let busy = $state(false);
  let saved = $state(false);

  $effect(() => {
    const prev = untrack(() => selections);
    const next: Record<string, { selectedId: string; freeform: string }> = {};
    for (const q of questions) {
      const rec = q.options.find((o) => o.recommended)?.id ?? q.options[0]?.id ?? '';
      next[q.id] = {
        selectedId: prev[q.id]?.selectedId ?? q.selected_id ?? rec,
        freeform: prev[q.id]?.freeform ?? q.freeform_answer ?? '',
      };
    }
    selections = next;
  });

  function payloadQuestions(): ClarificationQuestion[] {
    return questions.map((q) => ({
      ...q,
      selected_id: selections[q.id]?.selectedId ?? '',
      freeform_answer: selections[q.id]?.freeform ?? '',
    }));
  }

  async function saveToPlan() {
    busy = true;
    try {
      await apiFetch('/api/v1/plan', {
        method: 'PUT',
        body: JSON.stringify({ questions: payloadQuestions() }),
      });
      saved = true;
      await app.refreshPlan();
      app.notify('Appended clarification decisions to .tether/PLAN.md', 'success');
    } catch (ex) {
      app.notify(ex instanceof Error ? ex.message : 'Failed to save clarification decisions', 'error');
    } finally {
      busy = false;
    }
  }

  async function sendToChat() {
    const lines = payloadQuestions().map((q) => {
      const picked = q.options.find((o) => o.id === q.selected_id);
      const ans = q.freeform_answer?.trim() || (picked ? `${picked.label}${picked.description ? ` — ${picked.description}` : ''}` : q.selected_id);
      return `- **${q.question}**: ${ans}`;
    });
    await saveToPlan();
    await app.sendChat(`Plan clarifications selected:\n${lines.join('\n')}`);
  }
</script>

<div class="clarify card">
  <div class="clarify-head">
    <span class="pill pill-plan">Plan Clarification</span>
    {#if saved}<span class="pill pill-rw">Saved to .tether/PLAN.md</span>{/if}
  </div>

  {#each questions as q (q.id)}
    <div class="q-block">
      <div class="q-title">{q.question}</div>
      <div class="opt-grid">
        {#each q.options as opt (opt.id)}
          {@const active = selections[q.id]?.selectedId === opt.id}
          <button
            type="button"
            class="opt-btn"
            class:active
            onclick={() => {
              selections = { ...selections, [q.id]: { selectedId: opt.id, freeform: selections[q.id]?.freeform ?? '' } };
            }}
          >
            <div class="opt-top">
              <span class="opt-label">{opt.label}</span>
              {#if opt.recommended}
                <span class="pill pill-rw">Recommended</span>
              {/if}
            </div>
            {#if opt.description}
              <div class="opt-desc">{opt.description}</div>
            {/if}
          </button>
        {/each}
      </div>
      {#if q.allow_freeform !== false}
        <input
          class="input freeform"
          type="text"
          placeholder="Optional custom note / override…"
          value={selections[q.id]?.freeform ?? ''}
          oninput={(e) => {
            const val = (e.currentTarget as HTMLInputElement).value;
            selections = { ...selections, [q.id]: { selectedId: selections[q.id]?.selectedId ?? '', freeform: val } };
          }}
        />
      {/if}
    </div>
  {/each}

  <div class="clarify-actions">
    <button class="btn btn-sm" disabled={busy} onclick={saveToPlan}>Save to PLAN.md</button>
    <button class="btn btn-sm btn-primary" disabled={busy} onclick={sendToChat}>Save & Send Answers</button>
  </div>
</div>

<style>
  .clarify {
    margin-top: 8px;
    border-color: rgba(249, 226, 175, 0.45);
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .clarify-head {
    display: flex;
    gap: 8px;
    align-items: center;
  }
  .q-block {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .q-title {
    font-weight: 600;
    font-size: 13px;
    color: var(--text);
  }
  .opt-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(190px, 1fr));
    gap: 6px;
  }
  .opt-btn {
    text-align: left;
    padding: 8px 10px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border-subtle);
    background: var(--mantle);
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .opt-btn.active {
    border-color: var(--plan);
    background: rgba(249, 226, 175, 0.12);
  }
  .opt-top {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 6px;
  }
  .opt-label {
    font-weight: 600;
    font-size: 12.5px;
    color: var(--text);
  }
  .opt-desc {
    font-size: 11.5px;
    color: var(--subtext);
  }
  .freeform {
    font-size: 12px;
    padding: 5px 8px;
  }
  .clarify-actions {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
  }
</style>
