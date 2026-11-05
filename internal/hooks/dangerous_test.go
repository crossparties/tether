package hooks

import "testing"

// TestClassifyCommand pins the behaviour that motivated replacing substring
// matching with argv parsing: deleting a subdirectory must be allowed, while
// every shape that reaches the root, home, or a system directory is denied.
func TestClassifyCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    severity
	}{
		// The regression that started this: a normal recursive delete inside a
		// project used to be blocked because the string contains "rm -rf /".
		{name: "rm -rf temp dir", command: "rm -rf /tmp/build", want: allow},
		{name: "rm -rf nested tmp", command: "rm -rf /tmp/a/b/c", want: allow},
		{name: "rm -rf relative dir", command: "rm -rf ./internal/cache", want: allow},
		{name: "rm -rf node_modules", command: "rm -rf node_modules", want: allow},
		{name: "rm single file", command: "rm build/output.bin", want: allow},
		{name: "rm with tmp suffix", command: "rm -rf /tmpx", want: allow},

		// Real catastrophes.
		{name: "rm -rf root", command: "rm -rf /", want: deny},
		{name: "rm -rf root glob", command: "rm -rf /*", want: deny},
		{name: "rm -rf home tilde", command: "rm -rf ~", want: deny},
		{name: "rm -rf home var", command: "rm -rf $HOME", want: deny},
		{name: "rm -rf etc", command: "rm -rf /etc", want: deny},
		{name: "rm -rf usr", command: "rm -rf /usr", want: deny},
		{name: "rm -rf var", command: "rm -rf /var", want: deny},
		{name: "rm -fr bundled flags", command: "rm -fr /", want: deny},
		{name: "rm -Rf bundled flags", command: "rm -Rf /", want: deny},
		{name: "rm no-preserve-root", command: "rm -rf --no-preserve-root /", want: deny},
		{name: "rm root with trailing dot", command: "rm -rf /.", want: deny},

		// Chained and nested forms that substring matching missed.
		{name: "chained cd then root", command: "cd /tmp && rm -rf /", want: deny},
		{name: "semicolon chain", command: "echo hi; rm -rf /", want: deny},
		{name: "pipe into shell", command: "echo cm0gLXJmIC8= | sh -c 'rm -rf /'", want: deny},
		{name: "bash -c root", command: `bash -c "rm -rf /"`, want: deny},
		{name: "find exec rm root", command: "find / -exec rm -rf {} +", want: deny},
		{name: "find delete root", command: "find / -delete", want: deny},
		{name: "xargs rm root", command: "echo / | xargs rm -rf", want: deny},
		{name: "tar to-command", command: "tar -xf a.tar --to-command=sh", want: deny},
		{name: "awk system", command: `awk 'BEGIN{system("rm -rf /")}'`, want: deny},
		{name: "mkfs", command: "mkfs.ext4 /dev/sda1", want: deny},
		{name: "dd to device", command: "dd if=/dev/zero of=/dev/sda", want: deny},
		{name: "chmod -R root", command: "chmod -R 777 /", want: deny},
		{name: "chown -R root", command: "chown -R nobody /etc", want: deny},
		{name: "shred root", command: "shred -u /", want: deny},

		// Recursion that stays inside a project must not be blocked.
		{name: "find exec rm relative", command: "find . -name '*.tmp' -exec rm {} +", want: allow},
		{name: "chmod -R project", command: "chmod -R 755 ./scripts", want: allow},
		{name: "awk without system", command: `awk '{print $1}' file.txt`, want: allow},
		{name: "tar extract normal", command: "tar -xzf archive.tar.gz", want: allow},
		{name: "git clean", command: "git clean -fd", want: allow},
		{name: "env assignment prefix", command: "FOO=1 go test ./...", want: allow},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyCommand(tc.command)
			if got.level != tc.want {
				level := "allow"
				if got.level == deny {
					level = "deny"
				}
				t.Fatalf("classifyCommand(%q) = %s, want %s", tc.command, level, wantLabel(tc.want))
			}
		})
	}
}

func wantLabel(s severity) string {
	if s == deny {
		return "deny"
	}
	return "allow"
}

// TestClassifyCommandDenyMessageNamesToken guards the user-facing contract: a
// denial must say which token tripped the guard, not echo the whole command.
func TestClassifyCommandDenyMessageNamesToken(t *testing.T) {
	d := classifyCommand("rm -rf /tmp/x && rm -rf /")
	if d.level != deny {
		t.Fatalf("expected deny, got allow")
	}
	if d.token == "" {
		t.Fatalf("denial must carry a token")
	}
	if msg := d.describe(); msg == "" {
		t.Fatalf("denial must render a message")
	}
}

// TestTokenizeRespectsQuotes keeps the parser honest, since every classifier
// decision depends on argv being split correctly.
func TestTokenizeRespectsQuotes(t *testing.T) {
	tests := []struct {
		segment string
		want    []string
	}{
		{segment: `rm -rf /tmp`, want: []string{"rm", "-rf", "/tmp"}},
		{segment: `rm -rf "/my dir"`, want: []string{"rm", "-rf", "/my dir"}},
		{segment: `echo 'a b'`, want: []string{"echo", "a b"}},
		{segment: `awk 'BEGIN{system("x")}'`, want: []string{"awk", `BEGIN{system("x")}`}},
		{segment: `  spaced   out  `, want: []string{"spaced", "out"}},
	}
	for _, tc := range tests {
		got := tokenize(tc.segment)
		if len(got) != len(tc.want) {
			t.Fatalf("tokenize(%q) = %v, want %v", tc.segment, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("tokenize(%q)[%d] = %q, want %q", tc.segment, i, got[i], tc.want[i])
			}
		}
	}
}

// TestSplitCommandSegmentsKeepsQuotedOperators ensures `echo "a && b"` is one
// command, not two, so a quoted semicolon cannot hide a denial.
func TestSplitCommandSegmentsKeepsQuotedOperators(t *testing.T) {
	got := splitCommandSegments(`echo "a && b"; rm -rf /`)
	if len(got) != 2 {
		t.Fatalf("expected 2 segments, got %d: %v", len(got), got)
	}
	if got[1] != "rm -rf /" {
		t.Fatalf("second segment = %q, want %q", got[1], "rm -rf /")
	}
}
