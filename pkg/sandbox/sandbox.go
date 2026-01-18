package sandbox

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/colinrgodsey/goREgo/pkg/config"
)

// Sandbox wraps command execution in linux-sandbox for hermetic isolation.
type Sandbox struct {
	cfg    config.SandboxConfig
	logger *slog.Logger
}

// New creates a new Sandbox from configuration.
// Returns nil if sandboxing is disabled.
// Returns error if enabled but the binary is not found.
func New(cfg config.SandboxConfig) (*Sandbox, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	// Validate binary exists
	if _, err := os.Stat(cfg.BinaryPath); err != nil {
		return nil, fmt.Errorf("linux-sandbox binary not found at %s: %w", cfg.BinaryPath, err)
	}

	return &Sandbox{
		cfg:    cfg,
		logger: slog.Default().With("component", "sandbox"),
	}, nil
}

// WrapSpec contains the specification for wrapping a command in the sandbox.
type WrapSpec struct {
	// ExecRoot is the working directory (-W flag). This directory is writable.
	ExecRoot string

	// InputMounts maps source paths to target paths for bind mounting.
	// These are mounted read-only inside the sandbox.
	InputMounts map[string]string

	// WritablePaths are additional paths that should be writable inside the sandbox.
	// These are typically output directories.
	WritablePaths []string

	// Timeout is the maximum duration for command execution.
	Timeout time.Duration

	// Command is the actual command and arguments to execute.
	Command []string
}

// WrapCommand returns the linux-sandbox wrapped command arguments.
func (s *Sandbox) WrapCommand(spec WrapSpec) ([]string, error) {
	if len(spec.Command) == 0 {
		return nil, fmt.Errorf("command cannot be empty")
	}

	args := []string{s.cfg.BinaryPath}

	// Working directory (writable)
	args = append(args, "-W", spec.ExecRoot)

	// Bind mounts for inputs (read-only by default in linux-sandbox)
	// Sort keys for deterministic output (important for testing)
	sources := make([]string, 0, len(spec.InputMounts))
	for source := range spec.InputMounts {
		sources = append(sources, source)
	}
	sort.Strings(sources)

	for _, source := range sources {
		target := spec.InputMounts[source]
		args = append(args, "-M", source, "-m", target)
	}

	// Additional writable paths from config (e.g., /tmp)
	for _, p := range s.cfg.WritablePaths {
		args = append(args, "-w", p)
	}

	// Writable paths from spec (output directories)
	// Deduplicate and sort for deterministic output
	writablePaths := deduplicatePaths(spec.WritablePaths)
	sort.Strings(writablePaths)
	for _, p := range writablePaths {
		args = append(args, "-w", p)
	}

	// Network isolation
	if s.cfg.NetworkIsolation {
		args = append(args, "-N")
	}

	// Set hostname to 'localhost'
	args = append(args, "-H")

	// Timeout
	if spec.Timeout > 0 {
		args = append(args, "-T", fmt.Sprintf("%d", int(spec.Timeout.Seconds())))
		args = append(args, "-t", fmt.Sprintf("%d", s.cfg.KillDelay))
	}

	// Debug output
	if s.cfg.Debug {
		args = append(args, "-D")
	}

	// Delimiter before actual command
	args = append(args, "--")

	// Actual command
	args = append(args, spec.Command...)

	return args, nil
}

// Enabled returns true if sandboxing is configured and active.
func (s *Sandbox) Enabled() bool {
	return s != nil && s.cfg.Enabled
}

// deduplicatePaths removes duplicate paths from a slice.
func deduplicatePaths(paths []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(paths))
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			result = append(result, p)
		}
	}
	return result
}
