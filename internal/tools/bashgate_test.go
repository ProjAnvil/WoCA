package tools

import "testing"

func TestNewCommandPolicy(t *testing.T) {
	p := NewCommandPolicy(nil, nil)
	if len(p.Allowlist) != 0 || len(p.Denylist) != 0 {
		t.Fatalf("NewCommandPolicy(nil, nil) has non-empty slices: %#v", p)
	}
	if p.Allowlist == nil || p.Denylist == nil {
		t.Fatalf("NewCommandPolicy(nil, nil) should return non-nil empty slices: %#v", p)
	}
}

func TestCommandPolicyAllow(t *testing.T) {
	cases := []struct {
		name       string
		policy     CommandPolicy
		command    string
		wantOK     bool
		wantReason string
	}{
		// Empty policy allows everything.
		{
			name:    "empty policy allows all",
			policy:  NewCommandPolicy(nil, nil),
			command: "rm -rf /",
			wantOK:  true,
		},
		{
			name:    "empty policy allows empty",
			policy:  NewCommandPolicy(nil, nil),
			command: "",
			wantOK:  true,
		},

		// Allowlist prefix match.
		{
			name:    "allowlist prefix match",
			policy:  NewCommandPolicy([]string{"ls", "git", "go test"}, nil),
			command: "ls -la",
			wantOK:  true,
		},
		{
			name:    "allowlist multi word prefix",
			policy:  NewCommandPolicy([]string{"go test", "git"}, nil),
			command: "go test ./...",
			wantOK:  true,
		},
		{
			name:    "allowlist sudo wrapper",
			policy:  NewCommandPolicy([]string{"ls"}, nil),
			command: "sudo ls -la",
			wantOK:  true,
		},
		{
			name:    "allowlist env wrapper",
			policy:  NewCommandPolicy([]string{"ls"}, nil),
			command: "env FOO=bar ls -la",
			wantOK:  true,
		},
		{
			name:    "allowlist cd and chain",
			policy:  NewCommandPolicy([]string{"ls"}, nil),
			command: "cd /tmp && ls -la",
			wantOK:  true,
		},
		{
			name:    "allowlist cd semicolon chain",
			policy:  NewCommandPolicy([]string{"ls"}, nil),
			command: "cd /tmp; ls -la",
			wantOK:  true,
		},
		{
			name:    "allowlist stacked wrappers",
			policy:  NewCommandPolicy([]string{"ls"}, nil),
			command: "sudo env FOO=bar nice nohup timeout 5 ls -la",
			wantOK:  true,
		},

		// Allowlist reject.
		{
			name:       "allowlist reject",
			policy:     NewCommandPolicy([]string{"ls"}, nil),
			command:    "rm -rf /tmp/x",
			wantOK:     false,
			wantReason: "not in allowlist",
		},
		{
			name:       "allowlist reject wrapped",
			policy:     NewCommandPolicy([]string{"ls"}, nil),
			command:    "sudo rm -rf /tmp/x",
			wantOK:     false,
			wantReason: "not in allowlist",
		},
		{
			name:       "allowlist reject empty command",
			policy:     NewCommandPolicy([]string{"ls"}, nil),
			command:    "",
			wantOK:     false,
			wantReason: "not in allowlist",
		},

		// Denylist block (substring).
		{
			name:       "denylist substring",
			policy:     NewCommandPolicy(nil, []string{"rm -rf"}),
			command:    "rm -rf /tmp/x",
			wantOK:     false,
			wantReason: "blocked by denylist",
		},
		{
			name:       "denylist substring mid command",
			policy:     NewCommandPolicy(nil, []string{"password"}),
			command:    "echo mypasswordhere",
			wantOK:     false,
			wantReason: "blocked by denylist",
		},
		{
			name:       "denylist case insensitive",
			policy:     NewCommandPolicy(nil, []string{"DROP"}),
			command:    "drop table users",
			wantOK:     false,
			wantReason: "blocked by denylist",
		},

		// Denylist precedence over allowlist.
		{
			name:       "denylist precedence over allowlist",
			policy:     NewCommandPolicy([]string{"ls"}, []string{"sudo"}),
			command:    "sudo ls -la",
			wantOK:     false,
			wantReason: "blocked by denylist",
		},
		{
			name:       "denylist precedence same prefix",
			policy:     NewCommandPolicy([]string{"ls"}, []string{"ls -la"}),
			command:    "ls -la",
			wantOK:     false,
			wantReason: "blocked by denylist",
		},

		// Denylist only, non-matching commands allowed.
		{
			name:    "denylist non match allowed",
			policy:  NewCommandPolicy(nil, []string{"rm -rf"}),
			command: "ls -la",
			wantOK:  true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			gotOK, gotReason := c.policy.Allow(c.command)
			if gotOK != c.wantOK {
				t.Errorf("Allow(%q) ok = %v, want %v", c.command, gotOK, c.wantOK)
			}
			if gotReason != c.wantReason {
				t.Errorf("Allow(%q) reason = %q, want %q", c.command, gotReason, c.wantReason)
			}
		})
	}
}
