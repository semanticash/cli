//go:build windows

package launcher

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// taskInput holds the values embedded in the Task Scheduler XML.
// Internal to the windows backend.
type taskInput struct {
	// BinaryPath is the absolute path to the semantica binary the
	// task invokes. Quoted internally as the task <Command>.
	BinaryPath string

	// LogPath is the absolute path the worker opens via --log-file.
	// Task Scheduler does not redirect stdout/stderr at the OS
	// level, so the worker captures its own output. The path is
	// embedded into <Arguments> as `--log-file=<LogPath>`.
	LogPath string

	// WorkingDirectory is the directory Task Scheduler sets as
	// CWD before executing the action. Defaults to the directory
	// containing the binary; falls back to broker.GlobalBase if
	// the binary lives somewhere unusual.
	WorkingDirectory string
}

// validate rejects empty or non-absolute paths. Path absoluteness
// is checked against Windows conventions via filepath.IsAbs, which
// accepts forms like `C:\foo`, `\\server\share\file`, and `C:/foo`.
func (in taskInput) validate() error {
	if in.BinaryPath == "" {
		return errors.New("launcher: taskInput.BinaryPath is empty")
	}
	if !filepath.IsAbs(in.BinaryPath) {
		return fmt.Errorf("launcher: taskInput.BinaryPath must be absolute, got %q", in.BinaryPath)
	}
	if in.LogPath == "" {
		return errors.New("launcher: taskInput.LogPath is empty")
	}
	if !filepath.IsAbs(in.LogPath) {
		return fmt.Errorf("launcher: taskInput.LogPath must be absolute, got %q", in.LogPath)
	}
	if in.WorkingDirectory == "" {
		return errors.New("launcher: taskInput.WorkingDirectory is empty")
	}
	if !filepath.IsAbs(in.WorkingDirectory) {
		return fmt.Errorf("launcher: taskInput.WorkingDirectory must be absolute, got %q", in.WorkingDirectory)
	}
	return nil
}

// workerTaskTemplate is the Task Scheduler XML template. The XML
// schema is documented at
// https://learn.microsoft.com/windows/win32/taskschd/task-scheduler-schema.
//
// Settings worth calling out:
//
//   - <Triggers/> is empty: the task is on-demand only, kicked by
//     the post-commit hook via `schtasks /Run`. No boot or schedule
//     trigger.
//   - <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>:
//     a second kick during an in-flight drain is dropped, matching
//     launchd's kickstart behavior (it never starts a second copy).
//   - <Principals><LogonType>InteractiveToken</LogonType><RunLevel>
//     LeastPrivilege</RunLevel></Principals>: per-user task in the
//     interactive session, no UAC elevation. Using SYSTEM or
//     HighestPrivilege would prompt for elevation and pin the
//     wrong security context for post-commit hooks.
//   - <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>: no automatic
//     timeout. The worker decides when to exit.
//
// XML substitution slots receive XML-escaped values. The wrapper
// (renderWorkerTask) handles the encoding via xml.EscapeText so
// callers do not need to escape inputs themselves.
const workerTaskTemplate = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Semantica worker drain</Description>
  </RegistrationInfo>
  <Triggers/>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>true</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%s</Command>
      <Arguments>%s</Arguments>
      <WorkingDirectory>%s</WorkingDirectory>
    </Exec>
  </Actions>
  <Principals>
    <Principal id="Author">
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
</Task>
`

// renderWorkerTask renders the Task Scheduler XML body as a UTF-8
// string. The encodeUTF16LE helper converts to UTF-16 LE with BOM
// before writing to disk; schtasks.exe /Create /XML expects
// Unicode-encoded files.
//
// The <Arguments> element value is a Windows command line, not a
// pre-split argv list: Task Scheduler concatenates Command and
// Arguments and hands the result to CreateProcess, which splits
// on whitespace per CommandLineToArgvW rules. Each argument is
// passed through windowsCmdQuote so values containing spaces
// (a log path under "Test User" or "Program Files") survive the
// split as a single argv element. Quoting is conditional - bare
// tokens like "worker" or "drain" pass through unmodified.
func renderWorkerTask(in taskInput) (string, error) {
	if err := in.validate(); err != nil {
		return "", err
	}
	args := strings.Join([]string{
		windowsCmdQuote("worker"),
		windowsCmdQuote("drain"),
		windowsCmdQuote("--log-file=" + in.LogPath),
	}, " ")
	return fmt.Sprintf(
		workerTaskTemplate,
		xmlEscape(in.BinaryPath),
		xmlEscape(args),
		xmlEscape(in.WorkingDirectory),
	), nil
}

// windowsCmdQuote quotes s for inclusion in a Windows command line
// per CommandLineToArgvW rules so the value survives CreateProcess's
// argv split as a single argument.
//
// Quoting is conditional: a string with no whitespace and no `"`
// is returned bare (no quoting needed, no quoting noise). Otherwise
// the value is wrapped in double quotes and escaped per the
// documented MSVCRT rules:
//
//   - Each `"` is escaped as `\"`.
//   - Each backslash run that immediately precedes a `"` (or the
//     closing quote) is doubled. This is the standard
//     CommandLineToArgvW handling of `\\\"` -> `\"`.
//   - Other backslashes pass through unchanged. Most paths fall
//     into this case (a single `\` between path components is
//     emitted verbatim).
//
// Reference: https://learn.microsoft.com/en-us/cpp/cpp/main-function-command-line-args
func windowsCmdQuote(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\n\v\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	backslashes := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			backslashes++
		case '"':
			// 2N backslashes followed by `"` means the backslashes
			// were literal but the quote terminates the run. To
			// preserve the literal `"` inside the quoted span we
			// need 2N+1 backslashes followed by the literal quote.
			b.WriteString(strings.Repeat(`\`, backslashes*2+1))
			b.WriteByte('"')
			backslashes = 0
		default:
			b.WriteString(strings.Repeat(`\`, backslashes))
			backslashes = 0
			b.WriteByte(c)
		}
	}
	// Trailing backslashes precede the closing quote; double them
	// so the last `\` stays literal rather than escaping the close.
	b.WriteString(strings.Repeat(`\`, backslashes*2))
	b.WriteByte('"')
	return b.String()
}

// xmlEscape escapes XML text so user-supplied paths embed safely
// into element content. Handles the five XML predefined entities
// (&, <, >, ", ').
func xmlEscape(s string) string {
	var buf bytes.Buffer
	if err := xml.EscapeText(&buf, []byte(s)); err != nil {
		return s
	}
	return buf.String()
}

// encodeUTF16LE encodes a UTF-8 string as UTF-16 LE with a BOM,
// the encoding schtasks.exe /XML expects. Surrogate pairs are
// handled by utf16.Encode.
func encodeUTF16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 2+2*len(units))
	out[0] = 0xFF
	out[1] = 0xFE
	for i, u := range units {
		binary.LittleEndian.PutUint16(out[2+2*i:], u)
	}
	return out
}

// resolveWorkingDirectory returns the directory the task should
// run from. Defaults to the binary's parent directory; falls back
// to broker.GlobalBase when the binary's parent is empty (defensive
// - should not happen with the absoluteness check, but guards
// against future input shapes).
func resolveWorkingDirectory(binaryPath, globalBase string) string {
	dir := filepath.Dir(binaryPath)
	if strings.TrimSpace(dir) == "" || dir == "." {
		return globalBase
	}
	return dir
}
