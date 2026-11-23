package safety

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckShellCommandSecurity(t *testing.T) {
	ws := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outsideFile, []byte("outside-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "cmd"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "cmd", "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("SECRET=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "custom_secret.txt"), []byte("ignored"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".tether"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".tether", "ignore"), []byte("custom_secret.txt\nsecrets/\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_ = os.Symlink(outsideFile, filepath.Join(ws, "evil_link.txt"))
	_ = os.Symlink(filepath.Join(ws, ".env"), filepath.Join(ws, "env_link.txt"))

	safeCmds := []string{
		"git status",
		"git status --short | head -n 20",
		"go test ./...",
		"go vet ./...",
		"ls -la cmd/",
		"find . -name '*.go' | sort",
		"grep \"main\" cmd/main.go",
		"cat .tetherignore",
	}
	for _, cmd := range safeCmds {
		if ok, reason := CheckShellCommand(cmd, ws); !ok {
			t.Errorf("expected safe command %q to pass CheckShellCommand, denied: %s", cmd, reason)
		}
	}

	blockedCmds := []string{
		"cat .env",
		"cat .env.production",
		"cat .e*",
		"head *.pem",
		"cat credentials*.json",
		"cat $(echo .env)",
		"cat `echo .env`",
		"cat <(cat .env)",
		"echo secret > .env",
		"echo x >> .tether/session.db",
		"echo x > .tether/ignore",
		"cat >.env",
		"cat custom_secret.txt",
		"cat custom_*.txt",
		"cat evil_link.txt",
		"cat env_link.txt",
		"cat ../outside.txt",
		"cat /etc/passwd",
		"F=.env && cat $F",
		`sh -c "cat .env"`,
		"grep -e main .env",
		"cat {.env,cmd/main.go}",
		"cd .. && cat outside.txt",
		"cd .tether && cat session.db",
	}
	for _, cmd := range blockedCmds {
		if ok, _ := CheckShellCommand(cmd, ws); ok {
			t.Errorf("expected dangerous/secret command %q to be DENIED by CheckShellCommand", cmd)
		}
	}
}

func TestIsNonCriticalToolEnforcesReadSymlinkAndIgnore(t *testing.T) {
	ws := t.TempDir()
	outside := filepath.Join(t.TempDir(), "host_secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "private.txt"), []byte("priv"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".tether"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".tether", "ignore"), []byte("private.txt\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_ = os.Symlink(outside, filepath.Join(ws, "read_escape.txt"))

	if ok, _ := IsNonCriticalTool("code", "read_file", "main.go", "", ws); !ok {
		t.Errorf("expected read_file on normal workspace file to be non-critical")
	}
	for _, blocked := range []string{"read_escape.txt", "../outside.txt", "private.txt", ".env", ".tether/session.db"} {
		if ok, _ := IsNonCriticalTool("code", "read_file", blocked, "", ws); ok {
			t.Errorf("expected read_file(%q) NOT to pass IsNonCriticalTool", blocked)
		}
	}
}

func TestShellGuardBypassesAndHardening(t *testing.T) {
	ws := t.TempDir()
	outsideFile := filepath.Join(t.TempDir(), "host_secret.txt")
	_ = os.WriteFile(outsideFile, []byte("outside"), 0600)
	_ = os.MkdirAll(filepath.Join(ws, "cmd"), 0755)
	_ = os.WriteFile(filepath.Join(ws, "cmd", "main.go"), []byte("package main\n"), 0644)
	_ = os.WriteFile(filepath.Join(ws, ".env"), []byte("SECRET=1\n"), 0600)
	_ = os.WriteFile(filepath.Join(ws, "custom-ignored.txt"), []byte("cust\n"), 0600)
	_ = os.MkdirAll(filepath.Join(ws, ".tether"), 0755)
	_ = os.WriteFile(filepath.Join(ws, ".tether", "ignore"), []byte("custom-ignored.txt\nre:^internal_key.*$\n"), 0644)
	if err := os.Symlink(outsideFile, filepath.Join(ws, "escape_link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	// Safe bundled shell -ec, sed -n, awk, and multi-stack validation commands pass.
	for _, safe := range []string{
		`sh -ec "ls -la cmd/"`,
		`bash -lc "git status"`,
		`sed -n '1p' cmd/main.go`,
		`awk '{print $1}' cmd/main.go`,
		`cd web && npm run check`,
	} {
		if safe == `cd web && npm run check` {
			_ = os.MkdirAll(filepath.Join(ws, "web"), 0755)
		}
		if ok, reason := CheckShellCommand(safe, ws); !ok {
			t.Errorf("expected safe command %q allowed, got: %s", safe, reason)
		}
	}

	bypasses := []string{
		// Flag=val / attached short flags / key=value
		"grep --file=.env main cmd/main.go",
		"tool --config=escape_link",
		"sort --output=custom-ignored.txt cmd/main.go",
		"grep -f.env main cmd/main.go",
		"dd if=.env of=out.txt",
		"dd if=cmd/main.go of=custom-ignored.txt",
		"dd if=escape_link of=out.txt",
		// Bundled/uninspected shell flags and positional script args
		`sh -ec "cat .env"`,
		`bash -lc "cat .env"`,
		`sh -sc "cat .env"`,
		`sh -c 'cat "$1"' _ .env`,
		"bash script.sh",
		// Inline interpreters & sed/awk
		`python3 -c 'print(open(".env").read())'`,
		`python3 -uc 'print(open("custom-ignored.txt").read())'`,
		`node -e 'require("fs").readFileSync(".env")'`,
		`perl -e 'open(F,".env")'`,
		`awk 'BEGIN{while((getline < ".env")>0) print}'`,
		`awk 'BEGIN{while((getline < "custom-ignored.txt")>0) print}'`,
		`sed -e 'r .env' cmd/main.go`,
		`sed -e 'r custom-ignored.txt' cmd/main.go`,
		`sed -n '1{e id}' cmd/main.go`,
	}
	for _, cmd := range bypasses {
		if ok, _ := CheckShellCommand(cmd, ws); ok {
			t.Errorf("expected bypass command %q to be DENIED", cmd)
		}
	}

	// CheckShellCommandWithMount enforces custom DB/hook authorizer policy too.
	auth := func(raw string, write bool) (bool, string) {
		if filepath.Base(raw) == "db_ignored.txt" || write && filepath.Base(raw) == "ro.txt" {
			return false, "Tether Guard: denied by live mount authorizer"
		}
		return true, ""
	}
	if ok, _ := CheckShellCommandWithMount("cat db_ignored.txt", ws, auth); ok {
		t.Errorf("expected CheckShellCommandWithMount to deny custom authorizer read")
	}
	if ok, _ := CheckShellCommandWithMount("echo hi > ro.txt", ws, auth); ok {
		t.Errorf("expected CheckShellCommandWithMount to deny custom authorizer write")
	}
	if ok, _ := CheckShellCommandWithMount("cat cmd/main.go", ws, auth); !ok {
		t.Errorf("expected CheckShellCommandWithMount to allow authorized file")
	}

	// IsReadOnlyCommand & IsValidationCommand hardening
	for _, roNo := range []string{
		"sed -n '1{e id}' cmd/main.go",
		"cat $ENVFILE",
		"cat {a,b}",
	} {
		if ok, _ := IsReadOnlyCommand(roNo); ok {
			t.Errorf("expected IsReadOnlyCommand(%q) = false", roNo)
		}
	}
	for _, valNo := range []string{
		"go vet -vettool=/tmp/evil ./...",
		"go vet -toolexec=/tmp/evil ./...",
		"go test $ARGS",
	} {
		if ok, _ := IsValidationCommand(valNo); ok {
			t.Errorf("expected IsValidationCommand(%q) = false", valNo)
		}
	}
	if ok, _ := IsValidationCommand("cd web && npm run check"); !ok {
		t.Errorf("expected IsValidationCommand('cd web && npm run check') = true")
	}
}
