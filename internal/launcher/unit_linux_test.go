//go:build linux

package launcher

import (
	"strings"
	"testing"
)

func TestRenderWorkerUnit_ContainsRequiredKeysAndInvocation(t *testing.T) {
	got, err := renderWorkerUnit(unitInput{
		BinaryPath: "/usr/local/bin/semantica",
		LogPath:    "/home/test/.semantica/worker-launcher.log",
	})
	if err != nil {
		t.Fatalf("renderWorkerUnit: %v", err)
	}

	// Required sections.
	for _, want := range []string{"[Unit]", "[Service]"} {
		if !strings.Contains(got, want) {
			t.Errorf("unit missing %q section; got:\n%s", want, got)
		}
	}

	// Type=oneshot per the design - on-demand kicks, no [Install].
	if !strings.Contains(got, "Type=oneshot") {
		t.Errorf("unit must declare Type=oneshot; got:\n%s", got)
	}

	// ExecStart must include the binary path, the worker subcommand
	// chain, and the --log-file flag with the configured path. Both
	// argv elements are double-quoted so paths with whitespace or
	// systemd-special characters round-trip safely.
	want := `ExecStart="/usr/local/bin/semantica" worker drain "--log-file=/home/test/.semantica/worker-launcher.log"`
	if !strings.Contains(got, want) {
		t.Errorf("ExecStart line missing or wrong; got:\n%s\nwant substring:\n%s", got, want)
	}
}

// Paths legally can contain spaces (macOS-style "User Name" home
// directories, Windows-style "Program Files", arbitrary install
// locations). systemd parses ExecStart as a command line, splitting
// on whitespace; without quoting, a spaced path would be broken into
// the wrong argv. The quote wrapper must keep both the binary and
// the log argument as single argv elements.
func TestRenderWorkerUnit_HandlesPathsWithSpaces(t *testing.T) {
	got, err := renderWorkerUnit(unitInput{
		BinaryPath: "/Users/Test User/bin/semantica",
		LogPath:    "/Users/Test User/.semantica/worker-launcher.log",
	})
	if err != nil {
		t.Fatalf("renderWorkerUnit: %v", err)
	}
	want := `ExecStart="/Users/Test User/bin/semantica" worker drain "--log-file=/Users/Test User/.semantica/worker-launcher.log"`
	if !strings.Contains(got, want) {
		t.Errorf("paths-with-spaces ExecStart broken; got:\n%s\nwant substring:\n%s", got, want)
	}
}

// The unit must NOT declare an [Install] section - that would
// require systemctl --user enable to autostart at boot, which is
// not the design. The unit is on-demand only.
func TestRenderWorkerUnit_OmitsInstallSection(t *testing.T) {
	got, err := renderWorkerUnit(unitInput{
		BinaryPath: "/usr/local/bin/semantica",
		LogPath:    "/tmp/log",
	})
	if err != nil {
		t.Fatalf("renderWorkerUnit: %v", err)
	}
	if strings.Contains(got, "[Install]") {
		t.Errorf("unit must omit [Install]; got:\n%s", got)
	}
	if strings.Contains(got, "WantedBy=") {
		t.Errorf("unit must omit WantedBy directives; got:\n%s", got)
	}
}

// The unit must NOT use StandardOutput=append: which requires
// systemd 240+ and would fail to parse on the supported distro
// floor (Ubuntu 18.04 ships systemd 237). The worker captures its
// own output via --log-file instead.
func TestRenderWorkerUnit_DoesNotUseAppendStandardOutput(t *testing.T) {
	got, err := renderWorkerUnit(unitInput{
		BinaryPath: "/usr/local/bin/semantica",
		LogPath:    "/tmp/log",
	})
	if err != nil {
		t.Fatalf("renderWorkerUnit: %v", err)
	}
	if strings.Contains(got, "StandardOutput=append:") {
		t.Errorf("unit must not use append: directive (systemd 240+); got:\n%s", got)
	}
	if strings.Contains(got, "StandardError=append:") {
		t.Errorf("unit must not use append: directive (systemd 240+); got:\n%s", got)
	}
}

func TestRenderWorkerUnit_ValidatesRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		in   unitInput
	}{
		{"empty binary path", unitInput{BinaryPath: "", LogPath: "/tmp/log"}},
		{"empty log path", unitInput{BinaryPath: "/usr/bin/semantica", LogPath: ""}},
		{"both empty", unitInput{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := renderWorkerUnit(tc.in)
			if err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

func TestRenderWorkerUnit_RejectsRelativePaths(t *testing.T) {
	cases := []struct {
		name string
		in   unitInput
	}{
		{
			name: "relative binary path with dot prefix",
			in:   unitInput{BinaryPath: "./semantica", LogPath: "/tmp/log"},
		},
		{
			name: "bare-name binary path",
			in:   unitInput{BinaryPath: "semantica", LogPath: "/tmp/log"},
		},
		{
			name: "relative log path",
			in:   unitInput{BinaryPath: "/usr/bin/semantica", LogPath: "log.txt"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := renderWorkerUnit(tc.in)
			if err == nil {
				t.Errorf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

// systemdQuote wraps the input in double quotes and escapes the
// four characters systemd treats specially inside ExecStart
// arguments: `"`, `\`, `%`, `$`. The table pins each escape so a
// regression on any single character surfaces clearly.
func TestSystemdQuote(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain path", "/usr/local/bin/semantica", `"/usr/local/bin/semantica"`},
		{"path with space", "/Users/Test User/bin/semantica", `"/Users/Test User/bin/semantica"`},
		{"path with tab", "/path/with\ttab", "\"/path/with\ttab\""},
		{"percent in path", "/foo/%bar", `"/foo/%%bar"`},
		{"dollar in path", "/foo/$bar", `"/foo/$$bar"`},
		{"backslash in path", `/foo/\bar`, `"/foo/\\bar"`},
		{"double quote in path", `/foo/"bar`, `"/foo/\"bar"`},
		{"all four specials together", `/foo/%$"\bar`, `"/foo/%%$$\"\\bar"`},
		{"empty string", "", `""`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := systemdQuote(tc.in)
			if got != tc.want {
				t.Errorf("systemdQuote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
