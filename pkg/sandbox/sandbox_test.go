package sandbox

import (
	"strings"
	"testing"
	"time"

	"github.com/colinrgodsey/goREgo/pkg/config"
)

func TestNew_DisabledReturnsNil(t *testing.T) {
	cfg := config.SandboxConfig{Enabled: false}
	s, err := New(cfg)
	if err != nil {
		t.Errorf("expected no error for disabled sandbox, got %v", err)
	}
	if s != nil {
		t.Error("expected nil sandbox when disabled")
	}
}

func TestNew_EnabledBinaryNotFound(t *testing.T) {
	cfg := config.SandboxConfig{
		Enabled:    true,
		BinaryPath: "/nonexistent/linux-sandbox",
	}
	s, err := New(cfg)
	if err == nil {
		t.Error("expected error when binary not found")
	}
	if s != nil {
		t.Error("expected nil sandbox when binary not found")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error message, got: %v", err)
	}
}

func TestSandbox_Enabled(t *testing.T) {
	tests := []struct {
		name     string
		sandbox  *Sandbox
		expected bool
	}{
		{
			name:     "nil sandbox",
			sandbox:  nil,
			expected: false,
		},
		{
			name: "disabled sandbox",
			sandbox: &Sandbox{
				cfg: config.SandboxConfig{Enabled: false},
			},
			expected: false,
		},
		{
			name: "enabled sandbox",
			sandbox: &Sandbox{
				cfg: config.SandboxConfig{Enabled: true},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.sandbox.Enabled()
			if result != tt.expected {
				t.Errorf("Enabled() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestWrapCommand(t *testing.T) {
	tests := []struct {
		name            string
		cfg             config.SandboxConfig
		spec            WrapSpec
		wantContains    []string
		wantNotContains []string
		wantErr         bool
	}{
		{
			name: "basic with network isolation",
			cfg: config.SandboxConfig{
				Enabled:          true,
				BinaryPath:       "/usr/bin/linux-sandbox",
				NetworkIsolation: true,
				KillDelay:        5,
			},
			spec: WrapSpec{
				ExecRoot: "/tmp/exec",
				Timeout:  10 * time.Minute,
				Command:  []string{"echo", "test"},
			},
			wantContains:    []string{"/usr/bin/linux-sandbox", "-W", "/tmp/exec", "-N", "-H", "-T", "600", "-t", "5", "--", "echo", "test"},
			wantNotContains: []string{},
		},
		{
			name: "without network isolation",
			cfg: config.SandboxConfig{
				Enabled:          true,
				BinaryPath:       "/usr/bin/linux-sandbox",
				NetworkIsolation: false,
			},
			spec: WrapSpec{
				ExecRoot: "/tmp/exec",
				Command:  []string{"echo", "test"},
			},
			wantContains:    []string{"-W", "/tmp/exec", "-H", "--"},
			wantNotContains: []string{"-N"},
		},
		{
			name: "with input mounts",
			cfg: config.SandboxConfig{
				Enabled:          true,
				BinaryPath:       "/usr/bin/linux-sandbox",
				NetworkIsolation: false,
			},
			spec: WrapSpec{
				ExecRoot: "/tmp/exec",
				InputMounts: map[string]string{
					"/tmp/inputs/foo.txt": "/tmp/exec/foo.txt",
					"/tmp/inputs/bar.txt": "/tmp/exec/bar.txt",
				},
				Command: []string{"cat", "foo.txt"},
			},
			wantContains: []string{
				"-M", "/tmp/inputs/bar.txt", "-m", "/tmp/exec/bar.txt",
				"-M", "/tmp/inputs/foo.txt", "-m", "/tmp/exec/foo.txt",
			},
		},
		{
			name: "with writable paths",
			cfg: config.SandboxConfig{
				Enabled:          true,
				BinaryPath:       "/usr/bin/linux-sandbox",
				NetworkIsolation: false,
				WritablePaths:    []string{"/tmp"},
			},
			spec: WrapSpec{
				ExecRoot:      "/tmp/exec",
				WritablePaths: []string{"/tmp/exec/output", "/tmp/exec/bin"},
				Command:       []string{"gcc", "-o", "output/a.out"},
			},
			wantContains: []string{"-w", "/tmp", "-w", "/tmp/exec/bin", "-w", "/tmp/exec/output"},
		},
		{
			name: "with debug enabled",
			cfg: config.SandboxConfig{
				Enabled:          true,
				BinaryPath:       "/usr/bin/linux-sandbox",
				NetworkIsolation: false,
				Debug:            true,
			},
			spec: WrapSpec{
				ExecRoot: "/tmp/exec",
				Command:  []string{"echo", "test"},
			},
			wantContains: []string{"-D"},
		},
		{
			name: "empty command returns error",
			cfg: config.SandboxConfig{
				Enabled:    true,
				BinaryPath: "/usr/bin/linux-sandbox",
			},
			spec: WrapSpec{
				ExecRoot: "/tmp/exec",
				Command:  []string{},
			},
			wantErr: true,
		},
		{
			name: "no timeout omits -T flag",
			cfg: config.SandboxConfig{
				Enabled:          true,
				BinaryPath:       "/usr/bin/linux-sandbox",
				NetworkIsolation: false,
			},
			spec: WrapSpec{
				ExecRoot: "/tmp/exec",
				Timeout:  0,
				Command:  []string{"echo", "test"},
			},
			wantNotContains: []string{"-T"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Sandbox{cfg: tt.cfg}
			args, err := s.WrapCommand(tt.spec)

			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("WrapCommand failed: %v", err)
			}

			argsStr := strings.Join(args, " ")

			for _, want := range tt.wantContains {
				if !strings.Contains(argsStr, want) {
					t.Errorf("expected args to contain %q, got: %s", want, argsStr)
				}
			}

			for _, notWant := range tt.wantNotContains {
				if strings.Contains(argsStr, notWant) {
					t.Errorf("expected args NOT to contain %q, got: %s", notWant, argsStr)
				}
			}
		})
	}
}

func TestWrapCommand_ArgumentOrder(t *testing.T) {
	cfg := config.SandboxConfig{
		Enabled:          true,
		BinaryPath:       "/usr/bin/linux-sandbox",
		NetworkIsolation: true,
		KillDelay:        5,
	}
	s := &Sandbox{cfg: cfg}

	spec := WrapSpec{
		ExecRoot: "/tmp/exec",
		Timeout:  10 * time.Minute,
		Command:  []string{"/bin/bash", "-c", "echo hello"},
	}

	args, err := s.WrapCommand(spec)
	if err != nil {
		t.Fatalf("WrapCommand failed: %v", err)
	}

	// First argument should be the binary
	if args[0] != "/usr/bin/linux-sandbox" {
		t.Errorf("expected first arg to be sandbox binary, got %s", args[0])
	}

	// Find the -- delimiter
	delimIdx := -1
	for i, arg := range args {
		if arg == "--" {
			delimIdx = i
			break
		}
	}

	if delimIdx == -1 {
		t.Fatal("expected -- delimiter in args")
	}

	// Command should come after --
	commandArgs := args[delimIdx+1:]
	if len(commandArgs) != 3 {
		t.Errorf("expected 3 command args after --, got %d: %v", len(commandArgs), commandArgs)
	}

	if commandArgs[0] != "/bin/bash" {
		t.Errorf("expected command to be /bin/bash, got %s", commandArgs[0])
	}
}

func TestDeduplicatePaths(t *testing.T) {
	tests := []struct {
		name   string
		input  []string
		expect int
	}{
		{
			name:   "no duplicates",
			input:  []string{"/a", "/b", "/c"},
			expect: 3,
		},
		{
			name:   "with duplicates",
			input:  []string{"/a", "/b", "/a", "/c", "/b"},
			expect: 3,
		},
		{
			name:   "empty",
			input:  []string{},
			expect: 0,
		},
		{
			name:   "all same",
			input:  []string{"/a", "/a", "/a"},
			expect: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := deduplicatePaths(tt.input)
			if len(result) != tt.expect {
				t.Errorf("deduplicatePaths(%v) = %d items, want %d", tt.input, len(result), tt.expect)
			}
		})
	}
}
