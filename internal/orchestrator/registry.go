package orchestrator

import "strings"

// CommandSpec describes a slash command available in the interactive REPL and
// live command auto-complete menu.
type CommandSpec struct {
	Name        string
	Usage       string
	Description string
	NeedsArg    bool
}

// DefaultCommandSpecs returns the canonical list of slash commands supported by
// Tether's interactive session, including zero-context-pollution `/btw`.
func DefaultCommandSpecs() []CommandSpec {
	return []CommandSpec{
		{
			Name:        "/btw",
			Usage:       "/btw <pertanyaan>",
			Description: "tanyakan sesuatu secara cepat di luar konteks utama (side-question zero pollution)",
			NeedsArg:    true,
		},
		{
			Name:        "/model",
			Usage:       "/model <nama>",
			Description: "pilih atau ganti model LLM aktif",
			NeedsArg:    true,
		},
		{
			Name:        "/read",
			Usage:       "/read <path>",
			Description: "baca isi file langsung ke sesi tanpa mutasi",
			NeedsArg:    true,
		},
		{
			Name:        "/exec",
			Usage:       "/exec <cmd>",
			Description: "jalankan perintah shell dengan pengawasan Tether Guard",
			NeedsArg:    true,
		},
		{
			Name:        "/clear",
			Usage:       "/clear",
			Description: "bersihkan riwayat percakapan di memori (session.db tetap aman)",
			NeedsArg:    false,
		},
		{
			Name:        "/exit",
			Usage:       "/exit",
			Description: "keluar dari sesi interaktif Tether",
			NeedsArg:    false,
		},
		{
			Name:        "/plan",
			Usage:       "/plan",
			Description: "beralih ke mode PLAN (penulisan file dikunci)",
			NeedsArg:    false,
		},
		{
			Name:        "/code",
			Usage:       "/code",
			Description: "beralih ke mode CODE (penulisan file dibuka)",
			NeedsArg:    false,
		},
		{
			Name:        "/provider",
			Usage:       "/provider <nama>",
			Description: "pilih atau ganti provider LLM dan muat API key",
			NeedsArg:    true,
		},
		{
			Name:        "/key",
			Usage:       "/key <api-key>",
			Description: "simpan API key untuk provider aktif",
			NeedsArg:    true,
		},
		{
			Name:        "/status",
			Usage:       "/status",
			Description: "tampilkan status workspace, sesi, mode, KB, dan worker aktif",
			NeedsArg:    false,
		},
		{
			Name:        "/undo",
			Usage:       "/undo [turn]",
			Description: "kembalikan snapshot file dari turn terakhir atau turn tertentu",
			NeedsArg:    false,
		},
		{
			Name:        "/help",
			Usage:       "/help",
			Description: "tampilkan daftar lengkap perintah slash",
			NeedsArg:    false,
		},
	}
}

// FilterCommands returns all CommandSpecs matching the given prefix.
// An empty prefix or "/" returns all registered commands in canonical order.
func FilterCommands(prefix string) []CommandSpec {
	specs := DefaultCommandSpecs()
	q := strings.ToLower(strings.TrimSpace(prefix))
	if q == "" || q == "/" {
		out := make([]CommandSpec, len(specs))
		copy(out, specs)
		return out
	}
	if !strings.HasPrefix(q, "/") {
		q = "/" + q
	}

	var matched []CommandSpec
	for _, spec := range specs {
		if strings.HasPrefix(strings.ToLower(spec.Name), q) {
			matched = append(matched, spec)
		}
	}
	return matched
}

// IsSlashTriggerActive determines whether the live slash-command dropdown
// should be active for the given input buffer and cursor position.
func IsSlashTriggerActive(input string, cursorPos int) (filter string, active bool) {
	if input == "" || cursorPos <= 0 {
		return "", false
	}
	if cursorPos > len(input) {
		cursorPos = len(input)
	}
	if input[0] != '/' {
		return "", false
	}
	sub := input[:cursorPos]
	if strings.ContainsAny(sub, " \t\n\r") || strings.ContainsAny(input, " \t\n\r") {
		return "", false
	}
	return sub, true
}
