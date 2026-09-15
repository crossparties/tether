---
name: tether-product
description: Product Architecture and Strategy Skill for Tether. Encapsulates the 5 foundational pillars (Problem, Why, Solution, Ergonomics, Invention Defense), the OKF (Open Knowledge Fact-finding) methodology, and Ponytail minimalist engineering principles for Tether.
---

# Tether Product Architecture Skill

Use this skill when designing features, writing specifications, evaluating agent workflows, or answering strategic product questions about **Tether** (`github.com/croosparties/tether`).

## The Core Concept
**Tether** is the *Local-First Deterministic Substrate & Proactive Lifecycle Gatekeeper for Autonomous AI Coding Agents*.

> *"A powerful kite in a storm can only soar stably if anchored to the ground. Cut the string, and it crashes."*
> Tether is the deterministic anchor for probabilistic, stochastic AI agents.

---

## The 5 Foundational Pillars

### 1. What The Problem Is
- **The Amnesiac Agent**: LLMs are stateless compute engines. Session ends erase architectural decisions, bug history, and conventions, forcing repetitive onboarding or massive prompt dumps.
- **The Siloed Agent Paradox**: Modern developers use multiple best-of-breed agents (Antigravity for planning, Claude Code for CLI refactoring, Cursor for inline editing), but they live on isolated islands with zero shared memory.
- **Reckless Coding**: Without hard guardrails, probabilistic agents mutate production code prematurely before design validation.

### 2. Why It Was Made
- **Zero-Cloud Code Sovereignty**: Reject sending proprietary codebases and metadata to third-party cloud memory sinks (Mem0, Zep). Keep all data 100% local in SQLite.
- **Hard Runtime Enforcement vs. Soft Prompt Guidance**: Static prompt rules (`.cursorrules`, `CLAUDE.md`) are mere moral pleas that degrade as context expands. Tether intercepts at the OS/tool boundary using `exit 2` to physically block unauthorized writes during plan mode.
- **Independent Substrate**: Standalone infrastructure layer, not a locked-in single-vendor assistant.

### 3. What The Solutions Provide
- **Local-First Deterministic Substrate**: Single binary Go with embedded SQLite (`session.db`), sub-2ms query latency, zero external servers.
- **Proactive In-Flight Lifecycle Interceptor**:
  - `PreInvocation`: Surgical context injection (saving up to 80% token window).
  - `PreToolUse`: Write gating and command safety enforcement (Hard denial exit code 2).
  - `PostToolUse`: Automatic state recording and audit logging.
- **Universal Stdio MCP Protocol Server**: Standard newline-delimited JSON-RPC 2.0 with 17+ tools.
- **Cross-Agent Session Continuity**: Shared state machine and todo graph accessible by any agent.

### 4. How It Makes Developer Life Easier
- **Zero-Friction Initialization**: `tether init` configures database, stack detection, and `.gitignore` in <1 second.
- **Fluid Multi-Agent Handoff**: Plan in Antigravity, refactor in Claude Code, inspect in Cursor—zero repeated explanations.
- **Surgical Token Economy**: Eliminates token pollution by injecting only active decisions, relevant patterns, and current tasks.
- **Developer Observability**: `tether status`, `tether doctor`, and `tether replay` provide absolute transparency.

### 5. Why It Is A Genuine Invention
- **Proactive vs. Reactive**: In-flight interception before token generation vs. passive database queries.
- **Hard OS Enforcement vs. Soft Prompts**: Exit-code level process control vs. prompt hopes.
- **Universal Neutral Substrate vs. Siloed Tools**: Cross-agent cognitive substrate vs. vendor-locked CLI clones.

---

## Methodological Directives

1. **OKF (Open Knowledge Fact-finding)**:
   - Always validate assumptions against open industry standards (Claude Code lifecycle hooks, Model Context Protocol specifications, SQLite capabilities).
   - Use web search and official documentation to verify mechanics before proposing architecture.

2. **Ponytail Minimalist Discipline**:
   - The best code is code never written.
   - Standard library and native OS capabilities before external dependencies.
   - Keep diffs surgical; do not build speculative abstractions (YAGNI).
   - Require a concrete verification step for every change.
