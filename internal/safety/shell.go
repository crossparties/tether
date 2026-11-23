package safety

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/crossparties/tether/internal/mount"
)

type shellSegKind int

const (
	segUnsafe shellSegKind = iota
	segCD
	segReadOnly
	segValidation
)

type shellToken struct {
	val          string
	unquoted     bool
	singleQuoted bool
	hasUnsafeVar bool
}

// CheckShellCommand enforces Point 2 security containment on shell commands,
// failing closed on workspace escapes, sensitive/ignored path arguments,
// dangerous redirections, glob arguments that can expand to secrets/ignored paths,
// and unresolvable subshell/command/process indirection ($(...), backticks, <(), >()).
func CheckShellCommand(cmdStr, workspaceRoot string) (bool, string) {
	return CheckShellCommandWithMount(cmdStr, workspaceRoot, nil)
}

// CheckShellCommandWithMount enforces Point 2 security containment on shell commands
// using workspace mount/ignore containment plus an optional live mount authorizer
// (e.g., enforcing DB-bound mounted_files and active governance mode).
func CheckShellCommandWithMount(cmdStr, workspaceRoot string, authorize func(rawPath string, write bool) (bool, string)) (bool, string) {
	trimmed := strings.TrimSpace(cmdStr)
	if trimmed == "" {
		return false, "Tether Guard: empty shell command"
	}
	root := strings.TrimSpace(workspaceRoot)
	if root == "" {
		root = "."
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false, fmt.Sprintf("Tether Guard: cannot resolve workspace root: %v", err)
	}
	realRoot, err := mount.EvalSymlinksWithAncestor(absRoot)
	if err != nil {
		return false, fmt.Sprintf("Tether Guard: cannot canonicalize workspace root: %v", err)
	}
	mountEngine := mount.NewEngine(absRoot)
	return checkShellCommandDepth(trimmed, absRoot, realRoot, mountEngine, authorize, 0)
}

func checkShellCommandDepth(cmd, absRoot, curDir string, mountEngine *mount.Engine, authorize func(rawPath string, write bool) (bool, string), depth int) (bool, string) {
	if depth > 4 {
		return false, "Tether Guard: nested shell depth exceeded"
	}
	rawSegs, ok := splitShellSegments(cmd)
	if !ok || len(rawSegs) == 0 {
		return false, "Tether Guard: unresolvable shell syntax or command/subshell indirection ($(...), backticks, <(), >()) is blocked"
	}

	userRules := mountEngine.CustomRules()

	for segIdx, seg := range rawSegs {
		toks, tokOK := tokenizeShellSegment(seg)
		if !tokOK || len(toks) == 0 {
			return false, "Tether Guard: malformed or unquoted shell tokens are blocked"
		}

		// Reject unquoted/double-quoted variable expansion anywhere in command tokens.
		for _, t := range toks {
			if t.hasUnsafeVar {
				return false, fmt.Sprintf("Tether Guard: unresolvable variable expansion in %q is blocked", t.val)
			}
			if t.unquoted && hasBraceExpansion(t.val) {
				return false, fmt.Sprintf("Tether Guard: brace expansion %q is blocked", t.val)
			}
		}

		// Check all redirections against mountEngine before stripping benign ones.
		cleaned, redirOK, redirReason := inspectAndStripRedirections(toks, absRoot, curDir, mountEngine, authorize)
		if !redirOK || len(cleaned) == 0 {
			return false, redirReason
		}

		stripped, envOK := stripSafeEnvAssignments(cleaned)
		if !envOK || len(stripped) == 0 {
			return false, "Tether Guard: dangerous environment assignment or empty command is blocked"
		}

		bin := filepath.Base(stripped[0].val)
		args := stripped[1:]

		// Unwrap `env` prefix if present.
		if bin == "env" {
			i := 0
			for i < len(args) {
				v := args[i].val
				if eq := strings.IndexByte(v, '='); eq > 0 && isEnvIdentifier(v[:eq]) {
					if dangerousEnvVars[strings.ToUpper(v[:eq])] {
						return false, fmt.Sprintf("Tether Guard: dangerous env override %q is blocked", v)
					}
					i++
					continue
				}
				if v == "-C" || strings.HasPrefix(v, "-C") || v == "--chdir" || strings.HasPrefix(v, "--chdir=") {
					return false, "Tether Guard: env directory change (-C/--chdir) is blocked"
				}
				if strings.HasPrefix(v, "-") {
					i++
					continue
				}
				break
			}
			if i >= len(args) {
				continue
			}
			bin = filepath.Base(args[i].val)
			args = args[i+1:]
		}

		if bin == "cd" {
			if len(args) != 1 {
				return false, "Tether Guard: cd without single workspace directory is blocked"
			}
			nextDir, cdOK, reason := resolveShellDir(args[0].val, absRoot, curDir, mountEngine, authorize)
			if !cdOK {
				return false, reason
			}
			curDir = nextDir
			continue
		}

		// Nested shell invocations: parse safe options + -c/-ec/-lc script recursively;
		// fail closed on unrecognized flags, script files, or piped stdin execution.
		if isPOSIXShellBin(bin) || bin == "eval" {
			if bin == "eval" {
				var parts []string
				for _, a := range args {
					parts = append(parts, a.val)
				}
				inner := strings.TrimSpace(strings.Join(parts, " "))
				if inner == "" {
					return false, "Tether Guard: empty eval is blocked"
				}
				if ok, reason := checkShellCommandDepth(inner, absRoot, curDir, mountEngine, authorize, depth+1); !ok {
					return false, reason
				}
				continue
			}
			cScriptIdx, hasC, shErr := parsePOSIXShellCommandFlag(bin, args)
			if shErr != "" {
				return false, shErr
			}
			if !hasC {
				_ = segIdx
				return false, fmt.Sprintf("Tether Guard: interactive or uninspected %s invocation is blocked", bin)
			}
			if ok, reason := checkShellCommandDepth(args[cScriptIdx].val, absRoot, curDir, mountEngine, authorize, depth+1); !ok {
				return false, reason
			}
			for _, tail := range args[cScriptIdx+1:] {
				if ok, reason := inspectTrailingPositionalToken(tail, absRoot, curDir, mountEngine, userRules, authorize); !ok {
					return false, reason
				}
			}
			continue
		}

		if ok, reason := inspectSegmentArgs(bin, args, absRoot, curDir, mountEngine, userRules, authorize); !ok {
			return false, reason
		}
	}

	return true, ""
}

func isPOSIXShellBin(bin string) bool {
	switch bin {
	case "sh", "bash", "zsh", "dash", "ksh", "ash":
		return true
	}
	return false
}

func parsePOSIXShellCommandFlag(bin string, args []shellToken) (int, bool, string) {
	if len(args) == 0 {
		return -1, false, fmt.Sprintf("Tether Guard: interactive or piped %s execution is blocked", bin)
	}
	for i := 0; i < len(args); i++ {
		v := args[i].val
		if v == "-o" || v == "+o" {
			if i+1 >= len(args) {
				return -1, false, fmt.Sprintf("Tether Guard: malformed %s -o option is blocked", bin)
			}
			i++
			continue
		}
		if v == "--norc" || v == "--noprofile" || v == "--login" || v == "--posix" {
			continue
		}
		if strings.HasPrefix(v, "-") && !strings.HasPrefix(v, "--") && len(v) > 1 {
			hasC := false
			for _, r := range v[1:] {
				switch r {
				case 'c':
					hasC = true
				case 'e', 'u', 'x', 'l', 'v', 'h', 'f', 'p':
				default:
					return -1, false, fmt.Sprintf("Tether Guard: unrecognized %s flag %q is blocked", bin, v)
				}
			}
			if hasC {
				if i+1 >= len(args) {
					return -1, false, fmt.Sprintf("Tether Guard: %s %s missing command string is blocked", bin, v)
				}
				return i + 1, true, ""
			}
			continue
		}
		return -1, false, fmt.Sprintf("Tether Guard: uninspected %s flag or script %q is blocked", bin, v)
	}
	return -1, false, ""
}

func inspectTrailingPositionalToken(tok shellToken, absRoot, curDir string, mountEngine *mount.Engine, userRules []mount.Rule, authorize func(string, bool) (bool, string)) (bool, string) {
	v := tok.val
	if v == "" || v == "-" || v == "_" || v == "/dev/null" {
		return true, ""
	}
	if hasGlobMeta(v) {
		return checkGlobArg(v, tok.singleQuoted, absRoot, curDir, mountEngine, userRules, authorize)
	}
	if containsSensitiveShellTarget(v) || containsSensitiveInlineRef(v, false) {
		return false, fmt.Sprintf("Tether Guard: sensitive path %q is blocked", v)
	}
	if _, isSec := mount.MatchesBuiltinSecret(v); isSec {
		return false, fmt.Sprintf("Tether Guard: protected secret path %q is blocked", v)
	}
	if looksLikePathArg(v) || pathExistsAt(curDir, v) {
		return authorizeShellPath(v, absRoot, curDir, mountEngine, authorize, mount.OpRead, false)
	}
	candidate := filepath.Join(curDir, v)
	if !mountEngine.AuthorizePath(candidate, mount.OpRead, "code").Allowed {
		return false, fmt.Sprintf("Tether Guard: ignored path %q is blocked", v)
	}
	if authorize != nil {
		if ok, reason := authorize(candidate, false); !ok {
			return false, reason
		}
	}
	return true, ""
}

func inspectAndStripRedirections(toks []shellToken, absRoot, curDir string, mountEngine *mount.Engine, authorize func(string, bool) (bool, string)) ([]shellToken, bool, string) {
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
		case ">", ">>", ">|", "&>", "1>", "1>>", "2>", "2>>", "<", "<>":
			if i+1 >= len(toks) {
				return nil, false, "Tether Guard: incomplete redirection is blocked"
			}
			targetTok := toks[i+1]
			i++
			op := mount.OpWrite
			if v == "<" {
				op = mount.OpRead
			}
			if targetTok.val == "/dev/null" && (v == ">" || v == "1>" || v == "2>" || v == "<") {
				continue
			}
			if ok, reason := authorizeRedirectionTarget(targetTok, absRoot, curDir, mountEngine, authorize, op); !ok {
				return nil, false, reason
			}
			continue
		default:
			// Handle attached redirection like `>.env`, `>>out.txt`, `2>/tmp/x`, `<.env`.
			prefix, redirOp, target, splitOK := splitInlineRedirection(v)
			if !splitOK || target == "" {
				return nil, false, fmt.Sprintf("Tether Guard: unsupported shell redirection %q is blocked", v)
			}
			if prefix != "" && prefix != "1" && prefix != "2" && prefix != "&" {
				out = append(out, shellToken{val: prefix, unquoted: true})
			}
			if target == "/dev/null" && (prefix == "" || prefix == "1" || prefix == "2") && (redirOp == ">" || redirOp == "<") {
				continue
			}
			op := mount.OpWrite
			if redirOp == "<" {
				op = mount.OpRead
			}
			if ok, reason := authorizeRedirectionTarget(shellToken{val: target, unquoted: true}, absRoot, curDir, mountEngine, authorize, op); !ok {
				return nil, false, reason
			}
		}
	}
	return out, true, ""
}

func splitInlineRedirection(tok string) (prefix, op, target string, ok bool) {
	for i := 0; i < len(tok); i++ {
		if tok[i] != '<' && tok[i] != '>' {
			continue
		}
		prefix = tok[:i]
		// Prefix before < or > must be empty, fd digit(s), &, or an ordinary command arg.
		j := i
		if tok[j] == '>' && j+1 < len(tok) && (tok[j+1] == '>' || tok[j+1] == '|') {
			j += 2
		} else if tok[j] == '<' && j+1 < len(tok) && tok[j+1] == '>' {
			j += 2
		} else {
			j++
		}
		op = tok[i:j]
		target = tok[j:]
		return prefix, op, target, target != ""
	}
	return "", "", "", false
}

func authorizeRedirectionTarget(targetTok shellToken, absRoot, curDir string, mountEngine *mount.Engine, authorize func(string, bool) (bool, string), op mount.Operation) (bool, string) {
	target := strings.TrimSpace(targetTok.val)
	if target == "" || hasGlobMeta(target) || targetTok.hasUnsafeVar || strings.ContainsAny(target, "$`") {
		return false, fmt.Sprintf("Tether Guard: unresolvable or glob redirection target %q is blocked", target)
	}
	return authorizeShellPath(target, absRoot, curDir, mountEngine, authorize, op, false)
}

func resolveShellDir(rawDir, absRoot, curDir string, mountEngine *mount.Engine, authorize func(string, bool) (bool, string)) (string, bool, string) {
	trimmed := strings.TrimSpace(rawDir)
	if trimmed == "" || trimmed == "-" || trimmed == "~" || strings.HasPrefix(trimmed, "~/") {
		return "", false, fmt.Sprintf("Tether Guard: cd to %q escapes workspace", rawDir)
	}
	if hasGlobMeta(trimmed) {
		return "", false, fmt.Sprintf("Tether Guard: glob in cd %q is blocked", rawDir)
	}
	candidate := trimmed
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(curDir, candidate)
	}
	rel, canon, _, err := mount.ResolveWithinWorkspace(candidate, absRoot)
	if err != nil {
		return "", false, err.Error()
	}
	if rel == "." {
		return canon, true, ""
	}
	d := mountEngine.AuthorizePath(candidate, mount.OpRead, "code")
	if !d.Allowed {
		return "", false, d.Reason
	}
	if authorize != nil {
		if ok, reason := authorize(candidate, false); !ok {
			return "", false, reason
		}
	}
	return d.CanonicalPath, true, ""
}

func inspectSegmentArgs(bin string, args []shellToken, absRoot, curDir string, mountEngine *mount.Engine, userRules []mount.Rule, authorize func(string, bool) (bool, string)) (bool, string) {
	skipNextAsValue := false
	sawExplicitPatternFlag := false
	sawExplicitScriptFlag := false
	positionalIdx := 0

	for i := 0; i < len(args); i++ {
		tok := args[i]
		v := tok.val
		if v == "" || v == "-" || v == "/dev/null" {
			continue
		}

		if skipNextAsValue {
			skipNextAsValue = false
			// Still verify sensitive/glob values on pattern flags (e.g. find -name '*.pem').
			if hasGlobMeta(v) {
				if ok, reason := checkGlobArg(v, tok.singleQuoted, absRoot, curDir, mountEngine, userRules, authorize); !ok {
					return false, reason
				}
			} else if containsSensitiveShellTarget(v) {
				return false, fmt.Sprintf("Tether Guard: sensitive target %q is blocked", v)
			}
			continue
		}

		if strings.HasPrefix(v, "-") {
			if v == "-e" || v == "--regexp" || v == "--expression" || v == "-f" || v == "--file" ||
				strings.HasPrefix(v, "--regexp=") || strings.HasPrefix(v, "--expression=") || strings.HasPrefix(v, "--file=") {
				sawExplicitPatternFlag = true
				sawExplicitScriptFlag = true
			}

			// Inline scripts/expressions attached via --eval=, --exec=, --source=, --expression=, or -c/-e flags.
			if eq := strings.IndexByte(v, '='); eq > 0 {
				flagName := v[:eq]
				val := v[eq+1:]
				if isInlineScriptFlag(bin, flagName) {
					if ok, reason := inspectInlineScript(bin, val, absRoot, curDir, mountEngine, userRules, authorize); !ok {
						return false, reason
					}
					sawExplicitScriptFlag = true
					continue
				}
				if ok, reason := inspectArgValue(bin, flagName, val, tok.singleQuoted, absRoot, curDir, mountEngine, userRules, authorize); !ok {
					return false, reason
				}
				continue
			}

			// Next-arg inline script flags (exact -c/-e/--eval/--source/--expression or bundled short flags ending in c/e like -uc, -pe, -ne).
			if isInlineScriptNextArgFlag(bin, v) {
				sawExplicitScriptFlag = true
				if i+1 < len(args) {
					if ok, reason := inspectInlineScript(bin, args[i+1].val, absRoot, curDir, mountEngine, userRules, authorize); !ok {
						return false, reason
					}
					i++
				}
				continue
			}

			// Short flags with attached inline values (e.g. `grep -f.env`, `sort -ocustom.txt`, `git -C../out`, `python3 -c...`).
			if !strings.HasPrefix(v, "--") && len(v) > 2 {
				flagChar := v[1]
				rest := v[2:]
				if isInlineScriptShortPrefix(bin, flagChar) {
					sawExplicitScriptFlag = true
					if ok, reason := inspectInlineScript(bin, rest, absRoot, curDir, mountEngine, userRules, authorize); !ok {
						return false, reason
					}
					continue
				}
				if ok, reason := inspectAttachedShortFlag(bin, v[:2], rest, tok.singleQuoted, absRoot, curDir, mountEngine, userRules, authorize); !ok {
					return false, reason
				}
				continue
			}

			if takesNextFlagValue(bin, v) {
				if isPathFlag(v) {
					if i+1 < len(args) {
						if ok, reason := inspectArgValue(bin, v, args[i+1].val, args[i+1].singleQuoted, absRoot, curDir, mountEngine, userRules, authorize); !ok {
							return false, reason
						}
						i++
						continue
					}
				}
				skipNextAsValue = true
			}
			continue
		}

		pos := positionalIdx
		positionalIdx++

		// Positional inline expression for sed / awk when -e/-f was omitted.
		if !sawExplicitScriptFlag && pos == 0 && (bin == "sed" || bin == "awk" || bin == "gawk" || bin == "nawk") {
			if ok, reason := inspectInlineScript(bin, v, absRoot, curDir, mountEngine, userRules, authorize); !ok {
				return false, reason
			}
			continue
		}

		// Key=value path operands (e.g. `dd if=.env of=custom_secret.txt`).
		if eq := strings.IndexByte(v, '='); eq > 0 && !hasGlobMeta(v[:eq]) {
			key := v[:eq]
			val := v[eq+1:]
			if ok, reason := inspectArgValue(bin, key, val, tok.singleQuoted, absRoot, curDir, mountEngine, userRules, authorize); !ok {
				return false, reason
			}
			continue
		}

		// Skip non-path first positional arg for pattern/expression tools unless -e/-f was already supplied.
		if isPatternFirstCmd(bin) && !sawExplicitPatternFlag && pos == 0 {
			if containsSensitiveShellTarget(v) {
				return false, fmt.Sprintf("Tether Guard: sensitive reference %q is blocked", v)
			}
			continue
		}
		if (bin == "echo" || bin == "printf") && !hasGlobMeta(v) {
			if containsSensitiveShellTarget(v) {
				return false, fmt.Sprintf("Tether Guard: sensitive reference %q is blocked", v)
			}
			continue
		}

		if hasGlobMeta(v) {
			if ok, reason := checkGlobArg(v, tok.singleQuoted, absRoot, curDir, mountEngine, userRules, authorize); !ok {
				return false, reason
			}
			continue
		}

		if containsSensitiveShellTarget(v) {
			return false, fmt.Sprintf("Tether Guard: sensitive path %q is blocked", v)
		}
		if _, isSec := mount.MatchesBuiltinSecret(v); isSec {
			return false, fmt.Sprintf("Tether Guard: protected secret path %q is blocked", v)
		}

		if v == "./..." || v == "..." {
			continue
		}

		if isFileIOCmd(bin) || looksLikePathArg(v) || pathExistsAt(curDir, v) {
			op := mount.OpRead
			if isMutatingFileCmd(bin) {
				op = mount.OpWrite
			}
			if ok, reason := authorizeShellPath(v, absRoot, curDir, mountEngine, authorize, op, true); !ok {
				return false, reason
			}
		}
	}
	return true, ""
}

func isInterpreterBin(bin string) bool {
	switch bin {
	case "python", "python3", "python2", "node", "nodejs", "perl", "ruby":
		return true
	}
	return false
}

func isInlineScriptFlag(bin, flagName string) bool {
	if isInterpreterBin(bin) {
		return flagName == "-c" || flagName == "-e" || flagName == "--eval" || flagName == "--exec"
	}
	switch bin {
	case "awk", "gawk", "nawk":
		return flagName == "-e" || flagName == "--source"
	case "sed":
		return flagName == "-e" || flagName == "--expression"
	}
	return false
}

func isInlineScriptShortPrefix(bin string, flagChar byte) bool {
	if isInterpreterBin(bin) {
		return flagChar == 'c' || flagChar == 'e'
	}
	if bin == "sed" || bin == "awk" || bin == "gawk" || bin == "nawk" {
		return flagChar == 'e'
	}
	return false
}

func isInlineScriptNextArgFlag(bin, flag string) bool {
	if isInterpreterBin(bin) {
		if flag == "-c" || flag == "-e" || flag == "--eval" || flag == "--exec" {
			return true
		}
		if !strings.HasPrefix(flag, "--") && len(flag) > 1 {
			last := flag[len(flag)-1]
			return last == 'c' || last == 'e'
		}
		return false
	}
	if bin == "awk" || bin == "gawk" || bin == "nawk" {
		return flag == "-e" || flag == "--source"
	}
	if bin == "sed" {
		if flag == "-e" || flag == "--expression" {
			return true
		}
		return !strings.HasPrefix(flag, "--") && len(flag) > 1 && flag[len(flag)-1] == 'e'
	}
	return false
}

func isPathFlag(flag string) bool {
	switch strings.ToLower(flag) {
	case "-c", "--git-dir", "--work-tree", "-f", "--file", "-o", "--output",
		"--out", "--in", "--input", "--config", "--conf", "--dir", "--path",
		"--root", "--target", "--src", "--dst", "--dest", "if", "of",
		"out", "in", "file", "output", "input", "config":
		return true
	}
	return flag == "-C"
}

func isWriteFlag(bin, flag string) bool {
	switch strings.ToLower(flag) {
	case "-o", "--output", "--out", "of", "out", "output", "dest", "dst", "--dest":
		return true
	}
	if isMutatingFileCmd(bin) {
		return true
	}
	return false
}

func inspectAttachedShortFlag(bin, shortFlag, rest string, singleQuoted bool, absRoot, curDir string, mountEngine *mount.Engine, userRules []mount.Rule, authorize func(string, bool) (bool, string)) (bool, string) {
	if rest == "" {
		return true, ""
	}
	switch shortFlag[1] {
	case 'f', 'o', 'C', 'I', 'T', 'w', 'r', 'd':
		return inspectArgValue(bin, shortFlag, rest, singleQuoted, absRoot, curDir, mountEngine, userRules, authorize)
	}
	if containsSensitiveShellTarget(rest) || looksLikePathArg(rest) {
		return inspectArgValue(bin, shortFlag, rest, singleQuoted, absRoot, curDir, mountEngine, userRules, authorize)
	}
	if _, isSec := mount.MatchesBuiltinSecret(rest); isSec {
		return false, fmt.Sprintf("Tether Guard: protected secret path %q is blocked", rest)
	}
	return true, ""
}

func inspectArgValue(bin, flagName, val string, singleQuoted bool, absRoot, curDir string, mountEngine *mount.Engine, userRules []mount.Rule, authorize func(string, bool) (bool, string)) (bool, string) {
	if val == "" || val == "/dev/null" {
		return true, ""
	}
	if hasGlobMeta(val) {
		return checkGlobArg(val, singleQuoted, absRoot, curDir, mountEngine, userRules, authorize)
	}
	if containsSensitiveShellTarget(val) {
		return false, fmt.Sprintf("Tether Guard: sensitive target %q is blocked", val)
	}
	if _, isSec := mount.MatchesBuiltinSecret(val); isSec {
		return false, fmt.Sprintf("Tether Guard: protected secret path %q is blocked", val)
	}
	op := mount.OpRead
	if isWriteFlag(bin, flagName) || bin == "sort" || bin == "tree" {
		op = mount.OpWrite
	}
	if isPathFlag(flagName) || looksLikePathArg(val) || pathExistsAt(curDir, val) {
		return authorizeShellPath(val, absRoot, curDir, mountEngine, authorize, op, true)
	}
	// Also fail closed when a non-pathlike value directly matches .tether/ignore or DB mount overrides.
	candidate := filepath.Join(curDir, val)
	if !mountEngine.AuthorizePath(candidate, op, "code").Allowed {
		return false, fmt.Sprintf("Tether Guard: ignored path %q is blocked", val)
	}
	if authorize != nil {
		if ok, reason := authorize(candidate, op == mount.OpWrite); !ok {
			return false, reason
		}
	}
	return true, ""
}

func inspectInlineScript(bin, script, absRoot, curDir string, mountEngine *mount.Engine, userRules []mount.Rule, authorize func(string, bool) (bool, string)) (bool, string) {
	if strings.TrimSpace(script) == "" {
		return true, ""
	}
	if containsSensitiveInlineRef(script, bin == "sed" || bin == "awk" || bin == "gawk" || bin == "nawk") {
		return false, fmt.Sprintf("Tether Guard: %s inline script access to sensitive or outside-workspace path is blocked", bin)
	}

	if bin == "sed" {
		// Reject sed file read/write/exec/brace verbs when targeting unauthorized files or running shell commands.
		if isUnsafeSedExpr(script) {
			return false, "Tether Guard: unsafe sed file/execute expression is blocked"
		}
		// Inspect any explicit r/w clause targets as well.
		for _, tok := range extractScriptStringCandidates(script, false) {
			candidate := filepath.Join(curDir, tok)
			if pathExistsAt(curDir, tok) || !mountEngine.AuthorizePath(candidate, mount.OpRead, "code").Allowed {
				if ok, reason := authorizeShellPath(tok, absRoot, curDir, mountEngine, authorize, mount.OpRead, false); !ok {
					return false, reason
				}
			}
		}
		return true, ""
	}

	isAwk := bin == "awk" || bin == "gawk" || bin == "nawk"
	for _, sub := range extractScriptStringCandidates(script, !isAwk) {
		if hasGlobMeta(sub) {
			if (strings.ContainsAny(sub, "/*.") || !isAwk) && mount.GlobMayMatchSensitive(sub, userRules) {
				return false, fmt.Sprintf("Tether Guard: glob %q in %s script can match protected secret or ignored paths", sub, bin)
			}
		}
		if containsSensitiveShellTarget(sub) {
			return false, fmt.Sprintf("Tether Guard: sensitive path %q in %s script is blocked", sub, bin)
		}
		if _, isSec := mount.MatchesBuiltinSecret(sub); isSec {
			return false, fmt.Sprintf("Tether Guard: protected secret path %q in %s script is blocked", sub, bin)
		}
		// Check both explicit paths/existing files and custom .tether/ignore filenames.
		if looksLikePathArg(sub) || pathExistsAt(curDir, sub) {
			if ok, reason := authorizeShellPath(sub, absRoot, curDir, mountEngine, authorize, mount.OpRead, true); !ok {
				return false, reason
			}
			continue
		}
		candidate := filepath.Join(curDir, sub)
		if !mountEngine.AuthorizePath(candidate, mount.OpRead, "code").Allowed {
			return false, fmt.Sprintf("Tether Guard: ignored path %q in %s script is blocked", sub, bin)
		}
		if authorize != nil {
			if ok, reason := authorize(candidate, false); !ok {
				return false, reason
			}
		}
	}
	return true, ""
}

func containsSensitiveInlineRef(script string, regexLang bool) bool {
	if containsSensitiveShellTarget(script) {
		return true
	}
	low := strings.ToLower(script)
	for _, pat := range []string{
		".env", ".pem", ".key", ".p12", ".pfx", ".kdbx",
		".npmrc", ".pypirc", ".netrc", "kubeconfig",
		"id_rsa", "id_ed25519", "id_ecdsa", "id_dsa",
		"credentials", "session.db", "cache.db",
		".git", ".ssh", ".tether",
		"/tmp/", "/home/", "/root/", "/var/", "/proc/", "/sys/", "/etc/",
		"~/", "../", "..\\",
	} {
		if strings.Contains(low, pat) {
			return true
		}
	}
	if !regexLang && strings.Contains(script, "..") {
		return true
	}
	return false
}

// extractScriptStringCandidates extracts quoted string literals and (when includeUnquoted is true)
// word/path-like tokens from an inline script for security inspection.
func extractScriptStringCandidates(script string, includeUnquoted bool) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || s == "." || s == ".." || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	// Extract '...' and "..." literals verbatim.
	var quote byte
	start := -1
	for i := 0; i < len(script); i++ {
		c := script[i]
		if quote != 0 {
			if c == '\\' && quote == '"' && i+1 < len(script) {
				i++
				continue
			}
			if c == quote {
				add(script[start:i])
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			start = i + 1
		}
	}

	if includeUnquoted {
		fields := strings.FieldsFunc(script, func(r rune) bool {
			switch r {
			case ' ', '\t', '\n', '\r', '\'', '"', '`', '(', ')', '[', ']', '{', '}', ',', ';', '=', '<', '>':
				return true
			}
			return false
		})
		for _, f := range fields {
			add(f)
		}
	} else {
		// For awk/sed: also inspect file redirect targets after `<` / `>`.
		fields := strings.FieldsFunc(script, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '\n' || r == ';' || r == '<' || r == '>'
		})
		for _, f := range fields {
			f = strings.Trim(f, `'"()`)
			add(f)
		}
	}
	return out
}

func checkGlobArg(globVal string, singleQuoted bool, absRoot, curDir string, mountEngine *mount.Engine, userRules []mount.Rule, authorize func(string, bool) (bool, string)) (bool, string) {
	if strings.Contains(globVal, "..") || filepath.IsAbs(globVal) || strings.HasPrefix(globVal, "~") {
		return false, fmt.Sprintf("Tether Guard: glob %q outside workspace is blocked", globVal)
	}
	relGlob := globVal
	if relDir, err := filepath.Rel(absRoot, curDir); err == nil && relDir != "." && !strings.HasPrefix(relDir, "..") {
		relGlob = filepath.ToSlash(filepath.Join(relDir, globVal))
	}
	if mount.GlobMayMatchSensitive(relGlob, userRules) || mount.GlobMayMatchSensitive(globVal, userRules) {
		return false, fmt.Sprintf("Tether Guard: glob %q can match protected secret or ignored paths", globVal)
	}
	if singleQuoted {
		return true, ""
	}

	// Check the static prefix before the first glob metacharacter.
	if idx := strings.IndexAny(globVal, "*?["); idx > 0 {
		prefix := strings.TrimSuffix(globVal[:idx], "/")
		if prefix != "" && prefix != "." {
			if ok, reason := authorizeShellPath(prefix, absRoot, curDir, mountEngine, authorize, mount.OpRead, true); !ok {
				return false, reason
			}
		}
	}

	// Expand safely in Go and verify every matched workspace path is authorized.
	absGlob := filepath.Join(curDir, globVal)
	matches, err := filepath.Glob(absGlob)
	if err == nil {
		for _, m := range matches {
			if ok, reason := authorizeShellPath(m, absRoot, curDir, mountEngine, authorize, mount.OpRead, true); !ok {
				return false, reason
			}
		}
	}
	return true, ""
}

func authorizeShellPath(rawArg, absRoot, curDir string, mountEngine *mount.Engine, authorize func(string, bool) (bool, string), op mount.Operation, allowRootDot bool) (bool, string) {
	if rawArg == "/dev/null" {
		return true, ""
	}
	if strings.HasPrefix(rawArg, "~") {
		return false, fmt.Sprintf("Tether Guard: path %q escapes workspace", rawArg)
	}
	candidate := rawArg
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(curDir, candidate)
	}
	rel, _, _, rerr := mount.ResolveWithinWorkspace(candidate, absRoot)
	if rerr != nil {
		return false, rerr.Error()
	}
	if rel == "." {
		if allowRootDot && op == mount.OpRead {
			return true, ""
		}
		return false, "Tether Guard: cannot mutate workspace root directory"
	}
	d := mountEngine.AuthorizePath(candidate, op, "code")
	if !d.Allowed {
		return false, d.Reason
	}
	if authorize != nil {
		if ok, reason := authorize(candidate, op == mount.OpWrite); !ok {
			return false, reason
		}
	}
	return true, ""
}

func hasGlobMeta(s string) bool {
	return strings.ContainsAny(s, "*?[")
}

func hasBraceExpansion(s string) bool {
	open := strings.IndexByte(s, '{')
	if open < 0 {
		return false
	}
	close := strings.IndexByte(s[open+1:], '}')
	if close <= 0 {
		return false
	}
	inner := s[open+1 : open+1+close]
	return strings.Contains(inner, ",") || strings.Contains(inner, "..")
}

func looksLikePathArg(s string) bool {
	return strings.Contains(s, "/") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~")
}

func pathExistsAt(dir, p string) bool {
	candidate := p
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(dir, p)
	}
	_, err := os.Lstat(candidate)
	return err == nil
}

func isPatternFirstCmd(bin string) bool {
	switch bin {
	case "grep", "egrep", "fgrep", "rg", "ag", "ack", "sed", "awk", "gawk", "nawk":
		return true
	}
	return false
}

func isFileIOCmd(bin string) bool {
	switch bin {
	case "cat", "head", "tail", "less", "more", "nl", "tac", "rev", "stat", "wc",
		"ls", "dir", "find", "tree", "file", "realpath", "readlink", "diff", "cmp",
		"sort", "uniq", "cut", "tr", "paste", "comm", "jq", "tee", "rm", "mv",
		"cp", "touch", "mkdir", "chmod", "chown", "ln", "sqlite3", "base64",
		"xxd", "od", "strings", "tar", "zip", "unzip", "gzip", "curl", "wget",
		"scp", "rsync", "grep", "egrep", "fgrep", "rg", "ag", "ack", "sed", "awk":
		return true
	}
	return false
}

func isMutatingFileCmd(bin string) bool {
	switch bin {
	case "rm", "mv", "cp", "tee", "touch", "mkdir", "chmod", "chown", "ln", "truncate":
		return true
	}
	return false
}

func takesNextFlagValue(bin, flag string) bool {
	switch flag {
	case "-C", "--git-dir", "--work-tree", "-f", "--file", "-o", "--output":
		return true
	}
	switch bin {
	case "find":
		return flag == "-name" || flag == "-iname" || flag == "-path" || flag == "-ipath" || flag == "-regex"
	case "rg", "grep", "egrep", "fgrep":
		return flag == "-e" || flag == "-g" || flag == "--glob" || flag == "--include" || flag == "-m" || flag == "-A" || flag == "-B" || flag == "-C"
	case "git":
		return flag == "-l" || flag == "-n" || flag == "--format"
	case "go":
		return flag == "-run" || flag == "-tags"
	case "head", "tail":
		return flag == "-n" || flag == "-c"
	case "sed":
		return flag == "-e" || flag == "--expression"
	}
	return false
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
		for _, t := range toks {
			if t.hasUnsafeVar || (t.unquoted && hasBraceExpansion(t.val)) {
				return false, false, false, ""
			}
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
		"/.ssh/", "~/.ssh", "id_rsa", "id_ed25519", "id_ecdsa", "id_dsa",
		"credentials.json", ".npmrc", ".netrc", ".pypirc", "kubeconfig", ".kdbx",
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
	tokenHadSingle := false
	tokenHadDoubleOrUnquoted := false
	tokenHasUnsafeVar := false

	push := func() {
		if !hasToken {
			return
		}
		tokens = append(tokens, shellToken{
			val:          cur.String(),
			unquoted:     !tokenHadQuotes,
			singleQuoted: tokenHadSingle && !tokenHadDoubleOrUnquoted,
			hasUnsafeVar: tokenHasUnsafeVar,
		})
		cur.Reset()
		hasToken = false
		tokenHadQuotes = false
		tokenHadSingle = false
		tokenHadDoubleOrUnquoted = false
		tokenHasUnsafeVar = false
	}

	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			hasToken = true
			if !inSingle {
				tokenHadDoubleOrUnquoted = true
			}
			continue
		}
		switch {
		case c == '\\' && !inSingle:
			escaped = true
			hasToken = true
			tokenHadDoubleOrUnquoted = true
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			hasToken = true
			tokenHadQuotes = true
			tokenHadSingle = true
		case c == '"' && !inSingle:
			inDouble = !inDouble
			hasToken = true
			tokenHadQuotes = true
			tokenHadDoubleOrUnquoted = true
		case (c == ' ' || c == '\t') && !inSingle && !inDouble:
			push()
		default:
			if !inSingle {
				tokenHadDoubleOrUnquoted = true
				if c == '$' && i+1 < len(seg) {
					next := seg[i+1]
					if (next >= 'A' && next <= 'Z') || (next >= 'a' && next <= 'z') || next == '_' || next == '{' || next == '(' || next == '@' || next == '*' || next == '?' {
						tokenHasUnsafeVar = true
					}
				}
			}
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
		if low[0] == '{' || low[0] == '}' {
			return true
		}
		// Substitution command: check trailing flags (after 3rd delimiter) for e/w.
		isSubCmd := len(low) >= 2 && low[0] == 's' && !((low[1] >= 'a' && low[1] <= 'z') || (low[1] >= '0' && low[1] <= '9'))
		if isSubCmd {
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
		if strings.ContainsRune(low, '{') || strings.ContainsRune(low, '}') {
			return true
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
		for _, a := range args[1:] {
			if a.val == "-vettool" || strings.HasPrefix(a.val, "-vettool=") ||
				a.val == "-toolexec" || strings.HasPrefix(a.val, "-toolexec=") {
				return segUnsafe, ""
			}
		}
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
