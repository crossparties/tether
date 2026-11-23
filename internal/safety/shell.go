package safety

import (
	"path/filepath"
	"strings"
)

type shellSegKind int

const (
	segUnsafe shellSegKind = iota
	segCD
	segReadOnly
	segValidation
)

type shellToken struct {
	val      string
	unquoted bool
}

var dangerousEnvVars = map[string]bool{
	"LD_PRELOAD":            true,
	"LD_LIBRARY_PATH":       true,
	"DYLD_INSERT_LIBRARIES": true,
	"DYLD_LIBRARY_PATH":     true,
	"PATH":                  true,
	"BASH_ENV":              true,
	"ENV":                   true,
	"IFS":                   true,
	"PROMPT_COMMAND":        true,
	"GIT_SSH":               true,
	"GIT_SSH_COMMAND":       true,
	"GIT_EXTERNAL_DIFF":     true,
	"GIT_EXEC_PATH":         true,
	"PERL5OPT":              true,
	"PYTHONINSPECT":         true,
}

func analyzeShellCommand(cmdStr string) (allSafe bool, hasReadOnly bool, hasValidation bool, summaryBin string) {
	trimmed := strings.TrimSpace(cmdStr)
	if trimmed == "" || containsSensitiveShellTarget(trimmed) {
		return false, false, false, ""
	}

	rawSegs, ok := splitShellSegments(trimmed)
	if !ok || len(rawSegs) == 0 {
		return false, false, false, ""
	}

	nonCDCount := 0
	for _, seg := range rawSegs {
		toks, tokOK := tokenizeShellSegment(seg)
		if !tokOK || len(toks) == 0 {
			return false, false, false, ""
		}
		cleaned, redirOK := stripBenignRedirections(toks)
		if !redirOK || len(cleaned) == 0 {
			return false, false, false, ""
		}
		stripped, envOK := stripSafeEnvAssignments(cleaned)
		if !envOK || len(stripped) == 0 {
			return false, false, false, ""
		}

		kind, label := classifyShellTokens(stripped)
		switch kind {
		case segUnsafe:
			return false, false, false, ""
		case segCD:
			// cd is only safe when chained with another validated command.
			continue
		case segReadOnly:
			nonCDCount++
			hasReadOnly = true
			if summaryBin == "" {
				summaryBin = label
			}
		case segValidation:
			nonCDCount++
			hasValidation = true
			summaryBin = label
		}
	}

	if nonCDCount == 0 {
		return false, false, false, ""
	}
	return true, hasReadOnly, hasValidation, summaryBin
}

func containsSensitiveShellTarget(cmd string) bool {
	if isSensitiveTarget(cmd) {
		return true
	}
	lower := strings.ToLower(cmd)
	for _, pat := range []string{
		"/etc/passwd", "/etc/shadow", "/etc/sudoers",
		"/.ssh/", "~/.ssh", "id_rsa", "id_ed25519",
		"credentials.json", ".npmrc", ".netrc", ".pypirc",
	} {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}

// splitShellSegments splits cmd on unquoted &&, ||, ;, |, and newlines while
// rejecting dangerous shell constructs (backticks, $(), ${}, <(), >(), background &).
func splitShellSegments(cmd string) ([]string, bool) {
	var segs []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	escaped := false

	flush := func() bool {
		s := strings.TrimSpace(cur.String())
		cur.Reset()
		if s == "" {
			return false
		}
		segs = append(segs, s)
		return true
	}

	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			continue
		}
		if c == '\\' && !inSingle {
			escaped = true
			cur.WriteByte(c)
			continue
		}
		if c == '\'' && !inDouble {
			inSingle = !inSingle
			cur.WriteByte(c)
			continue
		}
		if c == '"' && !inSingle {
			inDouble = !inDouble
			cur.WriteByte(c)
			continue
		}

		// Outside single quotes, reject command substitution, process substitution, and subshells.
		if !inSingle {
			if c == '`' {
				return nil, false
			}
			if c == '$' && i+1 < len(cmd) && (cmd[i+1] == '(' || cmd[i+1] == '{') {
				return nil, false
			}
			if !inDouble && (c == '<' || c == '>') && i+1 < len(cmd) && cmd[i+1] == '(' {
				return nil, false
			}
		}

		if !inSingle && !inDouble {
			// Subshell grouping (...) is rejected for auto-approval.
			if c == '(' || c == ')' {
				return nil, false
			}
			switch c {
			case '\n', ';':
				if !flush() {
					// Allow trailing newline or semicolon? Disallow empty double ;;
					if len(segs) == 0 {
						return nil, false
					}
					// If followed by more non-whitespace tokens, next loop will fail or succeed; disallow empty segments like "a ;; b".
					rest := strings.TrimSpace(cmd[i+1:])
					if rest != "" {
						return nil, false
					}
				}
				continue
			case '&':
				if i+1 < len(cmd) && cmd[i+1] == '&' {
					if !flush() {
						return nil, false
					}
					i++
					continue
				}
				// Allow fd merges like 2>&1, >&1, 1>&2, >&2 (handled later), reject &> and background &.
				prevIsMerge := i > 0 && cmd[i-1] == '>'
				nextIsFD := i+1 < len(cmd) && (cmd[i+1] == '1' || cmd[i+1] == '2')
				if prevIsMerge && nextIsFD {
					cur.WriteByte(c)
					continue
				}
				return nil, false
			case '|':
				if !flush() {
					return nil, false
				}
				if i+1 < len(cmd) && cmd[i+1] == '|' {
					i++
				}
				continue
			}
		}
		cur.WriteByte(c)
	}

	if escaped || inSingle || inDouble {
		return nil, false
	}
	if strings.TrimSpace(cur.String()) != "" {
		segs = append(segs, strings.TrimSpace(cur.String()))
	} else if len(segs) > 0 {
		// Trailing operator like "ls |" or "ls &&" is invalid.
		lastTrim := strings.TrimRight(cmd, " \t\n;")
		if strings.HasSuffix(lastTrim, "|") || strings.HasSuffix(lastTrim, "&") {
			return nil, false
		}
	}
	return segs, len(segs) > 0
}

func tokenizeShellSegment(seg string) ([]shellToken, bool) {
	var tokens []shellToken
	var cur strings.Builder
	inSingle, inDouble := false, false
	escaped := false
	hasToken := false
	tokenHadQuotes := false

	push := func() {
		if !hasToken {
			return
		}
		tokens = append(tokens, shellToken{
			val:      cur.String(),
			unquoted: !tokenHadQuotes,
		})
		cur.Reset()
		hasToken = false
		tokenHadQuotes = false
	}

	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			hasToken = true
			continue
		}
		switch {
		case c == '\\' && !inSingle:
			escaped = true
			hasToken = true
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			hasToken = true
			tokenHadQuotes = true
		case c == '"' && !inSingle:
			inDouble = !inDouble
			hasToken = true
			tokenHadQuotes = true
		case (c == ' ' || c == '\t') && !inSingle && !inDouble:
			push()
		default:
			cur.WriteByte(c)
			hasToken = true
		}
	}
	if escaped || inSingle || inDouble {
		return nil, false
	}
	push()
	return tokens, len(tokens) > 0
}

// stripBenignRedirections removes harmless stderr/null redirections (`2>&1`, `2>/dev/null`,
// `>/dev/null`, `1>/dev/null`, `</dev/null`) and rejects any other unquoted `<` or `>`.
func stripBenignRedirections(toks []shellToken) ([]shellToken, bool) {
	var out []shellToken
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if !t.unquoted {
			out = append(out, t)
			continue
		}
		v := t.val
		if !strings.ContainsAny(v, "<>") {
			out = append(out, t)
			continue
		}
		switch v {
		case "2>&1", "1>&2", ">&2", ">&1", "2>/dev/null", ">/dev/null", "1>/dev/null", "</dev/null":
			continue
		case "2>", ">", "1>", "<":
			if i+1 < len(toks) && toks[i+1].val == "/dev/null" {
				i++
				continue
			}
			return nil, false
		default:
			return nil, false
		}
	}
	return out, true
}

func stripSafeEnvAssignments(toks []shellToken) ([]shellToken, bool) {
	i := 0
	for i < len(toks) {
		v := toks[i].val
		eq := strings.IndexByte(v, '=')
		if eq <= 0 {
			break
		}
		name := v[:eq]
		if !isEnvIdentifier(name) {
			break
		}
		if dangerousEnvVars[strings.ToUpper(name)] {
			return nil, false
		}
		i++
	}
	if i >= len(toks) {
		return nil, false
	}
	return toks[i:], true
}

func isEnvIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' {
			continue
		}
		if i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}

func classifyShellTokens(toks []shellToken) (shellSegKind, string) {
	if len(toks) == 0 {
		return segUnsafe, ""
	}
	bin := filepath.Base(toks[0].val)
	args := toks[1:]

	for _, a := range args {
		if containsSensitiveShellTarget(a.val) {
			return segUnsafe, ""
		}
	}

	switch bin {
	case "cd":
		if len(args) > 1 {
			return segUnsafe, ""
		}
		if len(args) == 1 {
			p := filepath.ToSlash(strings.TrimSpace(args[0].val))
			if p == ".git" || strings.Contains(p, "/.git") || strings.Contains(p, ".ssh") || strings.HasPrefix(p, "/etc") {
				return segUnsafe, ""
			}
		}
		return segCD, "cd"

	case "date", "pwd", "whoami", "uname", "hostname", "id", "uptime",
		"ls", "dir", "stat", "wc", "head", "tail", "cat", "nl", "tac", "rev",
		"grep", "egrep", "fgrep", "ag", "ack", "which", "whereis",
		"basename", "dirname", "realpath", "readlink", "file",
		"uniq", "cut", "tr", "paste", "comm", "diff", "cmp", "du", "df", "ps",
		"jq", "echo", "printf", "true", "false":
		return segReadOnly, bin

	case "tree":
		for _, a := range args {
			if a.val == "-o" || strings.HasPrefix(a.val, "-o=") {
				return segUnsafe, ""
			}
		}
		return segReadOnly, bin

	case "rg":
		for _, a := range args {
			if a.val == "--pre" || strings.HasPrefix(a.val, "--pre=") || a.val == "--pre-glob" || strings.HasPrefix(a.val, "--pre-glob=") {
				return segUnsafe, ""
			}
		}
		return segReadOnly, bin

	case "fd", "fdfind":
		for _, a := range args {
			if a.val == "-x" || a.val == "-X" || a.val == "--exec" || a.val == "--exec-batch" ||
				strings.HasPrefix(a.val, "--exec=") || strings.HasPrefix(a.val, "--exec-batch=") {
				return segUnsafe, ""
			}
		}
		return segReadOnly, bin

	case "sort":
		for _, a := range args {
			if a.val == "-o" || a.val == "--output" || strings.HasPrefix(a.val, "-o=") || strings.HasPrefix(a.val, "--output=") ||
				a.val == "--compress-program" || strings.HasPrefix(a.val, "--compress-program=") {
				return segUnsafe, ""
			}
		}
		return segReadOnly, bin

	case "find":
		for _, a := range args {
			low := strings.ToLower(a.val)
			switch low {
			case "-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fprint0", "-fprintf", "-fls":
				return segUnsafe, ""
			}
			for _, bad := range []string{"-exec=", "-execdir=", "-ok=", "-okdir=", "-fprint", "-fls"} {
				if strings.HasPrefix(low, bad) {
					return segUnsafe, ""
				}
			}
		}
		return segReadOnly, bin

	case "sed":
		if isSafeReadOnlySed(args) {
			return segReadOnly, "sed"
		}
		return segUnsafe, ""

	case "git":
		if sub, ok := classifyReadOnlyGit(args); ok {
			return segReadOnly, "git " + sub
		}
		return segUnsafe, ""

	case "go":
		return classifyGoCommand(args)

	case "gofmt":
		for _, a := range args {
			if a.val == "-w" || a.val == "--write" {
				return segUnsafe, ""
			}
		}
		return segValidation, "gofmt"

	case "npm", "pnpm", "yarn", "bun":
		if len(args) == 0 {
			return segUnsafe, ""
		}
		sub := args[0].val
		if sub == "--version" || sub == "-v" {
			return segReadOnly, bin + " " + sub
		}
		if sub == "test" {
			return segValidation, bin + " test"
		}
		if sub == "run" && len(args) >= 2 {
			script := strings.ToLower(args[1].val)
			if script == "test" || strings.HasPrefix(script, "test:") ||
				script == "lint" || strings.HasPrefix(script, "lint:") ||
				script == "typecheck" || script == "check" || script == "format:check" {
				return segValidation, bin + " run " + script
			}
		}
		return segUnsafe, ""

	case "npx":
		if isSafeValidationNpx(args) {
			return segValidation, "npx"
		}
		return segUnsafe, ""

	case "cargo":
		if len(args) == 0 {
			return segUnsafe, ""
		}
		switch args[0].val {
		case "version", "--version", "-V":
			return segReadOnly, "cargo " + args[0].val
		case "test", "check", "clippy":
			return segValidation, "cargo " + args[0].val
		case "fmt":
			for _, a := range args[1:] {
				if a.val == "--check" {
					return segValidation, "cargo fmt --check"
				}
			}
		}
		return segUnsafe, ""

	case "pytest", "py.test":
		return segValidation, "pytest"

	case "python", "python3":
		if len(args) == 1 && (args[0].val == "--version" || args[0].val == "-V") {
			return segReadOnly, bin + " --version"
		}
		if len(args) >= 2 && args[0].val == "-m" {
			switch args[1].val {
			case "pytest", "unittest":
				return segValidation, bin + " -m " + args[1].val
			}
		}
		return segUnsafe, ""

	case "make":
		if len(args) == 0 {
			return segUnsafe, ""
		}
		for _, a := range args {
			if strings.HasPrefix(a.val, "-") {
				if a.val == "-f" || a.val == "--file" || a.val == "-C" || a.val == "--directory" {
					continue
				}
				return segUnsafe, ""
			}
			switch strings.ToLower(a.val) {
			case "test", "check", "lint", "vet":
			default:
				return segUnsafe, ""
			}
		}
		return segValidation, "make " + args[0].val

	case "node", "rustc":
		if len(args) == 1 && (args[0].val == "-v" || args[0].val == "--version" || args[0].val == "-V") {
			return segReadOnly, bin
		}
		return segUnsafe, ""
	}

	return segUnsafe, ""
}

func isSafeReadOnlySed(args []shellToken) bool {
	if len(args) == 0 {
		return false
	}
	hasQuiet := false
	hasExpr := false
	positionalIdx := 0
	for i := 0; i < len(args); i++ {
		v := args[i].val
		if v == "-n" || v == "--quiet" || v == "--silent" {
			hasQuiet = true
			continue
		}
		if v == "-f" || v == "--file" || strings.HasPrefix(v, "--file=") ||
			v == "-i" || strings.HasPrefix(v, "-i=") || v == "--in-place" || strings.HasPrefix(v, "--in-place=") {
			return false
		}
		if v == "-e" || v == "--expression" {
			if i+1 >= len(args) || isUnsafeSedExpr(args[i+1].val) {
				return false
			}
			hasExpr = true
			i++
			continue
		}
		if strings.HasPrefix(v, "--expression=") {
			if isUnsafeSedExpr(strings.TrimPrefix(v, "--expression=")) {
				return false
			}
			hasExpr = true
			continue
		}
		if strings.HasPrefix(v, "-") && !strings.HasPrefix(v, "--") {
			if strings.ContainsRune(v[1:], 'i') || strings.ContainsRune(v[1:], 'f') {
				return false
			}
			if strings.ContainsRune(v[1:], 'n') {
				hasQuiet = true
			}
			continue
		}
		if strings.HasPrefix(v, "-") {
			continue
		}
		// Without -e/--expression, the first positional argument is the sed script itself;
		// subsequent positional arguments are input filenames.
		if !hasExpr && positionalIdx == 0 {
			if isUnsafeSedExpr(v) {
				return false
			}
		}
		positionalIdx++
	}
	return hasQuiet
}

func isUnsafeSedExpr(expr string) bool {
	clauses := strings.FieldsFunc(expr, func(r rune) bool {
		return r == ';' || r == '\n'
	})
	for _, raw := range clauses {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		// Advance past optional line/regex addresses (digits, $, ,, +, ~, !, /regex/, spaces).
		for s != "" {
			c := s[0]
			if (c >= '0' && c <= '9') || c == '$' || c == ',' || c == '+' || c == '~' || c == '!' || c == ' ' || c == '\t' {
				s = s[1:]
				continue
			}
			if c == '/' || c == '\\' {
				delim := byte('/')
				start := 1
				if c == '\\' && len(s) >= 2 {
					delim = s[1]
					start = 2
				}
				j := start
				escaped := false
				for j < len(s) {
					if escaped {
						escaped = false
					} else if s[j] == '\\' {
						escaped = true
					} else if s[j] == delim {
						j++
						break
					}
					j++
				}
				s = strings.TrimSpace(s[j:])
				continue
			}
			break
		}
		if s == "" {
			continue
		}
		low := strings.ToLower(s)
		// Substitution command: check trailing flags (after 3rd delimiter) for e/w.
		if len(low) >= 2 && low[0] == 's' && !((low[1] >= 'a' && low[1] <= 'z') || (low[1] >= '0' && low[1] <= '9')) {
			sep := low[1]
			delimCount := 0
			lastDelim := -1
			escaped := false
			for j := 1; j < len(low); j++ {
				if escaped {
					escaped = false
					continue
				}
				if low[j] == '\\' {
					escaped = true
					continue
				}
				if low[j] == sep {
					delimCount++
					lastDelim = j
					if delimCount == 3 {
						break
					}
				}
			}
			if delimCount >= 3 && lastDelim+1 < len(low) {
				flags := low[lastDelim+1:]
				if strings.ContainsRune(flags, 'e') || strings.ContainsRune(flags, 'w') {
					return true
				}
			}
			continue
		}
		// Reject execute ('e'), file write ('w'/'W'), or file read ('r'/'R') verbs.
		switch low[0] {
		case 'e', 'w', 'r':
			return true
		}
	}
	return false
}

func classifyReadOnlyGit(args []shellToken) (string, bool) {
	i := 0
	for i < len(args) {
		v := args[i].val
		if v == "-C" || v == "--git-dir" || v == "--work-tree" {
			i += 2
			continue
		}
		if v == "--no-pager" || v == "-p" || v == "--paginate" {
			i++
			continue
		}
		// Disallow `-c`, `--exec-path`, `--upload-pack` which can run arbitrary binaries.
		if strings.HasPrefix(v, "-") {
			return "", false
		}
		break
	}
	if i >= len(args) {
		return "", false
	}
	sub := args[i].val
	rest := args[i+1:]
	switch sub {
	case "status", "log", "show", "rev-parse", "ls-files", "ls-tree", "grep", "blame",
		"shortlog", "describe", "symbolic-ref", "name-rev", "cat-file", "merge-base", "diff-tree":
		return sub, true
	case "diff":
		for _, a := range rest {
			if a.val == "--ext-diff" || a.val == "--output" || strings.HasPrefix(a.val, "--output=") {
				return "", false
			}
		}
		return sub, true
	case "branch":
		for _, a := range rest {
			if strings.HasPrefix(a.val, "-") {
				switch a.val {
				case "-a", "--all", "-r", "--remotes", "-v", "-vv", "--verbose", "--list", "-l", "--show-current":
					continue
				default:
					if strings.HasPrefix(a.val, "--contains") || strings.HasPrefix(a.val, "--merged") || strings.HasPrefix(a.val, "--no-merged") || strings.HasPrefix(a.val, "--sort=") {
						continue
					}
					return "", false
				}
			}
			// Positional branch name without --list creates a branch!
			return "", false
		}
		return sub, true
	case "tag":
		if len(rest) == 0 {
			return sub, true
		}
		hasListFlag := false
		for _, a := range rest {
			switch a.val {
			case "-l", "--list":
				hasListFlag = true
			case "-n", "--sort":
				continue
			default:
				if strings.HasPrefix(a.val, "--sort=") || strings.HasPrefix(a.val, "--contains") ||
					strings.HasPrefix(a.val, "--merged") || strings.HasPrefix(a.val, "--no-merged") {
					continue
				}
				if strings.HasPrefix(a.val, "-") {
					return "", false
				}
				// Positional arg without -l/--list creates a new tag!
				if !hasListFlag {
					return "", false
				}
			}
		}
		return sub, true
	case "remote":
		if len(rest) == 0 || (len(rest) == 1 && (rest[0].val == "-v" || rest[0].val == "--verbose")) {
			return sub, true
		}
		return "", false
	}
	return "", false
}

func classifyGoCommand(args []shellToken) (shellSegKind, string) {
	if len(args) == 0 {
		return segUnsafe, ""
	}
	sub := args[0].val
	switch sub {
	case "version", "env", "list", "doc":
		return segReadOnly, "go " + sub
	case "vet":
		return segValidation, "go vet"
	case "test", "build":
		for _, a := range args[1:] {
			if a.val == "-o" || strings.HasPrefix(a.val, "-o=") || a.val == "-exec" || strings.HasPrefix(a.val, "-exec=") || strings.HasPrefix(a.val, "-toolexec") {
				return segUnsafe, ""
			}
		}
		return segValidation, "go " + sub
	case "mod":
		if len(args) >= 2 {
			switch args[1].val {
			case "verify", "why", "graph":
				return segValidation, "go mod " + args[1].val
			}
		}
		return segUnsafe, ""
	default:
		return segUnsafe, ""
	}
}

func isSafeValidationNpx(args []shellToken) bool {
	i := 0
	for i < len(args) && (args[i].val == "--no-install" || args[i].val == "--yes" || args[i].val == "-y") {
		if args[i].val == "--yes" || args[i].val == "-y" {
			return false // Never auto-install missing tools via npx!
		}
		i++
	}
	if i >= len(args) {
		return false
	}
	tool := filepath.Base(args[i].val)
	rest := args[i+1:]
	switch tool {
	case "tsc":
		for _, a := range rest {
			if a.val == "--noEmit" || a.val == "--noemit" || a.val == "--version" || a.val == "-v" {
				return true
			}
		}
		return false
	case "eslint":
		for _, a := range rest {
			if a.val == "--fix" || a.val == "--fix-dry-run" || a.val == "-o" || a.val == "--output-file" {
				return false
			}
		}
		return true
	case "prettier":
		for _, a := range rest {
			if a.val == "--check" || a.val == "-c" || a.val == "--list-different" || a.val == "-l" {
				return true
			}
		}
		return false
	case "vitest":
		return len(rest) >= 1 && rest[0].val == "run"
	case "jest":
		return true
	}
	return false
}
