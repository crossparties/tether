export type ThemeMode = 'mocha' | 'latte';
export type AppView = 'chat' | 'setup' | 'customize' | 'settings';
export type WorkbenchTab =
  | 'files'
  | 'diffs'
  | 'planner'
  | 'todos'
  | 'terminal'
  | 'usage'
  | 'preview';
export type CustomizeTab = 'mcp' | 'skills' | 'memory';
export type AccessMode = 'rw' | 'guarded' | 'ro' | 'ignored';
export type GovernanceMode = 'code' | 'plan' | 'review';

export interface AuthSession {
  authenticated: boolean;
  csrf_token: string;
  session_id?: string;
  workspace?: string;
  mode?: GovernanceMode;
  branch?: string;
  provider?: string;
  model?: string;
  thread_id?: string;
}

export interface KeyHealthEntry {
  key_id: string;
  provider: string;
  healthy: boolean;
  status_code: number;
  latency_ms: number;
  reason?: string;
  checked_at: string;
  expires_at: string;
}

export interface MaskedKeyView {
  id: string;
  provider: string;
  label?: string;
  base_url?: string;
  model?: string;
  priority: number;
  enabled: boolean;
  has_key: boolean;
  masked_key?: string;
  pinned?: boolean;
  health?: KeyHealthEntry;
}

export interface ProviderPresetChoice {
  Label: string;
  Detail: string;
}

export interface ProviderPresetInfo {
  name: string;
  base_url: string;
  default_model: string;
  env_key?: string;
  models?: ProviderPresetChoice[];
}

export interface SerenaSuggestion {
  recommended: boolean;
  serena_status: string;
  serena_name?: string;
  recommendation?: string;
  reasons?: string[];
  file_count: number;
  dominant_stack?: string;
  suggested_preset?: string;
}

export interface SetupStatusResponse {
  headless: boolean;
  has_global_keys: boolean;
  keys: MaskedKeyView[] | null;
  presets: ProviderPresetInfo[];
  active_provider: string;
  active_model: string;
  pinned_key_id: string;
  workspace: string;
  branch: string;
  mode: GovernanceMode;
  project_ready: boolean;
  has_tether_dir: boolean;
  has_ignore: boolean;
  has_gitignore: boolean;
  project_settings?: Record<string, string>;
  serena_suggestion?: SerenaSuggestion;
}

export interface KBStats {
  TotalFiles: number;
  TotalChunks: number;
}

export interface ProjectInfo {
  workspace: string;
  branch: string;
  session_id: string;
  mode: GovernanceMode;
  settings: Record<string, string>;
  kb_stats: KBStats;
  has_tether_dir: boolean;
  has_ignore_file: boolean;
  serena_suggestion?: SerenaSuggestion;
  provider?: string;
  model?: string;
  pinned_key_id?: string;
  thread_id?: string;
}

export interface ProjectBrowseEntry {
  name: string;
  path: string;
  is_tether_project: boolean;
  is_git_repo: boolean;
}

export interface ProjectBrowseResponse {
  current: string;
  parent: string;
  entries: ProjectBrowseEntry[] | null;
}

export interface LeaseInfo {
  crew_id: string;
  files: string[];
  expires_at: string;
}

export interface IgnoreRule {
  id?: number;
  pattern: string;
  is_regex: boolean;
  is_builtin: boolean;
  enabled: boolean;
  negated: boolean;
  dir_only: boolean;
  invalid?: string;
}

export interface MountedFileView {
  rel_path: string;
  ext: string;
  size_bytes: number;
  size_human: string;
  access_mode: AccessMode;
  source: string;
  lease_crew_id?: string;
  lease_expires_at?: string;
}

export interface MountsListResponse {
  workspace: string;
  mode: GovernanceMode;
  files: MountedFileView[];
  leases: LeaseInfo[] | null;
  builtin_patterns: string[];
  custom_rules: IgnoreRule[] | null;
}

export interface ImpactWarning {
  pattern: string;
  ext: string;
  count: number;
  message: string;
}

export interface SimulationResult {
  total_files: number;
  matched_count: number;
  matched_samples?: string[];
  by_extension?: Record<string, number>;
  warnings?: ImpactWarning[];
  invalid_patterns?: string[];
}

export interface ConversationThread {
  id: string;
  workspace: string;
  git_branch: string;
  title: string;
  mode: string;
  pinned_key_id: string;
  created_at: string;
  updated_at: string;
}

export interface ConversationEvent {
  id: number;
  thread_id: string;
  crew_id?: string;
  seq: number;
  role: string;
  usage_id: string;
  payload_json: string;
  token_est: number;
  created_at: string;
}

export interface ToolCallItem {
  id: string;
  type: string;
  function: {
    name: string;
    arguments: string;
  };
}

export interface PlanOption {
  id: string;
  label: string;
  description?: string;
  recommended?: boolean;
}

export interface ClarificationQuestion {
  id: string;
  question: string;
  options: PlanOption[];
  allow_freeform?: boolean;
  selected_id?: string;
  freeform_answer?: string;
}

export interface ChatTimelineItem {
  id: string;
  kind: 'user' | 'assistant' | 'tool' | 'notice' | 'condensation' | 'error';
  role?: string;
  content: string;
  reasoning?: string;
  toolCalls?: ToolCallItem[];
  toolCallId?: string;
  noticeKind?: string;
  clarifications?: ClarificationQuestion[];
  streaming?: boolean;
  createdAt?: string;
}

export interface PendingApproval {
  id: string;
  tool: string;
  path?: string;
  command?: string;
  label: string;
  preview?: string;
  full_preview?: string;
  scope_hint?: string;
  risk?: string;
  reason?: string;
  force_ask?: boolean;
}

export interface CrewSession {
  id: string;
  parent_thread_id: string;
  name: string;
  skill_name?: string;
  purpose_category?: string;
  status: string;
  lease_files?: string[];
  lease_expires_at?: string;
  summary?: string;
  created_at?: string;
  closed_at?: string;
}

export interface DiffEntryView {
  path: string;
  state: 'modified' | 'created' | string;
  crew_id?: string;
  post_hash?: string;
  actual_hash?: string;
  externally_modified: boolean;
  redacted?: boolean;
  diff?: string;
}

export interface DiffTurnView {
  turn: string;
  crew_id?: string;
  created: string;
  entries: DiffEntryView[];
}

export interface DiffsResponse {
  turns: DiffTurnView[];
  git_status: string;
  git_diff: string;
}

export interface RestoreResult {
  turn: string;
  path: string;
  crew_id?: string;
  restored: boolean;
  conflict?: boolean;
  expected_hash?: string;
  actual_hash?: string;
  error?: string;
}

export interface TodoItem {
  ID: number;
  Description: string;
  Details: string;
  Status: 'pending' | 'completed' | string;
  CreatedAt: string;
  CompletedAt: string;
}

export interface UsageMetrics {
  usage_id: string;
  calls: number;
  prompt_tokens: number;
  cached_tokens: number;
  miss_tokens: number;
  completion_tokens: number;
  last_turn_tokens: number;
  last_cache_hit: number;
  last_cache_miss: number;
  last_ttft_ms: number;
  context_pressure: number;
  cache_hit_ratio: number;
}

export interface TokensResponse {
  by_usage_id: Record<string, UsageMetrics>;
  entries: UsageMetrics[];
  main: UsageMetrics;
  usage_id: string;
  cache_hit_tokens: number;
  context_window: number;
  provider: string;
  model: string;
  pinned_key_id: string;
  summary: string;
}

export interface WaterfallTurnSample {
  turnLabel: string;
  calls: number;
  cached: number;
  miss: number;
  completion: number;
  ttftMs: number;
  timestamp: string;
}

export interface ExternalMCPConfig {
  name?: string;
  transport?: string;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  url?: string;
  headers?: Record<string, string>;
  enabled: boolean;
  preset?: string;
  enabled_tools?: string[];
  disabled_tools?: string[];
  scope?: string;
}

export interface ExternalMCPTool {
  name: string;
  server_name?: string;
  original_name?: string;
  description?: string;
  read_only?: boolean;
  read_only_hint?: boolean;
}

export interface ServerTestResult {
  name: string;
  scope: string;
  transport: string;
  preset: string;
  ok: boolean;
  latency_ms: number;
  raw_tool_count: number;
  filtered_tool_count: number;
  tools?: ExternalMCPTool[];
  warnings?: string[];
  error?: string;
}

export interface SkillView {
  name: string;
  rel_path: string;
  description?: string;
  content: string;
  bytes: number;
}

export interface DecisionItem {
  id: number;
  topic: string;
  decision: string;
  reason: string;
  created_at: string;
}

export interface PatternItem {
  id: number;
  type: string;
  description: string;
  created_at: string;
}

export interface ApprovalGrant {
  id: number;
  session_id: string;
  path_scope: string;
  kind: string;
  created_at: string;
  expires_at: string;
}

export interface SettingsResponse {
  mode: GovernanceMode;
  project_settings: Record<string, string>;
  global: Record<string, unknown>;
  provider?: string;
  model?: string;
  pinned_key_id?: string;
  context_window?: number;
}

export interface DoctorCheck {
  section: string;
  status: 'pass' | 'warn' | 'fail' | 'info' | 'fixed' | string;
  label: string;
  value: string;
}

export interface DoctorReport {
  workspace: string;
  fix: boolean;
  checks: DoctorCheck[];
  passed: number;
  warnings: number;
  failed: number;
  text: string;
}
