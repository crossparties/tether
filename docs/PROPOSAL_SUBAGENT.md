# Proposal: Subagent UI + Access Control (port dari cc-sdd v3.0.2)

Status: **menunggu approval user**. Belum ada kode yang diubah.

Setiap perubahan di bawah dikutip langsung dari sumber cc-sdd v3.0.2.
Kutipan ditandai `▸`. Bila sumber tidak mengatur sesuatu, itu disebut eksplisit.

---

## Bagian 1 — Hak akses: mengikuti pembagian kapabilitas cc-sdd

cc-sdd tidak memakai tabel kapabilitas. Ia memakai **pembagian peran yang tegas**:
parent memegang commit dan scope, worker memegang eksekusi dan verifikasi.

### 1.1 Parent memegang command set, subagent hanya menerima subset

▸ `kiro-impl/SKILL.md` Preflight:
> "Derive a canonical validation set for this repo: `TEST_COMMANDS`, `BUILD_COMMANDS`, and `SMOKE_COMMANDS`"
> "**Keep the full command set in the parent context, and pass only the task-relevant subset to implementer and reviewer subagents**"

**Konsekuensi untuk Tether:** subagent **boleh** menjalankan command. Ini menutup pertanyaan terbuka saya sebelumnya — jawabannya ya, karena sumbernya mewajibkan. Yang membatasi bukan larangan total, melainkan **subset yang diberikan parent**.

**Implementasi:** `spawn_subagent` menerima field opsional `commands []string`. Kalau kosong, subagent memakai `DefaultSubagentCommands` (lihat 1.4). `run_command` untuk subagent hanya lolos bila command-nya **prefix-match** dengan salah satu entri di subset tersebut — memakai `scopeCoversPath`-style matching yang sudah ada, bukan pencocokan string liar.

### 1.2 Worker wajib menjalankan test — bukan larangan

▸ `implementer-prompt.md`:
> "**Run tests and capture the failing output.** You will include this in the status report as evidence"
> "Run the parent-provided validation commands needed to establish confidence for this task"
> "Prefer the parent-discovered canonical commands over inventing new ones; only add a task-local verification command when the parent set does not cover the task, and explain why"

▸ `reviewer-prompt.md`:
> "**Taste encoded as tooling.** Where a check can be verified mechanically (grep, test execution, linter), run the command and use the result. Do not rely on visual inspection alone"
> "Run the project's test suite (e.g., `npm test`, `pytest`). Use the exit code."
> "Run `git diff` to see the actual code changes. This is your primary input."

**Konsekuensi:** `tether_qa` **dan** reviewer **wajib** bisa `go test` dan `git diff`. Melarang shell untuk subagent akan melanggar sumber.

**Pembatasan yang benar:** command yang membuktikan keadaan, bukan command yang mengubahnya. cc-sdd melarang `git add`/`git commit` untuk worker, bukan `git diff`/`git status`.

### 1.3 Commit parent-only — ini larangan mutlak di sumber

▸ `kiro-impl/SKILL.md`:
> "**e) Commit** (parent-only, selective staging)"
> "**NEVER** use `git add -A` or `git add .`"
> "Use `git add <file1> <file2> ...` with explicit file paths"

▸ `implementer-prompt.md`:
> "Do NOT create commits"

**Konsekuensi:** `git add`, `git commit`, `git reset`, `git checkout --`, `git stash` → **deny mutlak** untuk semua subagent, tanpa pengecualian per-role. Ini yang paling tajam: saat ini Tether tidak melarangnya sama sekali.

### 1.4 Command set default (diturunkan dari repo Tether, seperti Preflight cc-sdd)

▸ Preflight: "Inspect repository-local sources of truth in this order: project scripts/manifests (`package.json`, `pyproject.toml`, **`go.mod`**, ...), task runners (`Makefile`, `justfile`), CI/workflow files ... then `README*`"

Untuk Tether (`go.mod` + `project_settings` `build_cmd`/`test_cmd`), yang dihasilkan sumbernya:

```go
// Diturunkan dari tether project settings: build_cmd=go build ./..., test_cmd=go test ./...
// Urutannya mengikuti Preflight cc-sdd: manifest (go.mod) -> settings proyek.
var DefaultSubagentCommands = []string{
	"go test",    // TEST_COMMANDS
	"go build",   // BUILD_COMMANDS
	"go vet",     // pre-existing: bagian dari pipeline Go
}
```

`git` **tidak** ada di daftar ini — lihat 1.3.

### 1.5 Deny-by-default

▸ `reviewer-prompt.md` menuntut reviewer memverifikasi sendiri; `implementer-prompt.md` menuntut boundary dipatuhi. Keduanya mengandaikan **allowlist**, bukan "semua boleh kecuali daftar hitam".

**Implementasi:** fallback di `internal/hooks/handler.go:452` yang saat ini:

```go
return HookOutput{Decision: "allow", Reason: "Subagent read/query action allowed"}
```

diganti menjadi **deny** kecuali tool ada di salah satu daftar:

| Kategori | Isi | Peran |
|---|---|---|
| `readTools` | `read_file`, `glob`, `grep`, `list_dir`, `kb_search`, `sql_query`, `session_load`, `todo_list`, `proposal_status` | semua |
| `evidenceCommands` | `git diff`, `git status`, `git log`, `git show` | semua (dibutuhkan reviewer) |
| `validationCommands` | subset dari 1.4 yang diberikan parent | semua |
| write tools | — | **tidak ada** (sudah ditegakkan) |
| `git add/commit/reset/checkout/stash` | — | **tidak ada** |

### 1.6 Yang TIDAK diatur sumber

Disebut jujur: cc-sdd **tidak** mengatur timeout subagent, batas kedalaman rekursi, atau validasi nama peran (perannya adalah template markdown tetap, bukan string runtime). Tiga hal ini tetap tambalan Tether sendiri — dijaga tetap kecil dan dilaporkan terpisah di Bagian 3.

---

## Bagian 2 — Status report: parse, jangan percaya prosa

Ini port langsung dan **paling bernilai**, karena `internal/core/handoff.go` sudah bisa mem-parse tetapi belum pernah dipanggil.

▸ `kiro-impl/SKILL.md`:
> "If `STATUS` is missing, ambiguous, or replaced with prose, re-dispatch the implementer once ... **Do NOT proceed to review without a parseable value**"
> "If `VERDICT` is missing, ambiguous, or replaced with prose, re-dispatch the reviewer once requesting the exact structured verdict only. **Do NOT mark the task complete, commit, or continue to the next task** without a parseable `APPROVED | REJECTED` value."

▸ `implementer-prompt.md`:
> "- STATUS: <READY_FOR_REVIEW | BLOCKED | NEEDS_CONTEXT>"

▸ `reviewer-prompt.md`:
> "If REJECTED, REMEDIATION is mandatory — identify the exact file, the exact problem... Vague feedback like 'improve tests' is not acceptable."

**Implementasi di `runSubagent`:**

1. `core.ParseHandoff(ans)` → bila `err != nil` atau `Status == ""`, **re-dispatch sekali** dengan instruksi "return the structured STATUS block only".
2. Masih gagal → kembalikan ke model sebagai *incomplete*, bukan sukses.
3. Reviewer `REJECTED` wajib punya `REMEDIATION`; tanpa itu → dianggap `NEEDS_CONTEXT`.
4. Ronde debug dibatasi `core.MaxDebugRounds` (sudah ada, =2).

---

## Bagian 3 — UI subagent (tambalan Tether, minimal)

Sumber cc-sdd tidak mengatur UI karena host-nya yang menampilkan. Jadi bagian ini murni perbaikan Tether, dijaga sekecil mungkin.

**Masalah terukur:** `subCfg := r.cfg` mewarisi `Out`, sehingga seluruh tool call subagent mencetak ke terminal utama. `ui.RunWithSpinner` ada di `internal/ui/spinner.go` dengan **nol call site**.

**Perubahan:**

1. `subCfg.Out` → `&bytes.Buffer`. Detail tertahan.
2. Satu blok ringkasan setelah selesai:
   ```
   ━━ 🤖 tether_qa · 8 steps · 12.4s ━━
      ✓ READY_FOR_REVIEW · proposal #12
   ```
3. `ui.RunWithSpinner` membungkus eksekusi.
4. Validasi `role` terhadap daftar tertutup (tambalan, lihat 1.6).
5. `context.WithTimeout(SubagentTimeout)` + `MaxSubagentDepth` (tambalan, lihat 1.6).

---

## File yang disentuh

| File | Perubahan |
|---|---|
| `internal/harness/subagent.go` | **baru** — role tertutup, limit, command set, ringkasan |
| `internal/harness/runner.go` | `runSubagent` diubah: buffer, spinner, timeout, depth, parse status |
| `internal/hooks/handler.go` | fallback `allow` → allowlist; `git add/commit/...` deny mutlak |
| `internal/harness/subagent_test.go` | **baru** — role, limit, status fail-closed, kebocoran output |
| `internal/hooks/subagent_access_test.go` | **baru** — `git add` deny, `go test` allow, tool tak dikenal deny |

## Urutan eksekusi

1. `subagent.go` + test-nya (murni, tanpa dependensi lain)
2. `handler.go` allowlist + test (paling berisiko, diuji sendiri)
3. `runner.go` sambungkan keduanya
4. `go test ./internal/harness/ ./internal/hooks/` lalu `go test ./...`
5. `go build -o ~/go/bin/tether ./cmd/tether`

## Pertanyaan terbuka — sudah terjawab sumber

| Sebelumnya | Jawaban sumber |
|---|---|
| Boleh `tether_qa` jalankan `go test`? | **Ya, wajib.** ▸ "Run the project's test suite... Use the exit code." |
| Perlu `/subagent log`? | Tidak diatur sumber → **ditunda**, cukup ringkasan blok |
| Larang semua shell untuk subagent? | **Tidak.** ▸ subset dari parent, bukan larangan total |
