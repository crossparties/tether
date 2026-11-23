import { ApiError, apiFetch, setCsrfToken } from './api';
import { extractClarificationQuestions } from './markdown';
import type {
  AppView,
  AuthSession,
  ChatTimelineItem,
  ConversationEvent,
  ConversationThread,
  CrewSession,
  CustomizeTab,
  DiffsResponse,
  GovernanceMode,
  LeaseInfo,
  MountsListResponse,
  PendingApproval,
  ProjectInfo,
  SetupStatusResponse,
  ThemeMode,
  TodoItem,
  TokensResponse,
  UsageMetrics,
  WaterfallTurnSample,
  WorkbenchTab,
} from './types';

function parseEventToTimeline(ev: ConversationEvent): ChatTimelineItem | null {
  if (ev.crew_id) {
    return null;
  }
  let payload: Record<string, unknown> = {};
  try {
    payload = JSON.parse(ev.payload_json) as Record<string, unknown>;
  } catch {
    payload = { content: ev.payload_json };
  }

  if (ev.role === 'condensation') {
    const count = Number(payload.condensed_count ?? 0);
    const reason = String(payload.reason ?? 'threshold');
    const summary = String(payload.summary ?? '');
    return {
      id: `ev-${ev.id}`,
      kind: 'condensation',
      content: summary,
      noticeKind: `EpisodicCondenser (${count} events, ${reason})`,
      createdAt: ev.created_at,
    };
  }

  const content = typeof payload.content === 'string' ? payload.content : '';
  const reasoning = typeof payload.reasoning_content === 'string' ? payload.reasoning_content : '';
  const toolCalls = Array.isArray(payload.tool_calls) ? (payload.tool_calls as ChatTimelineItem['toolCalls']) : undefined;
  const toolCallId = typeof payload.tool_call_id === 'string' ? payload.tool_call_id : undefined;

  if (ev.role === 'user') {
    return {
      id: `ev-${ev.id}`,
      kind: 'user',
      role: 'user',
      content,
      createdAt: ev.created_at,
    };
  }
  if (ev.role === 'tool') {
    return {
      id: `ev-${ev.id}`,
      kind: 'tool',
      role: 'tool',
      content,
      toolCallId,
      createdAt: ev.created_at,
    };
  }
  return {
    id: `ev-${ev.id}`,
    kind: 'assistant',
    role: 'assistant',
    content,
    reasoning: reasoning || undefined,
    toolCalls,
    clarifications: extractClarificationQuestions(content),
    createdAt: ev.created_at,
  };
}

class TetherStore {
  theme = $state<ThemeMode>('mocha');
  authenticated = $state(false);
  authRequired = $state(false);
  bootstrapping = $state(true);

  activeView = $state<AppView>('chat');
  workbenchTab = $state<WorkbenchTab>('files');
  customizeTab = $state<CustomizeTab>('mcp');
  drawerOpen = $state(true);

  commandPaletteOpen = $state(false);
  projectPickerOpen = $state(false);
  btwOpen = $state(false);

  sseConnected = $state(false);
  toast = $state<{ id: number; kind: 'info' | 'success' | 'warn' | 'error'; text: string } | null>(null);

  session = $state<AuthSession | null>(null);
  project = $state<ProjectInfo | null>(null);
  setupStatus = $state<SetupStatusResponse | null>(null);
  mode = $state<GovernanceMode>('code');

  // Threads & Main Chat
  threads = $state<ConversationThread[]>([]);
  activeThreadId = $state('');
  timeline = $state<ChatTimelineItem[]>([]);
  turnActive = $state(false);
  activeTurnId = $state('');
  queuedPrompts = $state<string[]>([]);
  pendingApprovals = $state<PendingApproval[]>([]);

  // Active Crew Sub-Tab
  chatSubTab = $state<'main' | string>('main'); // 'main' or crew_id
  crews = $state<CrewSession[]>([]);
  leases = $state<LeaseInfo[]>([]);
  crewStreams = $state<Record<string, { content: string; reasoning: string }>>({});

  // /btw Side-Chat
  btwHistory = $state<Array<{ id: string; question: string; answer: string; usage?: UsageMetrics; error?: string }>>([]);
  btwBusy = $state(false);
  btwUsage = $state<UsageMetrics | null>(null);

  // Workbench Data
  mounts = $state<MountsListResponse | null>(null);
  diffs = $state<DiffsResponse | null>(null);
  todos = $state<TodoItem[]>([]);
  tokens = $state<TokensResponse | null>(null);
  waterfall = $state<WaterfallTurnSample[]>([]);
  planContent = $state('');

  // Incrementing counters for child editors / viewers to react cleanly
  planVersion = $state(0);
  diffsVersion = $state(0);
  mountsVersion = $state(0);

  private es: EventSource | null = null;
  private toastSeq = 0;
  private timelineSeq = 0;
  private prevMainCalls = 0;
  private prevMainCached = 0;
  private prevMainMiss = 0;
  private prevMainCompletion = 0;

  constructor() {
    const saved = typeof localStorage !== 'undefined' ? localStorage.getItem('tether_theme') : null;
    if (saved === 'latte' || saved === 'mocha') {
      this.applyTheme(saved, false);
    } else {
      this.applyTheme('mocha', false);
    }
  }

  applyTheme(next: ThemeMode, persist = true) {
    this.theme = next;
    if (typeof document !== 'undefined') {
      document.documentElement.dataset.theme = next;
    }
    if (persist && typeof localStorage !== 'undefined') {
      localStorage.setItem('tether_theme', next);
    }
  }

  toggleTheme() {
    this.applyTheme(this.theme === 'mocha' ? 'latte' : 'mocha');
  }

  notify(text: string, kind: 'info' | 'success' | 'warn' | 'error' = 'info') {
    const id = ++this.toastSeq;
    this.toast = { id, kind, text };
    setTimeout(() => {
      if (this.toast?.id === id) {
        this.toast = null;
      }
    }, 4200);
  }

  async init() {
    this.bootstrapping = true;
    try {
      const sess = await apiFetch<AuthSession>('/api/v1/auth/session');
      setCsrfToken(sess.csrf_token);
      this.session = sess;
      this.authenticated = true;
      this.authRequired = false;
      if (sess.mode) this.mode = sess.mode;
      if (sess.thread_id) this.activeThreadId = sess.thread_id;

      await this.refreshAll(true);
      this.connectSSE();
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        this.authenticated = false;
        this.authRequired = true;
      } else {
        this.notify(err instanceof Error ? err.message : 'Failed to connect to Tether server', 'error');
      }
    } finally {
      this.bootstrapping = false;
    }
  }

  async refreshAll(isInitial = false) {
    const [proj, setup, threadsRes, mountsRes, crewsRes, todosRes, tokensRes, planRes, settingsRes] = await Promise.all([
      apiFetch<ProjectInfo>('/api/v1/project').catch(() => null),
      apiFetch<SetupStatusResponse>('/api/v1/setup/status').catch(() => null),
      apiFetch<{ active_thread_id: string; threads: ConversationThread[] }>('/api/v1/threads').catch(() => null),
      apiFetch<MountsListResponse>('/api/v1/mounts').catch(() => null),
      apiFetch<{ crews: CrewSession[]; leases: LeaseInfo[] }>('/api/v1/crews?all=1').catch(() => null),
      apiFetch<{ todos: TodoItem[] }>('/api/v1/todos?all=1').catch(() => null),
      apiFetch<TokensResponse>('/api/v1/tokens').catch(() => null),
      apiFetch<{ content: string; mode: GovernanceMode }>('/api/v1/plan').catch(() => null),
      apiFetch<{ global?: Record<string, unknown> }>('/api/v1/settings').catch(() => null),
    ]);

    if (proj) {
      this.project = proj;
      this.mode = proj.mode;
    }
    if (setup) {
      this.setupStatus = setup;
      // Show Setup automatically only during initial load when no keys exist
      if (isInitial && !setup.has_global_keys && this.timeline.length === 0) {
        this.activeView = 'setup';
      }
    }
    if (threadsRes) {
      this.threads = threadsRes.threads ?? [];
      if (threadsRes.active_thread_id) {
        this.activeThreadId = threadsRes.active_thread_id;
        await this.loadThreadEvents(this.activeThreadId);
      }
    }
    if (mountsRes) {
      this.mounts = mountsRes;
      this.mountsVersion++;
    }
    if (crewsRes) {
      this.crews = crewsRes.crews ?? [];
      this.leases = crewsRes.leases ?? [];
    }
    if (todosRes) this.todos = todosRes.todos ?? [];
    if (tokensRes) this.ingestTokens(tokensRes);
    if (planRes) {
      this.planContent = planRes.content ?? '';
      this.planVersion++;
    }
    const savedTheme = settingsRes?.global?.theme;
    if (savedTheme === 'latte' || savedTheme === 'mocha') {
      this.applyTheme(savedTheme);
    }
  }

  private ingestTokens(res: TokensResponse) {
    this.tokens = res;
    const m = res.main;
    if (m && m.calls > this.prevMainCalls) {
      const dCached = Math.max(0, m.cached_tokens - this.prevMainCached);
      const dMiss = Math.max(0, m.miss_tokens - this.prevMainMiss);
      const dComp = Math.max(0, m.completion_tokens - this.prevMainCompletion);
      const sample: WaterfallTurnSample = {
        turnLabel: `Call #${m.calls}`,
        calls: m.calls,
        cached: dCached > 0 || dMiss > 0 ? dCached : m.last_cache_hit,
        miss: dCached > 0 || dMiss > 0 ? dMiss : m.last_cache_miss,
        completion: dComp,
        ttftMs: m.last_ttft_ms,
        timestamp: new Date().toLocaleTimeString([], { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' }),
      };
      this.waterfall = [...this.waterfall.slice(-23), sample];
      this.prevMainCalls = m.calls;
      this.prevMainCached = m.cached_tokens;
      this.prevMainMiss = m.miss_tokens;
      this.prevMainCompletion = m.completion_tokens;
    }
  }

  async refreshTokens() {
    const res = await apiFetch<TokensResponse>('/api/v1/tokens').catch(() => null);
    if (res) this.ingestTokens(res);
  }

  async refreshCrews() {
    const res = await apiFetch<{ crews: CrewSession[]; leases: LeaseInfo[] }>('/api/v1/crews?all=1').catch(() => null);
    if (res) {
      this.crews = res.crews ?? [];
      this.leases = res.leases ?? [];
    }
  }

  async refreshMounts(q = '') {
    const qs = q ? `?q=${encodeURIComponent(q)}` : '';
    const res = await apiFetch<MountsListResponse>(`/api/v1/mounts${qs}`).catch(() => null);
    if (res) {
      this.mounts = res;
      this.mountsVersion++;
    }
  }

  async refreshDiffs() {
    const res = await apiFetch<DiffsResponse>('/api/v1/diffs').catch(() => null);
    if (res) {
      this.diffs = res;
    }
  }

  async refreshTodos() {
    const res = await apiFetch<{ todos: TodoItem[] }>('/api/v1/todos?all=1').catch(() => null);
    if (res) this.todos = res.todos ?? [];
  }

  async refreshPlan() {
    const res = await apiFetch<{ content: string; mode: GovernanceMode }>('/api/v1/plan').catch(() => null);
    if (res) {
      this.planContent = res.content ?? '';
      this.mode = res.mode ?? this.mode;
      this.planVersion++;
    }
  }

  async refreshThreads() {
    const res = await apiFetch<{ active_thread_id: string; threads: ConversationThread[] }>('/api/v1/threads').catch(() => null);
    if (res) {
      this.threads = res.threads ?? [];
      if (res.active_thread_id) this.activeThreadId = res.active_thread_id;
    }
  }

  async loadThreadEvents(threadID: string) {
    if (!threadID) return;
    const res = await apiFetch<{ thread: ConversationThread; events: ConversationEvent[] }>(
      `/api/v1/threads/${encodeURIComponent(threadID)}/events`
    ).catch(() => null);
    if (!res) return;
    const items: ChatTimelineItem[] = [];
    for (const ev of res.events ?? []) {
      const parsed = parseEventToTimeline(ev);
      if (parsed) items.push(parsed);
    }
    this.timeline = items;
  }

  async selectThread(threadID: string) {
    if (this.turnActive) {
      this.notify('Finish or abort the active turn before switching threads', 'warn');
      return;
    }
    await apiFetch<ConversationThread>('/api/v1/threads', {
      method: 'POST',
      body: JSON.stringify({ id: threadID }),
    });
    this.activeThreadId = threadID;
    this.chatSubTab = 'main';
    await this.loadThreadEvents(threadID);
    await this.refreshCrews();
  }

  async createThread(title = '') {
    if (this.turnActive) {
      this.notify('Finish or abort the active turn before creating a thread', 'warn');
      return;
    }
    const created = await apiFetch<ConversationThread>('/api/v1/threads', {
      method: 'POST',
      body: JSON.stringify({ title: title.trim() || undefined, mode: this.mode }),
    });
    this.activeThreadId = created.id;
    this.timeline = [];
    this.chatSubTab = 'main';
    await this.refreshThreads();
  }

  async switchMode(next: GovernanceMode) {
    await apiFetch('/api/v1/settings', {
      method: 'PUT',
      body: JSON.stringify({ mode: next }),
    });
    this.mode = next;
    if (this.project) this.project = { ...this.project, mode: next };
    this.notify(`Switched to [${next.toUpperCase()}] mode`, 'info');
  }

  async sendChat(rawPrompt: string) {
    const prompt = rawPrompt.trim();
    if (!prompt) return;

    // Handle local slash-command ergonomics cleanly while preserving backend parity
    if (prompt === '/compact') {
      await this.compactNow();
      return;
    }
    if (prompt.startsWith('/btw ')) {
      this.btwOpen = true;
      await this.sendBtw(prompt.slice(5));
      return;
    }
    if (prompt === '/btw') {
      this.btwOpen = true;
      return;
    }
    if (prompt.startsWith('/mode ')) {
      const target = prompt.slice(6).trim().toLowerCase();
      if (target === 'code' || target === 'plan' || target === 'review') {
        await this.switchMode(target);
        return;
      }
    }

    if (this.turnActive) {
      this.queuedPrompts = [...this.queuedPrompts, prompt];
      this.notify('Turn running — queued follow-up message', 'info');
      return;
    }

    await this.dispatchChatPrompt(prompt);
  }

  private async dispatchChatPrompt(prompt: string) {
    this.timeline = [
      ...this.timeline,
      {
        id: `local-u-${++this.timelineSeq}`,
        kind: 'user',
        role: 'user',
        content: prompt,
      },
    ];
    this.turnActive = true;
    try {
      const res = await apiFetch<{ accepted: boolean; turn_id: string; thread_id: string }>('/api/v1/chat/send', {
        method: 'POST',
        body: JSON.stringify({ prompt, thread_id: this.activeThreadId || undefined }),
      });
      this.activeTurnId = res.turn_id;
      if (res.thread_id) this.activeThreadId = res.thread_id;
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        // Active turn started concurrently; queue cleanly
        this.queuedPrompts = [...this.queuedPrompts, prompt];
        return;
      }
      this.turnActive = false;
      this.timeline = [
        ...this.timeline,
        {
          id: `err-${++this.timelineSeq}`,
          kind: 'error',
          content: err instanceof Error ? err.message : 'Failed to send prompt',
        },
      ];
    }
  }

  removeQueuedPrompt(index: number) {
    this.queuedPrompts = this.queuedPrompts.filter((_, i) => i !== index);
  }

  async abortChat() {
    this.queuedPrompts = [];
    await apiFetch('/api/v1/chat/abort', { method: 'POST', body: JSON.stringify({}) });
  }

  async compactNow() {
    const res = await apiFetch<{ condensed: boolean; condensed_count: number; remaining_count: number; reason?: string }>(
      '/api/v1/chat/compact',
      { method: 'POST', body: JSON.stringify({ force: true }) }
    );
    if (res.condensed) {
      this.notify(`Condensed ${res.condensed_count} events (${res.remaining_count} remaining)`, 'success');
      await this.refreshTokens();
      if (this.activeThreadId) await this.loadThreadEvents(this.activeThreadId);
    } else {
      this.notify('Conversation is already compact', 'info');
    }
  }

  async submitApproval(id: string, verdict: 'once' | 'folder' | 'session' | 'reject', reason = '') {
    await apiFetch('/api/v1/chat/approval', {
      method: 'POST',
      body: JSON.stringify({ id, verdict, reason }),
    });
    this.pendingApprovals = this.pendingApprovals.filter((p) => p.id !== id);
  }

  async sendBtw(question: string) {
    const q = question.trim();
    if (!q || this.btwBusy) return;
    this.btwBusy = true;
    const entryId = `btw-${++this.timelineSeq}`;
    this.btwHistory = [...this.btwHistory, { id: entryId, question: q, answer: '' }];
    try {
      const res = await apiFetch<{ question: string; answer: string; usage: UsageMetrics }>('/api/v1/chat/btw', {
        method: 'POST',
        body: JSON.stringify({ question: q }),
      });
      this.btwUsage = res.usage;
      this.btwHistory = this.btwHistory.map((item) =>
        item.id === entryId ? { ...item, answer: res.answer, usage: res.usage } : item
      );
      await this.refreshTokens();
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'Side-chat error';
      this.btwHistory = this.btwHistory.map((item) =>
        item.id === entryId ? { ...item, error: msg } : item
      );
    } finally {
      this.btwBusy = false;
    }
  }

  async promoteBtw(question: string, answer: string) {
    await apiFetch('/api/v1/chat/btw/promote', {
      method: 'POST',
      body: JSON.stringify({ question, answer }),
    });
    this.notify('Promoted /btw Q&A to Main thread tail', 'success');
    if (this.activeThreadId) await this.loadThreadEvents(this.activeThreadId);
  }

  async pinBtw(topic: string, decision: string, reason = '') {
    await apiFetch('/api/v1/chat/btw/pin', {
      method: 'POST',
      body: JSON.stringify({ topic: topic || 'btw', decision, reason }),
    });
    this.notify('Pinned decision to .tether/session.db', 'success');
  }

  async openProject(path: string, init: boolean) {
    const proj = await apiFetch<ProjectInfo>('/api/v1/project/open', {
      method: 'POST',
      body: JSON.stringify({ path, init }),
    });
    this.project = proj;
    this.mode = proj.mode;
    this.waterfall = [];
    this.prevMainCalls = 0;
    this.prevMainCached = 0;
    this.prevMainMiss = 0;
    this.prevMainCompletion = 0;
    await this.refreshAll();
    this.diffsVersion++;
    this.notify(`Opened workspace ${proj.workspace}`, 'success');
  }

  connectSSE() {
    if (this.es) {
      this.es.close();
      this.es = null;
    }
    const es = new EventSource('/api/v1/events');
    this.es = es;

    es.addEventListener('connected', () => {
      this.sseConnected = true;
    });

    es.onerror = () => {
      this.sseConnected = false;
    };

    es.addEventListener('chat.delta', (ev) => {
      const d = safeJSON<{ thread_id?: string; turn_id?: string; kind?: string; text?: string }>(ev.data);
      if (!d) return;
      const tid = d.turn_id || this.activeTurnId || 'live';
      const streamId = `stream-${tid}`;
      const existingIdx = this.timeline.findIndex((m) => m.id === streamId);

      if (d.kind === 'endstep') {
        if (existingIdx >= 0) {
          this.timeline[existingIdx] = {
            ...this.timeline[existingIdx],
            streaming: false,
            clarifications: extractClarificationQuestions(this.timeline[existingIdx].content),
          };
        }
        return;
      }

      if (existingIdx < 0) {
        this.timeline = [
          ...this.timeline,
          {
            id: streamId,
            kind: 'assistant',
            role: 'assistant',
            content: d.kind === 'content' ? (d.text ?? '') : '',
            reasoning: d.kind === 'reasoning' ? (d.text ?? '') : '',
            streaming: true,
          },
        ];
      } else {
        const cur = this.timeline[existingIdx];
        const nextContent = d.kind === 'content' ? cur.content + (d.text ?? '') : cur.content;
        const nextReasoning = d.kind === 'reasoning' ? (cur.reasoning ?? '') + (d.text ?? '') : cur.reasoning;
        this.timeline[existingIdx] = {
          ...cur,
          content: nextContent,
          reasoning: nextReasoning,
        };
      }
    });

    es.addEventListener('chat.done', async (ev) => {
      const d = safeJSON<{ thread_id?: string; turn_id?: string; reply?: string }>(ev.data);
      const streamId = `stream-${d?.turn_id || this.activeTurnId || 'live'}`;
      const idx = this.timeline.findIndex((m) => m.id === streamId);
      if (idx >= 0) {
        const finalText = this.timeline[idx].content || d?.reply || '';
        this.timeline[idx] = {
          ...this.timeline[idx],
          content: finalText,
          streaming: false,
          clarifications: extractClarificationQuestions(finalText),
        };
      } else if (d?.reply) {
        this.timeline = [
          ...this.timeline,
          {
            id: `done-${++this.timelineSeq}`,
            kind: 'assistant',
            role: 'assistant',
            content: d.reply,
            clarifications: extractClarificationQuestions(d.reply),
          },
        ];
      }

      this.turnActive = false;
      this.activeTurnId = '';
      await Promise.all([this.refreshTokens(), this.refreshCrews(), this.refreshThreads()]);
      if (this.activeThreadId) {
        // Reload persisted thread events cleanly so tool calls & condensed history align 1-to-1
        await this.loadThreadEvents(this.activeThreadId);
      }
      if (this.queuedPrompts.length > 0) {
        const [next, ...rest] = this.queuedPrompts;
        this.queuedPrompts = rest;
        await this.dispatchChatPrompt(next);
      }
    });

    es.addEventListener('chat.error', async (ev) => {
      const d = safeJSON<{ turn_id?: string; error?: string }>(ev.data);
      const streamId = `stream-${d?.turn_id || this.activeTurnId || 'live'}`;
      this.timeline = this.timeline.map((m) => (m.id === streamId ? { ...m, streaming: false } : m));
      this.timeline = [
        ...this.timeline,
        {
          id: `err-${++this.timelineSeq}`,
          kind: 'error',
          content: d?.error || 'Turn failed',
        },
      ];
      this.turnActive = false;
      this.activeTurnId = '';
      await this.refreshTokens();
    });

    for (const noticeKind of ['keys.switched', 'stream.retry_reset', 'stuck_detection'] as const) {
      es.addEventListener(noticeKind, (ev) => {
        const d = safeJSON<{ detail?: string }>(ev.data);
        const detail = d?.detail || ev.data;
        this.timeline = [
          ...this.timeline,
          {
            id: `notice-${noticeKind}-${++this.timelineSeq}`,
            kind: 'notice',
            noticeKind,
            content: detail,
          },
        ];
      });
    }

    es.addEventListener('condenser.ran', async (ev) => {
      const d = safeJSON<{ reason?: string; condensed_count?: number; remaining_count?: number; summary?: string }>(ev.data);
      this.timeline = [
        ...this.timeline,
        {
          id: `condense-${++this.timelineSeq}`,
          kind: 'condensation',
          noticeKind: `EpisodicCondenser (${d?.condensed_count ?? 0} → ${d?.remaining_count ?? 0}, ${d?.reason ?? 'auto'})`,
          content: d?.summary ?? '',
        },
      ];
      await this.refreshTokens();
    });

    es.addEventListener('approval.required', (ev) => {
      const apr = safeJSON<PendingApproval>(ev.data);
      if (!apr?.id) return;
      if (!this.pendingApprovals.some((p) => p.id === apr.id)) {
        this.pendingApprovals = [...this.pendingApprovals, apr];
      }
    });

    es.addEventListener('approval.resolved', (ev) => {
      const d = safeJSON<{ id?: string }>(ev.data);
      if (d?.id) {
        this.pendingApprovals = this.pendingApprovals.filter((p) => p.id !== d.id);
      }
    });

    es.addEventListener('crew.status', async (ev) => {
      const d = safeJSON<{ crew_id?: string; status?: string }>(ev.data);
      await this.refreshCrews();
      if (d?.crew_id && d.status && d.status !== 'active' && d.crew_id in this.crewStreams) {
        const { [d.crew_id]: _removed, ...rest } = this.crewStreams;
        this.crewStreams = rest;
      }
    });

    es.addEventListener('crew.delta', (ev) => {
      const d = safeJSON<{ crew_id?: string; kind?: string; text?: string }>(ev.data);
      if (!d?.crew_id) return;
      const cur = this.crewStreams[d.crew_id] ?? { content: '', reasoning: '' };
      if (d.kind === 'content') {
        this.crewStreams = {
          ...this.crewStreams,
          [d.crew_id]: { ...cur, content: cur.content + (d.text ?? '') },
        };
      } else if (d.kind === 'reasoning') {
        this.crewStreams = {
          ...this.crewStreams,
          [d.crew_id]: { ...cur, reasoning: cur.reasoning + (d.text ?? '') },
        };
      }
    });

    es.addEventListener('lease.updated', async () => {
      await Promise.all([this.refreshCrews(), this.refreshMounts()]);
    });

    es.addEventListener('diff.updated', () => {
      this.diffsVersion++;
    });

    es.addEventListener('todo.updated', async () => {
      await this.refreshTodos();
    });

    es.addEventListener('mount.updated', async () => {
      await this.refreshMounts();
    });

    es.addEventListener('plan.updated', async () => {
      await this.refreshPlan();
    });
  }

  disconnectSSE() {
    if (this.es) {
      this.es.close();
      this.es = null;
    }
    this.sseConnected = false;
  }
}

function safeJSON<T>(raw: string): T | null {
  try {
    return JSON.parse(raw) as T;
  } catch {
    return null;
  }
}

export const app = new TetherStore();
