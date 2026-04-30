//go:build windows

package launcher

import (
	"strings"
	"testing"
)

// isTaskNotFoundError must match only the documented not-found
// wording. The previous broad fallback (`exit 1 && contains "error:"`)
// silently classified permission failures, scheduler-service
// outages, and other operational errors as "task not found",
// causing Status to report "not loaded" and Disable to swallow
// real delete failures. This table pins both sides of the contract:
// known not-found wording is matched, generic exit-1 ERROR: lines
// are not.
func TestIsTaskNotFoundError(t *testing.T) {
	cases := []struct {
		name     string
		exitCode int
		stderr   string
		want     bool
	}{
		// --- positive cases (real not-found wording) ---
		{
			name:     "system cannot find the file specified",
			exitCode: 1,
			stderr:   "ERROR: The system cannot find the file specified.",
			want:     true,
		},
		{
			name:     "task name does not exist variant",
			exitCode: 1,
			stderr:   "ERROR: Task name does not exist.",
			want:     true,
		},
		{
			name:     "the specified task name variant",
			exitCode: 1,
			stderr:   "ERROR: The specified task name does not exist.",
			want:     true,
		},
		// --- negative cases (real failures that must NOT be folded
		// into "not found") ---
		{
			name:     "permission denied (exit 1, ERROR: prefix, NOT not-found)",
			exitCode: 1,
			stderr:   "ERROR: Access is denied.",
			want:     false,
		},
		{
			name:     "scheduler service stopped",
			exitCode: 1,
			stderr:   "ERROR: The Task Scheduler service is not available.",
			want:     false,
		},
		{
			name:     "rpc server unavailable",
			exitCode: 1,
			stderr:   "ERROR: The RPC server is unavailable.",
			want:     false,
		},
		{
			name:     "malformed argument",
			exitCode: 1,
			stderr:   "ERROR: Invalid argument/option - '/BadFlag'.",
			want:     false,
		},
		{
			name:     "completely empty stderr",
			exitCode: 1,
			stderr:   "",
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isTaskNotFoundError(&schtasksError{
				Subcommand: "Query",
				ExitCode:   tc.exitCode,
				Stderr:     tc.stderr,
			})
			if got != tc.want {
				t.Errorf("isTaskNotFoundError(stderr=%q) = %v, want %v", tc.stderr, got, tc.want)
			}
		})
	}
}

// parseTaskQueryStatus pulls the third column from a `/FO CSV /NH`
// row. The header-suppression flag means the data row is the first
// (and typically only) record. The Status column position is fixed
// across schtasks versions; the column count is allowed to vary so
// locale-translated outputs with different padding still parse.
func TestParseTaskQueryStatus(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		err  bool
	}{
		{
			name: "running task",
			in:   `"\Semantica\sh.semantica.worker","N/A","Running"`,
			want: "Running",
		},
		{
			name: "ready task",
			in:   `"\Semantica\sh.semantica.worker","N/A","Ready"`,
			want: "Ready",
		},
		{
			name: "disabled task",
			in:   `"\Semantica\sh.semantica.worker","N/A","Disabled"`,
			want: "Disabled",
		},
		{
			name: "trailing whitespace stripped",
			in:   "\"\\Semantica\\sh.semantica.worker\",\"N/A\",\"Ready\"\r\n",
			want: "Ready",
		},
		{
			name: "extra columns tolerated",
			in:   `"\Semantica\sh.semantica.worker","N/A","Ready","extra1","extra2"`,
			want: "Ready",
		},
		{
			name: "empty input yields empty status",
			in:   "",
			want: "",
		},
		{
			name: "too few columns errors",
			in:   `"\Semantica\sh.semantica.worker","N/A"`,
			err:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTaskQueryStatus(tc.in)
			if tc.err {
				if err == nil {
					t.Errorf("expected error, got nil (status=%q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTaskQueryStatus: %v", err)
			}
			if got != tc.want {
				t.Errorf("parseTaskQueryStatus(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// schtasksError.Error renders consistently with the launchctl and
// systemctl error shapes so the three backends report failures the
// same way through audit and log surfaces.
func TestSchtasksError_StringFormat(t *testing.T) {
	e := &schtasksError{
		Subcommand: "Create",
		ExitCode:   1,
		Stderr:     "ERROR: Access is denied.",
	}
	got := e.Error()
	if !strings.Contains(got, "schtasks Create") {
		t.Errorf("error string missing subcommand: %q", got)
	}
	if !strings.Contains(got, "exit 1") {
		t.Errorf("error string missing exit code: %q", got)
	}
	if !strings.Contains(got, "Access is denied") {
		t.Errorf("error string missing stderr: %q", got)
	}
}
