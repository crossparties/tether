<script lang="ts">
  import { app } from '../../lib/store.svelte';

  let showCompleted = $state(true);

  const visible = $derived.by(() => {
    if (showCompleted) return app.todos;
    return app.todos.filter((t) => t.Status !== 'completed');
  });

  const pendingCount = $derived(app.todos.filter((t) => t.Status !== 'completed').length);
  const doneCount = $derived(app.todos.filter((t) => t.Status === 'completed').length);
</script>

<div class="todos-tab">
  <div class="td-head">
    <div class="td-counts">
      <span class="pill pill-plan num">{pendingCount} pending</span>
      <span class="pill pill-rw num">{doneCount} done</span>
    </div>
    <div class="td-acts">
      <label class="chk">
        <input type="checkbox" bind:checked={showCompleted} />
        <span>Show completed</span>
      </label>
      <button class="btn btn-sm" onclick={() => app.refreshTodos()}>Refresh</button>
    </div>
  </div>

  <div class="td-list pane-scroll">
    {#if visible.length === 0}
      <div class="empty-state">No todos recorded in this session.</div>
    {:else}
      {#each visible as todo (todo.ID)}
        <div class="card td-item" class:done={todo.Status === 'completed'}>
          <div class="td-row">
            <span class="pill num" class:pill-rw={todo.Status === 'completed'} class:pill-plan={todo.Status !== 'completed'}>
              #{todo.ID} {todo.Status}
            </span>
            <span class="td-desc">{todo.Description}</span>
          </div>
          {#if todo.Details}
            <div class="td-details">{todo.Details}</div>
          {/if}
        </div>
      {/each}
    {/if}
  </div>
</div>

<style>
  .todos-tab {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
    gap: 8px;
  }
  .td-head,
  .td-counts,
  .td-acts {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .chk {
    display: flex;
    align-items: center;
    gap: 5px;
    font-size: 12px;
    color: var(--subtext);
  }
  .td-list {
    flex: 1;
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .td-item {
    padding: 8px 10px;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .td-item.done {
    opacity: 0.72;
  }
  .td-row {
    display: flex;
    align-items: flex-start;
    gap: 8px;
  }
  .td-desc {
    font-weight: 500;
  }
  .td-details {
    font-size: 12px;
    color: var(--subtext);
  }
</style>
