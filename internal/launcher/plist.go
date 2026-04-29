//go:build darwin

package launcher

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
)

// plistInput holds the values embedded in the worker plist.
// Internal to the darwin backend; the public Install API does not
// expose plist semantics.
type plistInput struct {
	// BinaryPath is the absolute path launchd should execute.
	BinaryPath string

	// LogPath receives stdout and stderr.
	LogPath string
}

// validate rejects empty or non-absolute paths. Paths use POSIX
// rules because the plist is macOS-only.
func (in plistInput) validate() error {
	if in.BinaryPath == "" {
		return errors.New("launcher: plistInput.BinaryPath is empty")
	}
	if !isPOSIXAbsolute(in.BinaryPath) {
		return fmt.Errorf(
			"launcher: plistInput.BinaryPath must be absolute, got %q",
			in.BinaryPath,
		)
	}
	if in.LogPath == "" {
		return errors.New("launcher: plistInput.LogPath is empty")
	}
	if !isPOSIXAbsolute(in.LogPath) {
		return fmt.Errorf(
			"launcher: plistInput.LogPath must be absolute, got %q",
			in.LogPath,
		)
	}
	return nil
}

// workerPlistTemplate is the launchd plist template. The service
// runs only when kickstarted and logs stdout and stderr to one file.
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

// renderWorkerPlist renders the worker plist body. Internal to the
// darwin backend.
func renderWorkerPlist(in plistInput) (string, error) {
	if err := in.validate(); err != nil {
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

// xmlEscape escapes XML text.
func xmlEscape(s string) string {
	var buf bytes.Buffer
	if err := xml.EscapeText(&buf, []byte(s)); err != nil {
		return s
	}
	return buf.String()
}
