package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	defaultCaptureLimit = 4 << 20
	errorStderrLimit    = 2 << 10
)

// Runner executes a media command. The first argument is the executable path;
// subsequent arguments are passed directly to that executable without a shell.
type Runner interface {
	Run(context.Context, ...string) (stdout, stderr string, err error)
}

// ExecRunner executes commands with os/exec. Timeout and MaxCaptureBytes are
// optional; a zero capture limit uses a 4 MiB limit for each output stream.
type ExecRunner struct {
	Timeout          time.Duration
	MaxCaptureBytes  int
	RejectTruncation bool
}

// CommandError classifies an external command failure while retaining a
// bounded stderr excerpt for diagnostics.
type CommandError struct {
	Program  string
	Stderr   string
	ExitCode int
	Err      error
}

func (e *CommandError) Error() string {
	if e == nil {
		return "media command failed"
	}
	message := fmt.Sprintf("media command %q failed: %v", e.Program, e.Err)
	if detail := strings.TrimSpace(sanitizeCommandText(e.Stderr)); detail != "" {
		if len(detail) > errorStderrLimit {
			detail = detail[:errorStderrLimit] + "..."
		}
		message += ": " + detail
	}
	return message
}

func (e *CommandError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Run executes args directly, with bounded stdout/stderr capture. Context
// cancellation and Timeout are preserved through errors.Is.
func (r ExecRunner) Run(ctx context.Context, args ...string) (string, string, error) {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return "", "", errors.New("media command executable is required")
	}

	commandCtx := ctx
	cancel := func() {}
	if r.Timeout > 0 {
		commandCtx, cancel = context.WithTimeout(ctx, r.Timeout)
	}
	defer cancel()

	limit := r.MaxCaptureBytes
	if limit <= 0 {
		limit = defaultCaptureLimit
	}
	stdout := &boundedBuffer{remaining: limit}
	stderr := &boundedBuffer{remaining: limit}

	cmd := exec.CommandContext(commandCtx, args[0], args[1:]...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if commandCtx.Err() != nil {
		err = commandCtx.Err()
	}
	if err == nil && r.RejectTruncation && (stdout.overflow || stderr.overflow) {
		err = errors.New("media command capture truncated")
	}
	if err == nil {
		return stdout.String(), stderr.String(), nil
	}

	exitCode := -1
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		exitCode = exitError.ExitCode()
	}
	return stdout.String(), stderr.String(), &CommandError{
		Program:  args[0],
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		Err:      err,
	}
}

var credentialTextPattern = regexp.MustCompile(`(?i)((?:authorization|api[_-]?key|access[_-]?token|secret|token)\s*[:=]\s*(?:bearer\s+)?)[^\s,;]+`)

func sanitizeCommandText(text string) string {
	text = credentialTextPattern.ReplaceAllString(text, `${1}[REDACTED]`)
	for _, entry := range os.Environ() {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 || len(parts[1]) < 4 {
			continue
		}
		name := strings.ToUpper(parts[0])
		if strings.Contains(name, "KEY") || strings.Contains(name, "TOKEN") || strings.Contains(name, "SECRET") || strings.Contains(name, "CREDENTIAL") {
			text = strings.ReplaceAll(text, parts[1], "[REDACTED]")
		}
	}
	return text
}

type boundedBuffer struct {
	buffer    bytes.Buffer
	remaining int
	overflow  bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	originalLength := len(p)
	if len(p) > b.remaining {
		b.overflow = true
	}
	if b.remaining > 0 {
		if len(p) > b.remaining {
			p = p[:b.remaining]
		}
		_, _ = b.buffer.Write(p)
		b.remaining -= len(p)
	}
	return originalLength, nil
}

func (b *boundedBuffer) String() string {
	return b.buffer.String()
}
