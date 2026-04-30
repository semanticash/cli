//go:build linux

package launcher

import (
	"errors"
	"fmt"
	"strings"
)

// unitInput holds the values embedded in the systemd unit file.
// Internal to the linux backend.
type unitInput struct {
	// BinaryPath is the absolute path to the semantica binary the
	// unit invokes via ExecStart.
	BinaryPath string

	// LogPath is the absolute path passed to the worker via
	// --log-file. The worker opens it itself; the unit file does
	// not use StandardOutput=append: because that directive
	// requires systemd 240+ (excludes Ubuntu 18.04 / systemd 237).
	LogPath string
}

// validate rejects empty or non-absolute paths. POSIX rules apply
// because the unit file is consumed by Linux's systemd.
func (in unitInput) validate() error {
	if in.BinaryPath == "" {
		return errors.New("launcher: unitInput.BinaryPath is empty")
	}
	if !isPOSIXAbsolute(in.BinaryPath) {
		return fmt.Errorf("launcher: unitInput.BinaryPath must be absolute, got %q", in.BinaryPath)
	}
	if in.LogPath == "" {
		return errors.New("launcher: unitInput.LogPath is empty")
	}
	if !isPOSIXAbsolute(in.LogPath) {
		return fmt.Errorf("launcher: unitInput.LogPath must be absolute, got %q", in.LogPath)
	}
	return nil
}

// workerUnitTemplate is the systemd user unit template.
//
// Type=oneshot with on-demand kicks: no [Install] section because
// the unit is started via `systemctl --user start` from the
// post-commit hook, not enabled for boot.
//
// ExecStart passes --log-file to the worker so the worker captures
// stdout/stderr itself. We deliberately avoid StandardOutput=append:
// in the unit because that directive requires systemd 240+, which
// excludes Ubuntu 18.04 (systemd 237) - the floor of the supported
// distro set.
//
// Each ExecStart argument is wrapped with systemdQuote so paths
// containing whitespace or systemd-special characters round-trip
// safely. The substitution slots receive already-quoted strings;
// do not add bare quote characters to the template.
const workerUnitTemplate = `[Unit]
Description=Semantica worker drain
After=default.target

[Service]
Type=oneshot
ExecStart=%s worker drain %s
`

// renderWorkerUnit renders the systemd unit file body.
func renderWorkerUnit(in unitInput) (string, error) {
	if err := in.validate(); err != nil {
		return "", err
	}
	return fmt.Sprintf(
		workerUnitTemplate,
		systemdQuote(in.BinaryPath),
		systemdQuote("--log-file="+in.LogPath),
	), nil
}

// systemdQuote wraps s in double quotes and escapes the characters
// that systemd treats specially in ExecStart arguments: `"`, `\`,
// `%`, and `$`.
//
// Quoting is required here. systemd parses ExecStart as a command
// line, so unquoted paths with spaces or tabs would be split into
// multiple argv elements.
func systemdQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '%':
			b.WriteString("%%")
		case '$':
			b.WriteString("$$")
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
