# PRD: Tether — The Local-First Deterministic Substrate for Autonomous AI Agents

> **Product**: Tether  
> **Version**: 0.2.0 (Architecture Baseline & Target)  
> **Repository**: github.com/croosparties/tether  
> **Status**: Active Specification  

---

## 1. Executive Summary & The Invention Thesis

**Tether** adalah *local-first deterministic substrate* dan *proactive lifecycle gatekeeper* untuk autonomous AI coding agents (Antigravity/Agy, Claude Code, Cursor, Codex, dsb). 

Tether diciptakan untuk memecahkan dilema fundamental dalam rekayasa perangkat lunak berbasis AI: **Large Language Model (LLM) bersifat probabilistik dan stokastik, sedangkan rekayasa perangkat lunak menuntut determinisme absolut.**

```
                  ┌─────────────────────────────────────────────────────────┐
                  │                 OBSERVASI (Realita Lapangan)            │
                  │   Developer modern memakai banyak agent otonom tapi:    │
                  │   • Amnesiac (Stateless per sesi)                       │
                  │   • Fragmented (Silo antar tool & IDE)                  │
                  │   • Reckless (Menulis kode tanpa rem arsitektur)        │
                  └───────────────────────────┬─────────────────────────────┘
                                              │
                                              ▼
                  ┌─────────────────────────────────────────────────────────┐
                  │              KRITIK (Kegagalan Solusi Eksisting)        │
                  │   • Cloud Memory (Mem0/Zep): Pasif, mahal, latency liar │
                  │   • Prompt Rules (.cursorrules): Harapan moral rapuh    │
                  │   • Tool-Cloning: Terjebak duplikasi tanpa diferensiasi │
                  └───────────────────────────┬─────────────────────────────┘
                                              │
                                              ▼
                  ┌─────────────────────────────────────────────────────────┐
                  │              FAKTA (Kenyataan Teknis Dev Ecosystem)    │
                  │   • Zero-Cloud / Local-First adalah keharusan mutlak    │
                  │   • MCP adalah standar de-facto komunikasi agent        │
                  │   • Determinisme OS/Tool boundary mengalahkan prompt    │
                  └───────────────────────────┬─────────────────────────────┘
                                              │
                                              ▼
                        ╔═════════════════════════════════════╗
                        ║           INVENSI: TETHER           ║
                        ║   The Local-First Deterministic     ║
                        ║   Substrate for Autonomous Agents   ║
                        ╚═════════════════════════════════════╝
```

### 1.1 Filosofi Nama: "The Tether" (Tali Penambat)
> *"Sebuah layang-layang raksasa hanya dapat terbang stabil menembus angin kencang jika ditambatkan pada tali jangkar di tanah. Putuskan talinya, dan ia akan terhempas."*

AI Agent modern adalah entitas berkekuatan tinggi di tengah badai komputasi kode. **Tether** bertindak sebagai tali penambat deterministik: ia tidak mematikan daya kreasi agent, melainkan menambatkan energinya ke bumi realitas proyek—memaksakan disiplin perencanaan (*planning gate*), menjaga konsistensi keputusan arsitektur, dan menyediakan memori lokal yang berdaulat.

---

## 2. Problem Statement & Why It Was Made (Analisis Akar Masalah)

### 2.1 Observasi: Tiga Krisis Agen Otonom

1. **Amnesia Struktural (*The Amnesiac Agent*)**:
   LLM secara alamiah adalah stateless compute engine. Setiap kali proses CLI ditutup atau window editor di-refresh, seluruh pemahaman kontekstual musnah: alasan pemilihan library, histori bug yang telah diisolasi, dan pola arsitektur yang disepakati. Developer terpaksa melakukan onboarding berulang-ulang di setiap awal sesi.
2. **Fragmentasi Kepulauan Silo (*The Siloed Agent Paradox*)**:
   Workflow modern menggunakan *best-of-breed agents*: Antigravity (Agy) untuk reasoning arsitektur, Claude Code untuk refactoring terminal cepat, dan Cursor untuk inline autocompletion. Saat ini, setiap agent hidup di pulau terisolasi. Tidak ada *shared subconscious substrate*. Rencana di Agent A hilang saat developer membuka Agent B.
3. **Pelecehan Batasan Arsitektur (*Reckless Coding & Probabilistic Drift*)**:
   Diberikan akses ke tool mutasi (`fs_write`, `execute_bash`), agent probabilistik cenderung langsung membabat kode produksi sebelum menyelesaikan fase analisis atau diverifikasi.

### 2.2 Kritik (K): Mengapa Solusi Saat Ini Gagal

* **Kritik terhadap Cloud Memory Sinks (Mem0, Zep, LangGraph Store)**:
  - *Terlalu Pasif (Reactive)*: Cloud memory hanya bertindak sebagai database pencarian vektor pasif. Ia tidak memahami siklus hidup tool execution dan tidak bisa menghentikan aksi destruktif.
  - *Latensi & Biaya*: Network round-trip (200ms–1000ms) pada setiap prompt merusak responsivitas CLI lokal.
  - *Pelanggaran Kedaulatan Kode*: Mengunggah metadata codebase privat ke server pihak ketiga melanggar kepatuhan enterprise.
* **Kritik terhadap Static Prompt Rules (`.cursorrules`, `CLAUDE.md`, System Prompts)**:
  - *Hanya Harapan Moral (Soft Guidance)*: Prompt teks statis hanya "memohon" agar model patuh. Begitu ukuran konteks membesar (*attention drift*), model melupakan aturan dan kembali berhalusinasi. Teks statis tidak punya mekanisme membatalkan proses OS (`exit code`).

### 2.3 Fakta (F): Fondasi Teknis Tether

1. **Kedaulatan Zero-Cloud & Kecepatan SQLite**: Dev tools tercepat (Git, ripgrep, compiler) berjalan 100% lokal. SQLite embedded di Go murni memberikan latensi kueri <2ms tanpa daemon latar belakang.
2. **Standarisasi Terbuka (Model Context Protocol / MCP)**: MCP stdio JSON-RPC telah menjadi standar terbuka universal lintas vendor (Anthropic, Google, Cursor).
3. **Determinisme Boundary OS Mengalahkan Probabilitas Prompt**: Satu-satunya cara mengendalikan agent probabilistik adalah menghadangnya di level *process/hook boundary*. Jika hook mengembalikan exit code denial, OS secara fisik menolak mutasi disk.

---

## 3. The Invention Defense (Nilai Kebaruan & Moat)

| Dimensi | Solusi Konvensional (Status Quo) | Invensi Definitif: **Tether** |
| :--- | :--- | :--- |
| **Model Operasional** | **Reactive Memory Sink** (Mem0, Zep)<br>Pasif menunggu kueri RAG, tidak sadar tool execution. | **Proactive In-Flight Interceptor**<br>Menyisipkan diri di jalur eksekusi model; memotong, menyaring, dan mengarahkan konteks sebelum token pertama dibuat. |
| **Penegakan Aturan** | **Soft Prompt Guidance** (`.cursorrules`)<br>Teks imbauan moral; rentan dilanggar saat token window penuh. | **Hard Runtime Enforcement**<br>Intersepsi proses level OS/MCP. Write tool ditolak keras (exit code 2) jika berada dalam mode `PLAN`. |
| **Diferensiasi Ekosistem**| **Siloed Assistant**<br>Membuat agent tertutup baru yang mengunci pengguna ke satu vendor. | **Universal Cognitive Substrate**<br>Layer substrate netral; menyatukan memori dan guardrail untuk seluruh agent (Agy, Claude, Cursor, Codex). |

---

## 4. What Solutions It Provides (Pilar Solusi Tether)

```
┌──────────────────────────────────────────────────────────────────────────┐
│                   AUTONOMOUS AI CODING AGENTS                            │
│           (Antigravity / Claude Code / Cursor / Codex)                   │
└─────────────────────┬──────────────────────────────▲─────────────────────┘
                      │ Tool Execution / Hooks       │ Surgical Context
                      ▼                              │ Injection
┌──────────────────────────────────────────────────────────────────────────┐
│                           TETHER CORE ENGINE                             │
│                                                                          │
│  ┌────────────────────────┐  ┌────────────────────────────────────────┐  │
│  │  PROACTIVE INTERCEPTOR │  │           MCP PROTOCOL SERVER          │  │
│  │  • PreInvocation Hook  │  │  • Stdio JSON-RPC 2.0                  │  │
│  │  • PreToolUse Gating   │  │  • 17+ Unified Tools                   │  │
│  │  • PostToolUse Auditor │  │  • Context, Todos, KB, Session         │  │
│  └───────────┬────────────┘  └───────────────────┬────────────────────┘  │
│              │                                   │                       │
│  ┌───────────▼───────────────────────────────────▼────────────────────┐  │
│  │                 DETERMINISTIC LOCAL SUBSTRATE                      │  │
│  │  • Session State Machine (Code vs Plan Mode)                       │  │
│  │  • Architectural Decision Log (Rationale Persistence)              │  │
│  │  • Learned Patterns Engine (Do & Avoid)                            │  │
│  │  • Fast FTS5 Knowledge Base (BM25 Token Indexer)                   │  │
│  │  • Cross-Agent Task & Todo Synchronizer                            │  │
│  └───────────────────────────────────┬────────────────────────────────┘  │
└──────────────────────────────────────┼───────────────────────────────────┘
                                       ▼
                       ┌───────────────────────────────┐
                       │     SQLite: session.db        │
                       │     (Zero-Cloud, Embedded)    │
                       └───────────────────────────────┘
```

### 4.1 Local-First Deterministic Substrate
- Single binary Go tanpa runtime eksternal.
- Database lokal embedded SQLite (`session.db` di root project).
- Menyimpan state aktif: session context, keputusan arsitektur, pola coding terverifikasi, dan struktur todo.

### 4.2 Proactive In-Flight Lifecycle Interceptor
- **PreInvocation Hook (Surgical Context Router)**: Menyuntikkan irisan memori yang relevan secara selektif sebelum model berpikir (menghemat token hingga 80% dibanding context dumping).
- **PreToolUse Hook (Deterministic Gatekeeper)**: Memblokir tool mutasi disk (`write_to_file`, `replace_file_content`, command bash berisiko) saat mode proyek berada dalam fase `plan`.
- **PostToolUse Hook (Auditor)**: Merekam side-effect dan memperbarui state session secara otomatis setelah tool selesai dijalankan.

### 4.3 Universal Stdio MCP Server
- Mendukung spesifikasi standar MCP (Model Context Protocol) via stdio JSON-RPC 2.0 (newline-delimited).
- Menyediakan toolset bagi agent untuk memanggil memori, menyimpan keputusan, memanipulasi todo, dan mencari knowledge base lokal.

### 4.4 Cross-Agent Session Continuity & Handoff
- Menghubungkan berbagai agent ke dalam satu *shared state*.
- Developer dapat merancang arsitektur menggunakan Antigravity, lalu beralih ke terminal Claude Code untuk eksekusi tanpa kehilangan konteks, todo, atau aturan proyek.

---

## 5. How It Makes Developer Life Easier (Ergonomi & Workflow)

1. **Zero-Friction Initialization**:
   - `tether init` secara otomatis mendeteksi stack proyek, membuat `session.db`, dan memperbarui `.gitignore`.
2. **Seamless Multi-Agent Handoff**:
   - Memori arsitektur dan todo tersimpan di disk lokal. Developer bebas berganti agent sesuai keunggulan masing-masing tanpa proses re-onboarding manual.
3. **Observabilitas Penuh (Anti-Black-Box)**:
   - Perintah `tether status` menunjukkan kesehatan sesi, mode aktif, dan riwayat trigger hook.
   - Perintah `tether replay` menampilkan persis konteks apa yang disuntikkan ke agent pada prompt terakhir.
   - Perintah `tether doctor` memverifikasi integritas konfigurasi agent dan ketersediaan binary.

---

## 6. Technical Specifications & Contracts

### 6.1 Database Schema (`session.db`)

```sql
CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    status TEXT DEFAULT 'active',      -- active | completed | archived
    mode TEXT DEFAULT 'code',          -- code | plan
    summary TEXT DEFAULT '',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS session_context (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT REFERENCES sessions(id),
    agent TEXT DEFAULT '',
    role TEXT,                         -- user | assistant | plan | decision | project_index
    content TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS project_settings (
    key TEXT PRIMARY KEY,
    value TEXT,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS decisions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT REFERENCES sessions(id),
    topic TEXT,                        -- e.g. "database", "auth", "architecture"
    decision TEXT,
    reason TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS learned_patterns (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    pattern_type TEXT,                 -- "do" | "avoid"
    description TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS todos (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT REFERENCES sessions(id),
    title TEXT NOT NULL,
    description TEXT,
    status TEXT DEFAULT 'pending',     -- pending | in_progress | completed | cancelled
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    completed_at DATETIME
);
```

### 6.2 Hook Execution Contract

Setiap agent berkomunikasi dengan Tether melalui CLI command `tether hook <EventType>`.

* **Input**: Payload JSON melalui `stdin`
* **Output**: Payload JSON melalui `stdout`
* **Exit Codes**:
  - `0`: Success / Allow
  - `2`: Hard Deny (Operasi dibatalkan secara fisik oleh runtime; pesan penolakan dikirim melalui stderr/json)

```json
// Output Contract: PreInvocation
{
  "injectSteps": [
    {
      "ephemeralMessage": "## Tether Context\n- Mode: plan (writes BLOCKED)\n- Active Todo: Implement auth schema\n- Relevant Pattern: [do] Use stdlib crypto"
    }
  ]
}

// Output Contract: PreToolUse (Ketika mode = plan dan tool = write_to_file)
{
  "decision": "deny",
  "reason": "Tether Gatekeeper: Project is in PLAN mode. Write operations are blocked until plan is approved via 'tether mode code'."
}
```

### 6.3 MCP Server Protocol Contract
* **Transport**: Stdio
* **Framing**: Newline-delimited JSON-RPC 2.0 (`\n`)
* **Tools Tersedia**:
  - `tether_get_session`: Membaca metadata sesi aktif.
  - `tether_set_mode`: Mengubah mode (`plan` atau `code`).
  - `tether_save_decision`: Mencatat keputusan arsitektur.
  - `tether_add_pattern`: Menyimpan pola yang dipelajari (`do` / `avoid`).
  - `tether_todo_list`: Mengambil daftar task aktif.
  - `tether_todo_update`: Mengubah status task.
  - `tether_kb_search`: Melakukan pencarian full-text (FTS5 BM25) di codebase.

---

## 7. CLI Interface Specification

```text
tether — The Local-First Deterministic Substrate for AI Agents

Usage:
  tether init              Inisialisasi Tether di workspace saat ini
  tether mcp               Menjalankan server MCP (stdio JSON-RPC)
  tether hook <event>      Menangani lifecycle hook (PreInvocation, PreToolUse, PostToolUse)
  tether status            Menampilkan status sesi, mode aktif, todo count, dan statistik hook
  tether doctor            Memverifikasi integritas hooks.json, mcp_config.json, dan binary path
  tether replay            Menampilkan ephemeral injection terakhir yang diterima agent
  tether todo [cmd]        CLI todo manager (list, add, done)
  tether install [agent]   Memasang Tether ke config agent (agy | claude | codex | cursor)
  tether version           Menampilkan versi Tether
```

---

## 8. Implementation Roadmap

### Phase 1: Substrate Solidification & Protocol Hygiene (Immediate)
- Migrasi repository dan modul ke `github.com/croosparties/tether`.
- Standarisasi MCP stdio transport menggunakan Newline-Delimited JSON (memperbaiki hang pada handshake).
- Persistensi state mode (`code` / `plan`) di dalam database SQLite (menghilangkan in-memory struct state).

### Phase 2: Observability & Developer Trust
- Implementasi perintah `tether status` untuk transparansi instan status hook dan sesi.
- Implementasi perintah `tether doctor` untuk diagnosis konfigurasi otomatis.
- Implementasi perintah `tether replay` untuk inspeksi payload injection.

### Phase 3: Surgical Context Engine
- Refactor `PreInvocation` dari context dumping menyeluruh menjadi *surgical router* (hanya menyuntikkan 3 keputusan terbaru, pola relevan dengan ekstensi file, dan todo aktif).
- Perketat deteksi `ClassifyTask` agar deterministik berdasarkan prefix eksplisit (`/plan`, `/spec`).

### Phase 4: Universal Multi-Agent Adapters
- Adapter resmi untuk Antigravity CLI, Claude Code, Cursor, dan Codex CLI.
- Validasi end-to-end multi-agent session handoff secara lokal.
