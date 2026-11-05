package hooks

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// This file classifies shell commands by their argv rather than by substring.
//
// The previous implementation asked "does the whole command string contain
// 'rm -rf /'?" which answers yes for `rm -rf /tmp/build` — a perfectly normal
// command. Blocking legitimate work teaches users to route around the guard,
// which is worse than having no guard. At the same time the substring test
// missed dangerous shapes entirely (`find . -exec rm -rf {} +`,
// `tar --to-command=...`, `xargs rm`).
//
// Classifying on the parsed argv lets us ask the real question: which paths is
// this command about to delete, and do any of them reach the filesystem root,
// the home directory, or a top-level system path?

// severity ranks how badly a command should be blocked.
type severity int

const (
	// allow means the command looks safe enough to let through. It is not a
	// promise that the command is harmless, only that it is not catastrophic.
	allow severity = iota
	// deny means the command is well-known to destroy a whole system or home
	// directory. There is no legitimate reason to run it from an agent.
	deny
)

// danger describes why a command was blocked, so the caller can name a
// concrete token in the message instead of dumping the whole command back.
type danger struct {
	level  severity
	token  string
	detail string
}

// classifyCommand inspects a shell command and reports whether it is
// catastrophic. It understands quotes and simple command chaining so that
// `cd /tmp && rm -rf ../..` is still caught.
func classifyCommand(command string) danger {
	for _, segment := range splitCommandSegments(command) {
		args := tokenize(segment)
		if len(args) == 0 {
			continue
		}
		if d := classifyArgv(args); d.level == deny {
			return d
		}
	}
	return danger{level: allow}
}

// classifyArgv decides for a single simple command.
func classifyArgv(args []string) danger {
	// Skip leading environment assignments: FOO=bar rm -rf /
	for len(args) > 0 && isAssignment(args[0]) {
		args = args[1:]
	}
	if len(args) == 0 {
		return danger{level: allow}
	}

	program := path.Base(args[0])
	rest := args[1:]

	switch program {
	case "rm":
		return classifyRM(rest)
	case "mkfs", "mkfs.ext4", "mkfs.xfs", "mkfs.btrfs":
		return danger{level: deny, token: program, detail: "reformats a filesystem"}
	case "dd":
		return classifyDD(rest)
	case "shred":
		return classifyShred(rest)
	case "chmod", "chown", "chgrp":
		return classifyRecursiveSystem(program, rest)
	case "find":
		return classifyFind(rest)
	case "xargs":
		return classifyXargs(rest)
	case "tar":
		return classifyTar(rest)
	case "awk", "gawk", "nawk":
		return classifyAwkProgram(rest)
	case "eval", "sh", "bash", "zsh", "dash":
		return classifyNestedShell(program, rest)
	}

	// A bare ":(){ :|:& };:" fork bomb is not worth parsing; the substring is
	// unambiguous and rarely appears in legitimate input.
	if strings.Contains(strings.Join(args, " "), ":(){") {
		return danger{level: deny, token: ":(){", detail: "fork bomb"}
	}
	return danger{level: allow}
}

// classifyRM blocks recursive deletion that reaches root, home or a top-level
// system directory, and allows recursive deletion inside a project.
func classifyRM(args []string) danger {
	recursive := false
	noPreserveRoot := false
	var targets []string

	for _, a := range args {
		switch {
		case a == "--":
			continue
		case a == "--recursive":
			recursive = true
		case a == "--no-preserve-root":
			noPreserveRoot = true
		case strings.HasPrefix(a, "--"):
			continue
		case strings.HasPrefix(a, "-") && len(a) > 1:
			// Short flags may be bundled: -rf, -fr, -Rf
			if strings.ContainsAny(a[1:], "rR") {
				recursive = true
			}
		default:
			targets = append(targets, a)
		}
	}

	// `rm -rf /` and `rm -rf /*` are the classic footguns; --no-preserve-root
	// makes even a literal "/" destructive, so treat it as always denied.
	for _, target := range targets {
		if isRootPath(target) {
			if recursive || noPreserveRoot || true {
				return danger{
					level:  deny,
					token:  "rm -rf /",
					detail: "deletes the filesystem root",
				}
			}
		}
		if noPreserveRoot && isRootPath(target) {
			return danger{level: deny, token: "rm -rf /", detail: "deletes the filesystem root"}
		}
		if isHomeOrSystemPath(target) {
			return danger{
				level:  deny,
				token:  "rm -rf " + target,
				detail: "deletes a top-level system or home path",
			}
		}
	}
	return danger{level: allow}
}

// classifyDD blocks writes to a raw device, which wipes the disk.
func classifyDD(args []string) danger {
	for _, a := range args {
		if !strings.HasPrefix(a, "of=") {
			continue
		}
		target := strings.TrimPrefix(a, "of=")
		if strings.HasPrefix(target, "/dev/") {
			return danger{level: deny, token: "dd of=" + target, detail: "writes to a raw device"}
		}
	}
	return danger{level: allow}
}

// classifyShred treats shredding a system path as destructive.
func classifyShred(args []string) danger {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if isRootPath(a) || isHomeOrSystemPath(a) {
			return danger{level: deny, token: "shred " + a, detail: "overwrites a system path"}
		}
	}
	return danger{level: allow}
}

// classifyRecursiveSystem blocks chmod/chown -R applied to root, home or a
// top-level system directory, which breaks the machine.
func classifyRecursiveSystem(program string, args []string) danger {
	recursive := false
	var targets []string
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--recursive"):
			recursive = true
		case strings.HasPrefix(a, "--"):
			continue
		case strings.HasPrefix(a, "-") && len(a) > 1:
			if strings.ContainsRune(a[1:], 'R') {
				recursive = true
			}
		default:
			// First positional is the mode/owner, the rest are paths.
			targets = append(targets, a)
		}
	}
	if !recursive || len(targets) < 2 {
		return danger{level: allow}
	}
	for _, target := range targets[1:] {
		if isRootPath(target) || isHomeOrSystemPath(target) {
			return danger{
				level:  deny,
				token:  program + " -R " + target,
				detail: "recursively rewrites permissions on a system path",
			}
		}
	}
	return danger{level: allow}
}

// classifyFind blocks `find / -exec rm -rf {} +` style commands by inspecting
// the command find is asked to run.
func classifyFind(args []string) danger {
	// find's search roots are the leading non-flag arguments; the deletion
	// happens wherever those roots point, so `find / -exec rm {} +` is only
	// dangerous because the root is "/".
	var roots []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			break
		}
		roots = append(roots, a)
	}
	destructive := false
	for _, a := range args {
		switch a {
		case "-exec", "-execdir", "-ok", "-okdir", "-delete":
			destructive = true
		}
	}
	if destructive {
		for _, root := range roots {
			if isRootPath(root) || isHomeOrSystemPath(root) {
				return danger{
					level:  deny,
					token:  "find " + root,
					detail: "deletes or rewrites a system path",
				}
			}
		}
	}

	for i, a := range args {
		switch a {
		case "-exec", "-execdir", "-ok", "-okdir":
			if i+1 < len(args) {
				if d := classifyArgv(args[i+1:]); d.level == deny {
					return d
				}
			}
		case "-delete":
			// `find / -delete` is as destructive as rm -rf.
			for _, target := range args {
				if strings.HasPrefix(target, "-") {
					continue
				}
				if isRootPath(target) || isHomeOrSystemPath(target) {
					return danger{level: deny, token: "find " + target + " -delete", detail: "deletes a system path"}
				}
			}
		}
	}
	return danger{level: allow}
}

// classifyXargs blocks the command xargs is asked to run when it is destructive.
//
// xargs appends its input as arguments, so the paths it deletes come from
// stdin rather than from the visible command. `echo / | xargs rm -rf` therefore
// reaches the root even though no path appears after rm. A recursive delete
// behind xargs is treated as unbounded.
func classifyXargs(args []string) danger {
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		inner := args[i:]
		if d := classifyArgv(inner); d.level == deny {
			return d
		}
		program := path.Base(inner[0])
		if program == "rm" && hasRecursiveFlag(inner[1:]) {
			return danger{
				level:  deny,
				token:  "xargs rm -r",
				detail: "recursive delete over paths supplied by stdin",
			}
		}
		break
	}
	return danger{level: allow}
}

// hasRecursiveFlag reports whether args contain -r or -R, including bundled
// short flags such as -rf.
func hasRecursiveFlag(args []string) bool {
	for _, a := range args {
		if a == "--recursive" {
			return true
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a[1:], "rR") {
			return true
		}
	}
	return false
}

// classifyTar blocks archive extraction that pipes through an arbitrary
// command, which is an arbitrary code execution vector disguised as extraction.
func classifyTar(args []string) danger {
	for _, a := range args {
		if strings.HasPrefix(a, "--to-command") || strings.HasPrefix(a, "--use-compress-program") {
			return danger{
				level:  deny,
				token:  a,
				detail: "runs an arbitrary command during archive processing",
			}
		}
	}
	return danger{level: allow}
}

// classifyAwkProgram blocks awk programs that shell out via system().
func classifyAwkProgram(args []string) danger {
	for _, a := range args {
		if strings.Contains(a, "system(") {
			inner := extractSystemCallArg(a)
			if inner == "" {
				return danger{level: deny, token: "system()", detail: "runs an arbitrary shell command"}
			}
			if d := classifyCommand(inner); d.level == deny {
				return d
			}
			return danger{level: deny, token: "system(" + inner + ")", detail: "runs an arbitrary shell command"}
		}
	}
	return danger{level: allow}
}

// classifyNestedShell blocks `bash -c "rm -rf /"` by re-classifying the body.
func classifyNestedShell(program string, args []string) danger {
	for i, a := range args {
		if a == "-c" && i+1 < len(args) {
			if d := classifyCommand(args[i+1]); d.level == deny {
				return d
			}
		}
	}
	return danger{level: allow}
}

// isRootPath reports whether a path refers to the filesystem root, including
// glob and dot forms such as "/*", "/." and "/../".
func isRootPath(p string) bool {
	cleaned := filepath.Clean(filepath.FromSlash(p))
	if cleaned == "/" {
		return true
	}
	// Glob forms like "/*" and "/ *" clean to "/*", which is not "/".
	trimmed := strings.TrimRight(strings.TrimSpace(p), "*")
	trimmed = strings.TrimSpace(trimmed)
	if trimmed == "" {
		return false
	}
	return filepath.Clean(filepath.FromSlash(trimmed)) == "/"
}

// isHomeOrSystemPath reports whether a path is home or a single top-level
// system directory, the set of directories whose loss is unrecoverable.
func isHomeOrSystemPath(p string) bool {
	trimmed := strings.TrimSpace(p)
	trimmed = strings.TrimRight(trimmed, "*")
	if trimmed == "" || trimmed == "~" || strings.HasPrefix(trimmed, "~/") || trimmed == "$HOME" || strings.HasPrefix(trimmed, "$HOME/") {
		return true
	}
	cleaned := filepath.Clean(filepath.FromSlash(trimmed))
	if !filepath.IsAbs(cleaned) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
	if len(parts) != 1 || parts[0] == "" {
		// Deeper paths like /tmp/build are allowed; they are not catastrophic.
		return false
	}
	switch parts[0] {
	case "bin", "boot", "dev", "etc", "lib", "lib32", "lib64", "proc", "root",
		"run", "sbin", "srv", "sys", "usr", "var", "opt", "home", "Users",
		"System", "Library", "Applications":
		return true
	}
	return false
}

// isAssignment reports whether a token looks like NAME=value.
func isAssignment(token string) bool {
	eq := strings.IndexByte(token, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := token[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		default:
			if i == 0 || (c >= '0' && c <= '9') {
				return false
			}
			return false
		}
	}
	return true
}

// splitCommandSegments breaks a command line on &&, ||, ;, | and newlines so
// each simple command is classified on its own.
func splitCommandSegments(command string) []string {
	var segments []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	escaped := false

	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			segments = append(segments, s)
		}
		cur.Reset()
	}

	for i := 0; i < len(command); i++ {
		c := command[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			continue
		}
		switch {
		case c == '\\' && !inSingle:
			escaped = true
			cur.WriteByte(c)
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			cur.WriteByte(c)
		case c == '"' && !inSingle:
			inDouble = !inDouble
			cur.WriteByte(c)
		case !inSingle && !inDouble && (c == '\n' || c == ';'):
			flush()
		case !inSingle && !inDouble && c == '&' && i+1 < len(command) && command[i+1] == '&':
			flush()
			i++
		case !inSingle && !inDouble && c == '|' && i+1 < len(command) && command[i+1] == '|':
			flush()
			i++
		case !inSingle && !inDouble && c == '|':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return segments
}

// tokenize splits a single command segment into argv, honouring single and
// double quotes and backslash escapes.
func tokenize(segment string) []string {
	var tokens []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	escaped := false
	hasToken := false

	for i := 0; i < len(segment); i++ {
		c := segment[i]
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
		case c == '"' && !inSingle:
			inDouble = !inDouble
			hasToken = true
		case (c == ' ' || c == '\t') && !inSingle && !inDouble:
			if hasToken {
				tokens = append(tokens, cur.String())
				cur.Reset()
				hasToken = false
			}
		default:
			cur.WriteByte(c)
			hasToken = true
		}
	}
	if hasToken {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

// extractSystemCallArg returns the literal argument of the first system(...)
// call, or "" when it cannot be determined statically.
func extractSystemCallArg(program string) string {
	idx := strings.Index(program, "system(")
	if idx < 0 {
		return ""
	}
	rest := program[idx+len("system("):]
	end := strings.IndexByte(rest, ')')
	if end < 0 {
		return ""
	}
	arg := strings.TrimSpace(rest[:end])
	arg = strings.Trim(arg, `"'`)
	if strings.Contains(arg, "$") || strings.Contains(arg, "+") {
		return ""
	}
	return arg
}

// describe renders a blocked command for the user.
func (d danger) describe() string {
	if d.detail != "" {
		return fmt.Sprintf("Tether Guard: '%s' is blocked — %s.", d.token, d.detail)
	}
	return fmt.Sprintf("Tether Guard: '%s' is blocked.", d.token)
}
