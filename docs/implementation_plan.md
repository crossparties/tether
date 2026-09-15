# Implementation Plan: Tether — Universal Substrate & Proactive Lifecycle Gatekeeper for AI Agents

## Problem Statement
Proyek ini bertransisi menjadi arsitektur orisinal independen: **Tether**, sebuah *deterministic substrate* lokal-pertama (zero-cloud, offline) dan *proactive lifecycle gatekeeper* yang mengikat (*tethers*) AI agent yang bersifat probabilistik dan terisolasi, sehingga developer memiliki kontinuitas konteks, single source of truth, dan jaminan keamanan workflow lintasan agent.

---

## Requirements (Berdasarkan Jawaban User: 1b, 2b, 3a, 4c)
1. **Scope Identitas (1b)**: Penegasan konsep di level dokumentasi (`docs/PRD.md`) dan arsitektur produk sebagai entitas mandiri dan orisinal.
2. **Format Dokumen (2b)**: Menulis ulang `docs/PRD.md` menjadi **Single Unified PRD** berstandar industri yang memuat seluruh narasi produk dan spesifikasi teknis:
   - *What the problem is* (Stateless & uncoordinated agents)
   - *Why it was made* (Ketiadaan shared substrate lokal lintas vendor)
   - *What solutions it provides* (Proactive hook interceptor + SQLite state persistence)
   - *How it makes developer life easier* (Zero-cloud, seamless multi-agent handoff, automatic guardrails)
   - *Why it is a genuine invention* (Proactive in-flight gatekeeping vs reactive memory sinks seperti Mem0/Zep)
3. **Product Architect Subagent (3a)**: Menggunakan subagent berorientasi Product Architect untuk meriset lanskap kompetitif (Mem0, Zep, Letta, native agent hooks), merumuskan diferensiasi, dan memvalidasi keutuhan narasi invensi.
4. **Pilar Inti Invensi (4c)**: Menggabungkan dua pilar utama:
   - **Pilar 1 — Proactive Lifecycle Gatekeeper**: Intersepsi in-flight via hooks (`PreInvocation` surgical context router + `PreToolUse` write gating).
   - **Pilar 2 — Universal Shared Memory Substrate**: Single source of truth lokal (SQLite) yang memungkinkan session handoff antar agent (Claude Code, Agy, Cursor, Codex) tanpa vendor lock-in.

---

## Task Breakdown

### Task 1: Research & Competitive Positioning Synthesis (Product Architect Role)
- **Objective**: Membedah posisi Tether di pasar AI developer tools dan merumuskan argumen invensi yang solid terhadap kompetitor (Mem0, Letta/MemGPT, Zep, cursor rules).
- **Implementation**:
  - Menyusun 5 pilar narasi: Problem, Why, Solution, Ergonomics (How it's easier), dan Invention Defense (The Moat).
  - Merumuskan terminologi orisinal Tether (e.g., *Tether Substrate*, *Tether Hook Interceptor*, *Continuous Project Memory*, *Surgical Context Router*).
- **Done when**: Rumusan narasi selesai dan siap diintegrasikan ke dalam draft PRD terpadu.

### Task 2: Drafting the Unified Tether PRD (`docs/PRD.md`)
- **Objective**: Menulis ulang `docs/PRD.md` secara menyeluruh dengan struktur produk modern dan komprehensif.
- **Implementation**:
  - **Section 1: Executive Summary & The Invention Thesis**
    - Mengapa Tether adalah invensi (Proactive vs Reactive memory).
    - Filosofi "Tether": Menjinakkan agent liar dengan tali deterministik lokal.
  - **Section 2: The Core Problem & Why Tether Was Made**
    - Masalah amnesia agent per sesi.
    - Masalah fragmentasi multi-agent (Claude vs Agy vs Cursor tidak saling kenal).
    - Masalah premature coding (agent menulis kode tanpa plan matang).
  - **Section 3: Solutions & Architecture**
    - Local SQLite Substrate (Session, Decisions, Patterns, FTS5 Knowledge Base, Todos).
    - Lifecycle Interception Engine (`PreInvocation`, `PreToolUse`, `PostToolUse`).
    - Standard MCP Server (stdio newline-delimited protocol).
  - **Section 4: Ergonomics — How It Makes Development Easier**
    - Zero config cloudless setup (`tether init`).
    - Multi-agent session handoff (Pindah terminal tanpa kehilangan konteks).
    - Surgical context delivery (Hemat token hingga 80%).
  - **Section 5: Technical Specification & Contracts**
    - Database schema (`session.db`).
    - Hook contract specification.
    - MCP tools definition.
    - CLI commands spec (`init`, `status`, `doctor`, `replay`, `todo`).
  - **Section 6: Implementation Roadmap (v0.1.0 to v1.0.0)**
    - Phase 1: Solidify Substrate & Fix MCP Protocol Framing.
    - Phase 2: CLI Observability (`status`, `doctor`, `replay`).
    - Phase 3: Surgical Context Routing & Strict Gating.
- **Done when**: File `docs/PRD.md` terupdate penuh dan memiliki narasi invensi yang tajam dan kohesif.

### Task 3: Verification & Cleanliness Review
- **Objective**: Memverifikasi identitas Tether mandiri dan konsisten di seluruh dokumentasi.
- **Implementation**:
  - Verifikasi kelengkapan 5 elemen pertanyaan user (Problem, Why, Solution, Easier, Invention).
- **Done when**: Struktur PRD memenuhi standar arsitektur Tether.

---

## Verification Plan
### Automated Verification
- `head -n 40 docs/PRD.md` -> Memverifikasi judul, meta, dan executive summary Tether.

### Manual Verification
- Review alur narasi: Memastikan penjelasan "Why it is an invention" membedakan Tether secara tegas dari database memori cloud (Mem0/Zep) dan aturan statis (`.cursorrules`/`CLAUDE.md`).
