package library

import "testing"

func TestResolveKind(t *testing.T) {
	tests := []struct {
		name        string
		code        string
		interpreter string
		want        Kind
	}{
		{"bare word is not an absolute path", "uptime", "", KindScript},
		{"relative command with args", "ls -la /tmp", "", KindScript},
		{"absolute path", "/usr/bin/uptime", "", KindCommand},
		{"absolute path with args", "/bin/ls -la /tmp", "", KindCommand},
		{"pipe", "/bin/echo a | grep a", "", KindScript},
		{"shebang", "#!/bin/sh\necho hi", "", KindScript},
		{"multiple lines", "/bin/echo a\n/bin/echo b", "", KindScript},
		{"windows path with cmd", `C:\Windows\System32\ipconfig.exe`, "cmd", KindCommand},
		{"powershell relative", "Get-Service", "powershell", KindScript},
		{"bash interpreter", "/usr/bin/id", "/bin/bash", KindScript},
		{"tacoscript interpreter", "/usr/bin/id", "tacoscript", KindScript},
		{"legacy default interpreter", "/usr/bin/uptime", "shOrShebang", KindCommand},
		{"legacy system default shell", "/usr/bin/uptime", " systemDefaultShell ", KindCommand},
		{"default keyword", "/usr/bin/uptime", "default", KindCommand},
		{"comma", "/bin/echo a,b", "", KindScript},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveKind(tt.code, tt.interpreter); got != tt.want {
				t.Errorf("ResolveKind(%q, %q) = %q, want %q", tt.code, tt.interpreter, got, tt.want)
			}
		})
	}
}

func TestEffectiveKind(t *testing.T) {
	if got := (Implementation{Kind: KindAuto, Code: "/usr/bin/id"}).EffectiveKind(); got != KindCommand {
		t.Errorf("auto resolved to %q", got)
	}
	if got := (Implementation{Kind: KindScript, Code: "/usr/bin/id"}).EffectiveKind(); got != KindScript {
		t.Errorf("explicit kind overridden: %q", got)
	}
}

func TestNormalizeInterpreter(t *testing.T) {
	for in, want := range map[string]string{"": "", " default ": "", "shOrShebang": "", "SystemDefaultShell": "", "/bin/bash": "/bin/bash", " powershell ": "powershell"} {
		if got := NormalizeInterpreter(in); got != want {
			t.Errorf("NormalizeInterpreter(%q) = %q, want %q", in, got, want)
		}
	}
}
