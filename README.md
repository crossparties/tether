# ⚓ Tether

**The Local-First Memory & Safety Substrate for AI Coding Agents**

Tether anchors AI coding agents (Antigravity CLI, Claude Code, Cursor) to your local repository. It provides persistent project memory and enforceable plan-vs-code guardrails—with **zero cloud dependency** and **zero data leaving your machine**.

---

## 💡 Why Tether?

| Without Tether | With Tether |
| :--- | :--- |
| **Amnesiac Sessions**: Context, architectural decisions, and bug history vanish whenever a session closes. | **Continuous Project Memory**: Tether remembers your stack, decisions, and tasks locally across restarts and agents. |
| **Premature Code Edits**: Agents mutate production code before architectural specs are agreed upon. | **Plan vs Code Guardrails**: Lock the repository into `plan` mode. Tether safely blocks code mutations until you approve. |
| **Cloud Memory Risks**: Third-party memory services send your proprietary codebase metadata to external servers. | **100% Local Sovereignty**: All state lives inside your project's local `.gitignore`'d SQLite database. |
| **Permission Prompt Spam**: Agents trigger endless confirmation dialogs when trying to query project state. | **Frictionless Integration**: Pre-approved MCP and lifecycle hooks allow instant, seamless agent execution. |

---

## ⚡ Quick Start

### 1. Install Tether

```bash
go install github.com/crossparties/tether/cmd/tether@latest
```

*(Or download the prebuilt binary from [Releases](https://github.com/crossparties/tether/releases))*

### 2. Initialize in Your Project

Run inside any Git repository:

```bash
tether init
```

Tether detects your project's tech stack, initializes local storage, and updates `.gitignore`.

### 3. Connect to Your AI Agent

Wire Tether into your preferred coding agent:

```bash
# For Google Antigravity CLI (agy)
tether install agy

# For Anthropic Claude Code
tether install claude
```

Verify your setup at any time:

```bash
tether doctor
```

---

## 🚀 How It Works

Once wired, Tether runs silently in the background:

### 1. Automatic Memory Injection
When you start a new conversation turn or ask *"Where were we?"*, Tether automatically provides your agent with active tasks, recent architectural decisions, and stack conventions—without wasting prompt tokens.

### 2. Plan Mode vs. Code Mode
Keep your agents disciplined:

```bash
# Switch to Plan Mode: Agents can write specs in docs/, but cannot mutate source code
tether mode plan

# Switch to Code Mode: Full implementation unlocked
tether mode code
```

Agents can also switch modes programmatically once an implementation plan is approved.

### 3. Check Real-Time Project Status

```bash
tether status
```

Displays your active session ID, governance mode, tech stack, and pending task list.

---

## 🛠️ CLI Commands

| Command | Description |
| :--- | :--- |
| `tether init` | Initialize Tether in the current repository |
| `tether status` | Display real-time substrate dashboard & active tasks |
| `tether mode [plan\|code]` | Inspect or switch active governance mode |
| `tether doctor` | Run environment, database, and agent wiring diagnostics |
| `tether install [agent]` | Automatically wire Tether into agent configuration (`agy`, `claude`) |
| `tether version` | Display current Tether version |

---

## 🧩 Compatibility

- **Google Antigravity CLI (`agy`)**: Full support (MCP server + lifecycle hooks)
- **Anthropic Claude Code**: Full support (MCP server + hooks)
- **Cursor / VS Code**: MCP server integration
- **Platform**: Linux, macOS, Windows

---

## 📚 Deep Dive

Interested in how Tether works under the hood (WAL storage engine, lifecycle hooks, process isolation)?  
Check out the [Architecture Specification](docs/architecture.md).

---

## 📄 License

MIT License © [crossparties](https://github.com/crossparties)
