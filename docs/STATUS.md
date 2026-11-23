# STATUS: Tether — Peta Kondisi Nyata Implementasi (v0.2.0)

> **Dokumen**: Peta kondisi nyata implementasi  
> **Sumber**: Verifikasi langsung kode di `github.com/crossparties/tether`  
> **Diperbarui**: 2026-11-22  
> **Prinsip**: Setiap klaim di sini dapat ditelusuri langsung ke file dan simbol kode aktual.

---

## 1. Ringkasan Eksekutif

Tether (`v0.2.0`) kini telah berevolusi menjadi sistem dua lapis yang utuh:
1. **Substrat Memori & Guardrail Deterministik Lokal** (`internal/core`, `internal/mcp`, `internal/hooks`, `internal/safety`, `internal/snapshot`): Duduk di antara agen eksternal (Antigravity `agy`, Claude Code, Cursor, Windsurf, Cline, Roo, Codex, OpenCode) dan repositori lokal, menyediakan memori proyek persisten di SQLite WAL (`session.db`), **31 tool MCP**, *single choke-point approval gate*, *proposal gate* subagent, serta intersepsi *lifecycle hook* dengan penolakan fisik `exit code 2`.
2. **Native Interactive Agent Harness & TUI** (`internal/harness`, `internal/tui`, `internal/orchestrator`, `internal/ui`): Menyediakan antarmuka Terminal UI interaktif penuh (tanpa ketergantungan framework TUI eksternal), registri slash-command terpusat, dropdown autocomplete `/` dan `@file`, modal persetujuan diff interaktif, *ephemeral side-chat* (`/btw`), serta orkestrasi worker subagent.

---

## 2. Metrik Kode Aktual

| Metrik | Nilai Aktual | Sumber Verifikasi |
|---|---|---|
| Total File Go | **86 file Go** (termasuk 37 file `*_test.go`) | `tree-sitter` scan pada `/home/lenovo/crossparties/tether` |
| Paket Internal | **12 paket** di bawah `internal/` + `cmd/tether` | `cmd/tether`, `core`, `mcp`, `hooks`, `safety`, `snapshot`, `preview`, `harness`, `orchestrator`, `tui`, `ui`, `adapter` |
| Jumlah Tool MCP | **31 tool MCP** | `Server.ListTools` & `Server.CallTool` di [`internal/mcp/server.go`](file:///home/lenovo/crossparties/tether/internal/mcp/server.go) |
| Skema SQLite WAL | **SchemaVersion = 2** (9 tabel + 1 FTS5 virtual table `kb_fts`) | [`internal/core/engine.go`](file:///home/lenovo/crossparties/tether/internal/core/engine.go) |
| Ketergantungan CGO | **0 (Pure Go)** via `modernc.org/sqlite` | [`go.mod`](file:///home/lenovo/crossparties/tether/go.mod) |

---

## 3. Sebaran Modul & Tingkat Kematangan

| Paket | File Kunci | Peran Utama | Status Kematangan |
|---|---|---|---|
| `internal/mcp` | `server.go`, `approval.go` | 31 tool MCP + single choke-point approval gate + fuzzy edit + auto `go vet` | **Matang** |
| `internal/core` | `engine.go`, `grants.go`, `handoff.go`, `workspace.go` | Engine SQLite WAL, scoped grants (TTL 60m), proposal lifecycle, handoff parser, FTS5 BM25 | **Matang** |
| `internal/hooks` | `handler.go`, `dangerous.go`, `subagent_access.go` | `PreInvocation` (DeepSeek prefix-cache optimized), `PreToolUse` (5-layer gate), `PostToolUse` | **Matang** |
| `internal/safety` | `policy.go`, `shell.go` | Risk classifier (`RiskLow`/`RiskHigh`), `IsNonCriticalTool`, symlink-safe path resolution | **Matang** |
| `internal/snapshot` | `snapshot.go` | Pre-write snapshot `.tether/snapshots/<turn>/` & `tether undo` tanpa Git | **Matang** |
| `internal/preview` | `preview.go` | Unified diff generator untuk pratinjau perubahan | **Matang** |
| `internal/harness` | `runner.go`, `commands.go`, `subagent.go`, `btw.go`, `stream.go` | Multi-provider LLM loop, slash-command registry, `/btw` side-chat, subagent isolation | **Matang** |
| `internal/tui` | `model.go`, `update.go`, `view.go`, `program.go`, `views/*` | Full-screen custom Termios TUI, approval modal, `/` & `@file` autocomplete, inspector | **Aktif & Fungsional** |
| `internal/orchestrator` | `engine.go`, `worker.go`, `registry.go` | Concurrent worker orchestration & event bus untuk TUI | **Aktif & Fungsional** |
| `internal/ui` | `ui.go`, `picker.go`, `spinner.go`, `termios_*.go` | ANSI theme, Unicode/CJK width calculator, raw mode termios, interactive picker | **Matang** |
| `internal/adapter` | `adapter.go` | Auto-wiring untuk `agy`, `claude`, `codex`, `cursor`, `windsurf`, `cline`, `roo`, `opencode`, `all` | **Cukup** |
| `cmd/tether` | `main.go` | CLI dispatcher (`init`, `mcp`, `hook`, `mode`, `undo`, `status`, `doctor`, `install`, `proposal`, `tui`, `run/chat`) | **Matang** |

---

## 4. Fitur yang Sudah Terimplementasi & Terverifikasi

### 4.1 Substrat Memori & Optimasi Prefix Cache (`internal/core` & `internal/hooks`)
- **SQLite WAL (`session.db`)**: 9 tabel (`sessions`, `session_context`, `project_settings`, `decisions`, `learned_patterns`, `todos`, `kb_documents`, `subagent_proposals`, `approval_grants`) + `kb_fts` (FTS5 BM25).
- **Injeksi Memori Bedah (*Surgical Injection*)**: Dibatasi `LIMIT 5` untuk keputusan dan pola, dipotong maks 300 rune per entri, dengan *hard cap* 16 KB (`Anti-Pipe Deadlock Guard`).
- **Optimasi Prefix Cache LLM (DeepSeek 64-Token Block)**: Header statis dan `project_settings` yang diurutkan abjad ditempatkan di awal prompt (mengabaikan key volatil `current_turn`), sedangkan konteks dinamis ditempatkan di akhir prompt.

### 4.2 Single Choke-Point Approval Gate & Scoped Grants (`internal/mcp` & `internal/core/grants.go`)
- Dipasang di satu titik tunggal: `Server.CallTool` $\rightarrow$ `requireApproval`.
- **Path Grants vs. Command Grants**: Izin tulis direktori tidak pernah memberi izin eksekusi perintah shell.
- **Smart Command Prefix Expansion**: Binary bersubcommand (`git`, `go`, `npm`, `docker`, dll.) menggunakan prefix 2 kata (`go test`), sedangkan binary argumen posisional (`echo`, `cat`) berhenti di 1 kata.
- **Defense-in-Depth (`safety.IsNonCriticalTool`)**: Operasi baca, perintah validasi (`go test`, `go vet`), penulisan rencana di `docs/` saat mode `plan`, dan pengeditan file workspace rutin saat mode `code` (yang sudah dilindungi oleh `.tether/snapshots`) tidak memicu interupsi prompt yang tidak perlu.

### 4.3 Tata Kelola Subagent & Proposal Gate (`internal/hooks/subagent_access.go` & `internal/harness/subagent.go`)
- **Deny-by-Default Allowlist**: Membuka bungkus `call_mcp_tool` terlebih dahulu (`subagentUnwrapMCP`), hanya mengizinkan `subagentReadTools`, `sql_query` read-only, `git diff/status/log/show`, serta command validasi (`go test`, `go build`, `go vet`). Mutasi file dan `git add/commit/reset/checkout/stash` diblokir mutlak.
- **Enforceable Proposal Gate**: Selama ada proposal subagent berstatus `pending`, Main Agent diblokir dari menulis file atau mengubah state sampai proposal diputuskan lewat `approve_proposal` / `reject_proposal` atau melalui CLI `tether proposal approve|reject`.
- **Structured Handoff Parser (`core.ParseHandoff`)**: Memvalidasi blok `STATUS` dan `VERDICT` dari output subagent (dengan *single re-dispatch* jika format berupa prosa bebas).

### 4.4 Registri Slash Command, `/btw` Side-Chat, & Full TUI (`internal/harness` & `internal/tui`)
- **Registri Terpusat (`internal/harness/commands.go`)**: Seluruh slash command (`/plan`, `/code`, `/model`, `/provider`, `/key`, `/status`, `/clear`, `/undo`, `/btw`, `/subagent`, `/help`, `/exit`, `/quit`) terdaftar dalam satu tabel yang dijaga oleh `registry_tripwire_test.go`.
- **Isolasi `/btw` (`internal/harness/btw.go`)**: Pertanyaan sampingan dieksekusi tanpa mengubah histori pesan utama (`r.messages`) dan tanpa akses ke tool mutasi.
- **Full Terminal UI (`internal/tui`)**: Mendukung tampilan stream real-time, modal approval dengan diff berwarna (`Once`, `Folder/Prefix`, `Session`, `Reject`), dropdown autocomplete `/` dan `@file`, serta panel inspeksi subagent.

---

## 5. Catatan Teknis & Hal yang Perlu Diperhatikan ke Depan

1. **Konsistensi Nama Modul Go**:
   - Nama organisasi yang benar adalah **`crossparties`** (`github.com/crossparties/tether`).
   - Saat ini file [`go.mod`](file:///home/lenovo/crossparties/tether/go.mod#L1) dan pernyataan `import` internal masih menggunakan string `github.com/croosparties/tether`. Jika ingin melakukan migrasi nama modul di kode Go pada fase berikutnya, lakukan penggantian atomik di `go.mod` dan seluruh file `.go` secara serentak.
2. **Subcommand CLI Tambahan di PRD**:
   - Dokumen PRD awal sempat menyebut subcommand `tether replay` dan `tether todo`. Saat ini manajemen todo dilakukan melalui tool MCP (`todo_list`, `todo_add`, `todo_complete`, `todo_remove`) dan ringkasan ditampilkan melalui `tether status`.
3. **Dukungan Endpoint LLM Native Non-OpenAI**:
   - Seluruh provider di `internal/harness` saat ini berkomunikasi melalui kontrak `/chat/completions` yang kompatibel dengan OpenAI (`deepseek`, `openrouter`, `openai`, `groq`, `gemini`, `xai`, `ollama`). Dukungan native Anthropic Messages API (`/v1/messages`) dapat menggunakan OpenRouter atau ditambahkan sebagai adapter transport di `runner.go`.
