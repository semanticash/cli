package launcher

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestRenderWorkerPlist_ValidInputProducesParseableXML(t *testing.T) {
	got, err := RenderWorkerPlist(PlistInput{
		BinaryPath: "/usr/local/bin/semantica",
		LogPath:    "/Users/test/.semantica/worker-launcher.log",
	})
	if err != nil {
		t.Fatalf("RenderWorkerPlist: %v", err)
	}

	// Must be well-formed XML. This catches template corruption,
	// stray unescaped characters, and missing DOCTYPE.
	if err := xml.Unmarshal([]byte(got), new(struct{ XMLName xml.Name })); err != nil {
		t.Errorf("rendered plist is not valid XML: %v\n---\n%s", err, got)
	}
}

func TestRenderWorkerPlist_ContainsRequiredKeysAndInvocation(t *testing.T) {
	got, err := RenderWorkerPlist(PlistInput{
		BinaryPath: "/usr/local/bin/semantica",
		LogPath:    "/tmp/semantica-test.log",
	})
	if err != nil {
		t.Fatalf("RenderWorkerPlist: %v", err)
	}

	required := []string{
		"<key>Label</key>",
		"<string>sh.semantica.worker</string>",
		"<key>ProgramArguments</key>",
		"<string>/usr/local/bin/semantica</string>",
		"<string>worker</string>",
		"<string>drain</string>",
		"<key>RunAtLoad</key>",
		"<false/>",
		"<key>StandardOutPath</key>",
		"<key>StandardErrorPath</key>",
		"<string>/tmp/semantica-test.log</string>",
	}
	for _, want := range required {
		if !strings.Contains(got, want) {
			t.Errorf("rendered plist missing %q\n---\n%s", want, got)
		}
	}
}

func TestRenderWorkerPlist_DoesNotSetKeepAlive(t *testing.T) {
	got, err := RenderWorkerPlist(PlistInput{
		BinaryPath: "/usr/local/bin/semantica",
		LogPath:    "/tmp/log",
	})
	if err != nil {
		t.Fatalf("RenderWorkerPlist: %v", err)
	}
	if strings.Contains(got, "KeepAlive") {
		t.Errorf("plist must not set KeepAlive; short-lived on-demand agent\n---\n%s", got)
	}
}

func TestRenderWorkerPlist_SubstitutesPathsVerbatimForTypicalInputs(t *testing.T) {
	bin := "/Users/alice/go/bin/semantica"
	log := "/Users/alice/.semantica/worker-launcher.log"

	got, err := RenderWorkerPlist(PlistInput{BinaryPath: bin, LogPath: log})
	if err != nil {
		t.Fatalf("RenderWorkerPlist: %v", err)
	}
	if !strings.Contains(got, bin) {
		t.Errorf("expected verbatim binary path %q in rendered plist:\n%s", bin, got)
	}
	// Log path appears twice (stdout and stderr); both instances
	// should be the verbatim input.
	if strings.Count(got, log) != 2 {
		t.Errorf("expected log path %q to appear exactly twice, got %d\n---\n%s",
			log, strings.Count(got, log), got)
	}
}

func TestRenderWorkerPlist_EscapesXMLReservedCharacters(t *testing.T) {
	// A home directory name that contains an XML-reserved
	// character is unusual on macOS but not forbidden. The
	// renderer must escape such inputs so the resulting plist
	// still parses.
	bin := "/Users/alice/tools & scripts/semantica"
	log := "/Users/alice/logs/<worker>.log"

	got, err := RenderWorkerPlist(PlistInput{BinaryPath: bin, LogPath: log})
	if err != nil {
		t.Fatalf("RenderWorkerPlist: %v", err)
	}

	if strings.Contains(got, "tools & scripts") {
		t.Error("ampersand in binary path must be escaped as &amp;")
	}
	if !strings.Contains(got, "tools &amp; scripts") {
		t.Errorf("expected escaped ampersand form, got:\n%s", got)
	}
	if strings.Contains(got, "<worker>.log") {
		t.Error("angle brackets in log path must be escaped")
	}
	if err := xml.Unmarshal([]byte(got), new(struct{ XMLName xml.Name })); err != nil {
		t.Errorf("escaped plist must still parse as XML: %v\n---\n%s", err, got)
	}
}

func TestRenderWorkerPlist_ValidatesRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		in   PlistInput
	}{
		{"empty binary path", PlistInput{BinaryPath: "", LogPath: "/tmp/log"}},
		{"empty log path", PlistInput{BinaryPath: "/bin/semantica", LogPath: ""}},
		{"both empty", PlistInput{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RenderWorkerPlist(tc.in)
			if err == nil {
				t.Errorf("expected validation error for %+v", tc.in)
			}
		})
	}
}

// Launchd refuses relative ProgramArguments and log file paths;
// accepting them here would produce a plist that looks valid in
// tests but fails opaquely at bootstrap time. These cases lock
// the absolute-path contract documented on PlistInput so a future
// refactor of Validate cannot quietly weaken it.
func TestRenderWorkerPlist_RejectsRelativePaths(t *testing.T) {
	cases := []struct {
		name string
		in   PlistInput
	}{
		{
			name: "dot-relative binary path",
			in:   PlistInput{BinaryPath: "./semantica", LogPath: "/tmp/log"},
		},
		{
			name: "bare-name binary path",
			in:   PlistInput{BinaryPath: "semantica", LogPath: "/tmp/log"},
		},
		{
			name: "subdir-relative binary path",
			in:   PlistInput{BinaryPath: "bin/semantica", LogPath: "/tmp/log"},
		},
		{
			name: "tilde-prefixed binary path (shell meta, not expanded by launchd)",
			in:   PlistInput{BinaryPath: "~/bin/semantica", LogPath: "/tmp/log"},
		},
		{
			name: "dot-relative log path",
			in:   PlistInput{BinaryPath: "/bin/semantica", LogPath: "./log"},
		},
		{
			name: "subdir-relative log path",
			in:   PlistInput{BinaryPath: "/bin/semantica", LogPath: "logs/worker.log"},
		},
		{
			name: "tilde-prefixed log path",
			in:   PlistInput{BinaryPath: "/bin/semantica", LogPath: "~/logs/worker.log"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RenderWorkerPlist(tc.in)
			if err == nil {
				t.Fatalf("expected validation error for %+v", tc.in)
			}
			if !strings.Contains(err.Error(), "must be absolute") {
				t.Errorf("expected absolute-path error, got %v", err)
			}
		})
	}
}

func TestLabelWorker_StableAcrossVersions(t *testing.T) {
	// This test exists to make the label constant a deliberate
	// promise. Changing it would orphan any bootstrapped agents
	// on user machines; an upgrade that renames the label would
	// need an explicit migration plan.
	if LabelWorker != "sh.semantica.worker" {
		t.Errorf("LabelWorker changed to %q; this is an on-disk compatibility break",
			LabelWorker)
	}
}
