package agentbuild

import (
	"testing"

	"github.com/spf13/pflag"
)

// RegisterFlags binds every option; a full flag set parses and lands in the
// struct.
func TestRegisterFlagsBindsAllOptions(t *testing.T) {
	var o Options
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterFlags(fs, &o)
	args := []string{
		"-p", "hi", "--model", "m", "--provider", "openai", "--api-key", "k",
		"--base-url", "http://x", "--max-tokens", "100", "--thinking-budget", "5",
		"--output-format", "text", "--temperature", "0.3", "--permission-mode", "bypassPermissions",
		"--dangerously-skip-permissions", "--accept-edits", "--read-only", "--plan", "--workdir", "/tmp",
		"--system-prompt", "x",
		"--managed-settings", "ms.json", "--resume", "s", "--log-level", "debug", "--auto-compact", "10",
		"--fallback-model", "f", "--search-api", "brave", "--search-api-key", "sk",
		"--checkpoint-dir", "/tmp/cp", "--resume-checkpoint", "th", "--resume-value", "y",
		"--db", "none", "--max-turns", "3", "--debug-log",
		"--allow-command", "ls", "--deny-command", "rm", "--mcp", "srv=cmd",
	}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if o.Prompt != "hi" || o.Model != "m" || o.MaxTurns != 3 || !o.DebugLog || o.ExtraPrompt != "x" || o.ManagedSettings != "ms.json" || len(o.AllowCommands) != 1 || len(o.MCPServers) != 1 {
		t.Fatalf("options = %+v", o)
	}
}
