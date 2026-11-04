# Implementation Plan: Tether v0.2.0 — Universal MCP Adapter & Stdlib Micro-Harness

> **Methodology**: OKF (Open Knowledge Fact-finding) & Ponytail Minimalist Discipline  
> **Zero External Dependencies**: Pure Go standard library (`net/http`, `os/exec`) + existing `modernc.org/sqlite` substrate.

---

## 1. OKF & Ponytail Architectural Framing

### 1.1 What The Problem Is
1. **Incomplete Agent Coverage**: `tether install` currently wires `agy`, `claude`, `codex`, and `cursor`, leaving out popular MCP-native agents (`windsurf`, `cline`, `roo`, `opencode`, and `all`).
2. **Raw API Gap**: Developers holding a raw LLM API key (e.g., `DEEPSEEK_API_KEY` for `deepseek-chat` / `deepseek-reasoner`, OpenRouter, Groq, OpenAI, or local Ollama) cannot execute tasks directly against the Tether substrate without installing an external third-party harness.

### 1.2 Why This Design Was Chosen (Ponytail Minimalist Discipline)
- *"The best code is code never written."* Rather than cloning a bloated TUI/IDE agent, Tether provides a **<200 LOC surgical micro-harness (`tether run`)** that reuses 100% of Tether's existing `hooks.Handler` (`PreInvocation`, `PreToolUse`, `PostToolUse`) and `mcp.Server` tool dispatcher.
- **Zero New Dependencies**: Uses standard `net/http` against the industry-standard OpenAI-compatible `/chat/completions` contract supported by DeepSeek, OpenAI, OpenRouter, Groq, Gemini, xAI, and Ollama.
- **DeepSeek R1 (`deepseek-reasoner`) Native Compatibility**: Properly handles `reasoning_content` in assistant tool-call turns while stripping historical `reasoning_content` on subsequent user turns as mandated by the DeepSeek API specification.

---

## 2. Surgical Changes Breakdown

### Component 1: Unified Tool Dispatcher & 4 OS Primitives (`internal/mcp/server.go` & `internal/hooks/handler.go`)
- Expose `ListTools() []Tool` and `CallTool(name string, args json.RawMessage) (string, bool)` on `*mcp.Server`.
- Add 4 minimal workspace primitives to `mcp.Server` (`read_file`, `write_file`, `edit_file`, `run_command`) so raw APIs in `tether run` (and any minimal MCP client) can inspect and mutate the workspace.
- Register `edit_file` in `hooks.Handler` write-gating maps so `PLAN` mode and subagent restrictions physically block unauthorized mutations.

### Component 2: Stdlib Universal API Micro-Harness (`internal/harness/runner.go`)
- Provider presets:
  - `deepseek` (`https://api.deepseek.com/chat/completions`, default model: `deepseek-chat`)
  - `openrouter` (`https://openrouter.ai/api/v1/chat/completions`, default model: `deepseek/deepseek-chat`)
  - `openai` (`https://api.openai.com/v1/chat/completions`, default model: `gpt-4o`)
  - `groq` (`https://api.groq.com/openai/v1/chat/completions`, default model: `llama-3.3-70b-versatile`)
  - `gemini` (`https://generativelanguage.googleapis.com/v1beta/openai/chat/completions`, default model: `gemini-2.5-pro`)
  - `ollama` (`http://localhost:11434/v1/chat/completions`, default model: `qwen2.5-coder:7b`)
- Execution pipeline per turn:
  1. **PreInvocation**: Calls `hooks.Handler.Handle("PreInvocation", ...)` to inject surgical context from `session.db`.
  2. **Inference**: Sends messages + `mcp.Server.ListTools()` to the configured provider endpoint.
  3. **PreToolUse Gate**: Before executing any `tool_call`, validates via `hooks.Handler.Handle("PreToolUse", ...)`. If `decision == "deny"`, returns the denial reason directly to the model without touching disk.
  4. **Execution & PostToolUse**: Executes allowed tools via `mcp.Server.CallTool()` and records state via `hooks.Handler.Handle("PostToolUse", ...)`.
  5. **Session Continuity**: Persists user prompt and final assistant output into `session.db` (`session_context`), enabling instant handoff between `tether run` (DeepSeek API) and `agy` / `claude` / `cursor`.

### Component 3: Universal MCP Installer (`internal/adapter/adapter.go`)
- Add support for `windsurf`, `cline`, `roo`, `opencode`, and `all` in `adapter.Install()`.

### Component 4: CLI Wiring (`cmd/tether/main.go`)
- Add `tether run "<prompt>"` (with flags/env support: `--provider`, `--model`, `DEEPSEEK_API_KEY`, etc.).

---

## 3. Concrete Verification Plan
1. `go test ./...` — All existing tests in `internal/core`, `internal/hooks`, `internal/mcp` plus new tests in `internal/harness/runner_test.go` must pass.
2. Verify via `runner_test.go` mock server:
   - Multi-turn `tool_calls` with `deepseek-reasoner` (`reasoning_content`) executes `read_file` and `todo_add`.
   - Switching engine to `plan` mode causes `Tether Guard` (`PreToolUse`) to block `write_file` / `edit_file` during `tether run` while allowing writes to `docs/`.
