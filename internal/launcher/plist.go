package launcher

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
)

// LabelWorker is the launchd service label for the opt-in semantica
// worker agent. It is stable across versions because launchd
// identifies services by this string: changing it would orphan
// bootstrapped agents on user machines.
const LabelWorker = "sh.semantica.worker"

// PlistInput carries the per-install values that the worker plist
// embeds. Both fields are absolute paths resolved by the caller so
// the rendered plist never contains relative paths (launchd refuses
// to start a service with a relative ProgramArgument).
type PlistInput struct {
	// BinaryPath is the absolute path of the semantica executable
	// that launchd should invoke. The caller typically passes
	// os.Executable() or a user-specified override.
	BinaryPath string

	// LogPath is the absolute path that receives the worker's
	// stdout and stderr. Launchd opens this file in append mode;
	// callers are responsible for its parent directory existing
	// by the time a kickstart fires.
	LogPath string
}

// Validate returns an error when any required field on PlistInput
// is unset or violates the absolute-path contract documented on the
// struct fields. Called by RenderWorkerPlist before template
// expansion so bad input fails fast with a readable message rather
// than producing a plist that launchd later rejects with an opaque
// code.
//
// Absolute here means a POSIX-absolute path (leading "/"). That is
// what launchd accepts for ProgramArguments and log file paths on
// macOS, and the only shape this package produces. Using a plain
// prefix check rather than filepath.IsAbs keeps the rule identical
// regardless of the host OS the tests happen to run on.
func (in PlistInput) Validate() error {
	if in.BinaryPath == "" {
		return errors.New("launcher: PlistInput.BinaryPath is empty")
	}
	if !isPOSIXAbsolute(in.BinaryPath) {
		return fmt.Errorf(
			"launcher: PlistInput.BinaryPath must be absolute, got %q",
			in.BinaryPath,
		)
	}
	if in.LogPath == "" {
		return errors.New("launcher: PlistInput.LogPath is empty")
	}
	if !isPOSIXAbsolute(in.LogPath) {
		return fmt.Errorf(
			"launcher: PlistInput.LogPath must be absolute, got %q",
			in.LogPath,
		)
	}
	return nil
}

// isPOSIXAbsolute reports whether p is a POSIX-absolute path
// (leading "/"). Kept as a named helper rather than an inline
// HasPrefix so the intent reads clearly at the call site.
func isPOSIXAbsolute(p string) bool {
	return strings.HasPrefix(p, "/")
}

// workerPlistTemplate is the exact bytes-on-disk shape of the
// launchd plist, with XML-safe substitution points for the three
// variable fields. Keeping the template inline and hand-formatted
// avoids depending on a third-party plist encoder for a file that
// has three variables and a fixed structure.
//
// Design choices baked in:
//
//   - RunAtLoad is false. The agent must run only when explicitly
//     kickstarted by the post-commit hook, not on login.
//   - KeepAlive is intentionally absent. The agent is short-lived
//     and exits after draining its queue; launchd should not
//     resurrect it.
//   - StandardOutPath and StandardErrorPath point at the same file
//     so the worker's stderr (which carries the wlog output) and
//     any accidental stdout land in one log the user can tail.
const workerPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>worker</string>
    <string>drain</string>
  </array>
  <key>RunAtLoad</key>
  <false/>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`

// RenderWorkerPlist returns the XML bytes of the launchd plist for
// the semantica worker agent. String inputs are XML-escaped so a
// home directory with ampersands, angle brackets, or quotes cannot
// produce a malformed plist.
//
// A plist is produced only when PlistInput passes Validate; any
// validation error is returned unchanged so the caller can surface
// it to the user.
func RenderWorkerPlist(in PlistInput) (string, error) {
	if err := in.Validate(); err != nil {
		return "", err
	}
	return fmt.Sprintf(
		workerPlistTemplate,
		xmlEscape(LabelWorker),
		xmlEscape(in.BinaryPath),
		xmlEscape(in.LogPath),
		xmlEscape(in.LogPath),
	), nil
}

// xmlEscape returns s with XML-reserved characters replaced by
// their entity references (for example, & -> &amp;). Defined here
// as a thin wrapper over encoding/xml.EscapeText so the plist
// renderer does not spread that boilerplate into every caller.
func xmlEscape(s string) string {
	var buf bytes.Buffer
	// EscapeText only fails if the underlying writer fails, and
	// bytes.Buffer.Write never returns an error, so the error
	// return here is unreachable in practice. Still, not ignoring
	// it keeps the linter and future readers happy.
	if err := xml.EscapeText(&buf, []byte(s)); err != nil {
		return s
	}
	return buf.String()
}
