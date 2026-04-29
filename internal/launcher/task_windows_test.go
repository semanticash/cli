//go:build windows

package launcher

import (
	"strings"
	"testing"
)

func TestRenderWorkerTask_ContainsRequiredElements(t *testing.T) {
	got, err := renderWorkerTask(taskInput{
		BinaryPath:       `C:\Program Files\Semantica\semantica.exe`,
		LogPath:          `C:\Users\Test\.semantica\worker-launcher.log`,
		WorkingDirectory: `C:\Program Files\Semantica`,
	})
	if err != nil {
		t.Fatalf("renderWorkerTask: %v", err)
	}

	// Required outer structure.
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-16"?>`,
		`<Task version="1.2"`,
		`<Triggers/>`,
		`<Actions Context="Author">`,
		`<Principals>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("XML missing %q; got:\n%s", want, got)
		}
	}

	// Settings that pin behavior the design depends on.
	for _, want := range []string{
		`<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>`,
		`<AllowStartOnDemand>true</AllowStartOnDemand>`,
		`<Enabled>true</Enabled>`,
		`<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("XML missing required setting %q; got:\n%s", want, got)
		}
	}

	// Per-user, no-elevation principal. UAC concerns require these
	// exact values: changing LogonType or RunLevel would prompt for
	// elevation and pin the wrong security context.
	for _, want := range []string{
		`<LogonType>InteractiveToken</LogonType>`,
		`<RunLevel>LeastPrivilege</RunLevel>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("principal element wrong; got:\n%s\nwant substring %q", got, want)
		}
	}

	// Action substitution. Paths and arguments must round-trip
	// XML-escaped through the template.
	for _, want := range []string{
		`<Command>C:\Program Files\Semantica\semantica.exe</Command>`,
		`<Arguments>worker drain --log-file=C:\Users\Test\.semantica\worker-launcher.log</Arguments>`,
		`<WorkingDirectory>C:\Program Files\Semantica</WorkingDirectory>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("action element wrong; got:\n%s\nwant substring %q", got, want)
		}
	}
}

// Paths legally can contain spaces (Program Files, Test User home
// directories, arbitrary install locations). Task Scheduler passes
// <Arguments> as a Windows command line, so an unquoted spaced path
// would be split before Cobra's argv parsing sees it. The
// --log-file argument must be quoted via windowsCmdQuote so the
// value lands as one argv element. After XML escape, the literal
// `"` characters appear as `&#34;` in the on-disk file.
func TestRenderWorkerTask_HandlesSpacedLogPath(t *testing.T) {
	got, err := renderWorkerTask(taskInput{
		BinaryPath:       `C:\bin\semantica.exe`,
		LogPath:          `C:\Users\Test User\.semantica\worker-launcher.log`,
		WorkingDirectory: `C:\bin`,
	})
	if err != nil {
		t.Fatalf("renderWorkerTask: %v", err)
	}
	want := `<Arguments>worker drain &#34;--log-file=C:\Users\Test User\.semantica\worker-launcher.log&#34;</Arguments>`
	if !strings.Contains(got, want) {
		t.Errorf("spaced log path not properly quoted in <Arguments>; got:\n%s\nwant substring:\n%s", got, want)
	}
}

// windowsCmdQuote table-test: each row pins one CommandLineToArgvW
// quoting rule. A regression on any single rule surfaces clearly.
func TestWindowsCmdQuote(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no special chars passes bare", "worker", "worker"},
		{"empty string becomes empty quoted", "", `""`},
		{"space triggers quoting", "with space", `"with space"`},
		{"tab triggers quoting", "with\ttab", "\"with\ttab\""},
		{"plain path passes bare", `C:\bin\semantica.exe`, `C:\bin\semantica.exe`},
		{"spaced path quoted", `C:\Program Files\foo`, `"C:\Program Files\foo"`},
		{"embedded quote escaped", `say "hi"`, `"say \"hi\""`},
		{"backslash run before quote doubles", `\\` + `"`, `"\\\\\""`},
		{"trailing backslash in quoted string doubles", `path\`, `path\`},
		{"trailing backslash in quoted spaced string doubles", `with space\`, `"with space\\"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := windowsCmdQuote(tc.in)
			if got != tc.want {
				t.Errorf("windowsCmdQuote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The task is on-demand only. UAC-elevated principals (Administrators,
// SYSTEM) and boot triggers would change the security context or run
// schedule and break the design. Pin both negatives.
func TestRenderWorkerTask_RejectsElevationAndScheduleHints(t *testing.T) {
	got, err := renderWorkerTask(taskInput{
		BinaryPath:       `C:\bin\semantica.exe`,
		LogPath:          `C:\log\worker.log`,
		WorkingDirectory: `C:\bin`,
	})
	if err != nil {
		t.Fatalf("renderWorkerTask: %v", err)
	}
	for _, mustNot := range []string{
		"HighestAvailable",
		"Administrators",
		"S-1-5-18", // SYSTEM SID
		"<BootTrigger",
		"<TimeTrigger",
		"<CalendarTrigger",
		"<LogonTrigger",
	} {
		if strings.Contains(got, mustNot) {
			t.Errorf("task XML must not contain %q (would change security context or run schedule); got:\n%s", mustNot, got)
		}
	}
}

// Paths legally can contain XML-special characters via percent-
// escaped components or unusual install locations. The template
// escapes Command/Arguments/WorkingDirectory via xml.EscapeText so
// such inputs round-trip without breaking the document.
func TestRenderWorkerTask_EscapesXMLSpecialCharacters(t *testing.T) {
	got, err := renderWorkerTask(taskInput{
		BinaryPath:       `C:\foo & bar\semantica.exe`,
		LogPath:          `C:\path<weird>"quoted"\log`,
		WorkingDirectory: `C:\foo & bar`,
	})
	if err != nil {
		t.Fatalf("renderWorkerTask: %v", err)
	}
	// Ampersand must be escaped to keep the XML well-formed.
	if !strings.Contains(got, `C:\foo &amp; bar\semantica.exe`) {
		t.Errorf("ampersand not XML-escaped in Command; got:\n%s", got)
	}
	// Angle brackets and quotes must be escaped in arguments.
	if !strings.Contains(got, `&lt;weird&gt;`) {
		t.Errorf("angle brackets not XML-escaped in Arguments; got:\n%s", got)
	}
	if !strings.Contains(got, `&#34;quoted&#34;`) {
		t.Errorf("quotes not XML-escaped in Arguments; got:\n%s", got)
	}
}

func TestRenderWorkerTask_ValidatesRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		in   taskInput
	}{
		{
			"empty binary path",
			taskInput{BinaryPath: "", LogPath: `C:\log`, WorkingDirectory: `C:\bin`},
		},
		{
			"empty log path",
			taskInput{BinaryPath: `C:\bin\semantica.exe`, LogPath: "", WorkingDirectory: `C:\bin`},
		},
		{
			"empty working directory",
			taskInput{BinaryPath: `C:\bin\semantica.exe`, LogPath: `C:\log`, WorkingDirectory: ""},
		},
		{
			"relative binary path",
			taskInput{BinaryPath: `bin\semantica.exe`, LogPath: `C:\log`, WorkingDirectory: `C:\bin`},
		},
		{
			"relative log path",
			taskInput{BinaryPath: `C:\bin\semantica.exe`, LogPath: `log\worker.log`, WorkingDirectory: `C:\bin`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := renderWorkerTask(tc.in)
			if err == nil {
				t.Errorf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

// schtasks /Create /XML expects UTF-16 LE with a BOM. UTF-8 input
// is silently mangled or rejected on some Windows versions, so the
// encoding step is load-bearing. Pin the BOM and the byte ordering.
func TestEncodeUTF16LE_HasBOMAndLittleEndianOrder(t *testing.T) {
	got := encodeUTF16LE("AB")

	// BOM: 0xFF 0xFE.
	if len(got) < 2 || got[0] != 0xFF || got[1] != 0xFE {
		head := len(got)
		if head > 4 {
			head = 4
		}
		t.Fatalf("expected UTF-16 LE BOM 0xFF 0xFE prefix, got % x", got[:head])
	}
	// 'A' (U+0041) → 0x41 0x00, 'B' (U+0042) → 0x42 0x00 in little-endian.
	want := []byte{0xFF, 0xFE, 0x41, 0x00, 0x42, 0x00}
	if len(got) != len(want) {
		t.Fatalf("encoded length = %d, want %d (% x)", len(got), len(want), got)
	}
	for i, b := range want {
		if got[i] != b {
			t.Errorf("byte %d = 0x%02x, want 0x%02x; full bytes: % x", i, got[i], b, got)
		}
	}
}

func TestEncodeUTF16LE_HandlesSurrogatePair(t *testing.T) {
	// U+1F600 (😀) is outside the BMP and encodes as a surrogate
	// pair: high D83D, low DE00. Each goes out as two LE bytes.
	got := encodeUTF16LE("\U0001F600")
	want := []byte{0xFF, 0xFE, 0x3D, 0xD8, 0x00, 0xDE}
	if len(got) != len(want) {
		t.Fatalf("encoded length = %d, want %d (% x)", len(got), len(want), got)
	}
	for i, b := range want {
		if got[i] != b {
			t.Errorf("byte %d = 0x%02x, want 0x%02x", i, got[i], b)
		}
	}
}

func TestResolveWorkingDirectory(t *testing.T) {
	cases := []struct {
		name       string
		binaryPath string
		globalBase string
		want       string
	}{
		{
			name:       "uses parent of binary",
			binaryPath: `C:\Program Files\Semantica\semantica.exe`,
			globalBase: `C:\Users\test\.semantica`,
			want:       `C:\Program Files\Semantica`,
		},
		{
			name:       "falls back to global base when binary has no useful parent",
			binaryPath: `semantica.exe`,
			globalBase: `C:\Users\test\.semantica`,
			want:       `C:\Users\test\.semantica`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveWorkingDirectory(tc.binaryPath, tc.globalBase)
			if got != tc.want {
				t.Errorf("resolveWorkingDirectory(%q, %q) = %q, want %q",
					tc.binaryPath, tc.globalBase, got, tc.want)
			}
		})
	}
}

