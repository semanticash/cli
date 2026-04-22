package service

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// RePushAttribution should use its own log prefix.
func TestRePushAttribution_UsesReEnrichedPrefixNotPushRemote(t *testing.T) {
	var buf bytes.Buffer
	prev := wlogWriter
	wlogWriter = &buf
	defer func() { wlogWriter = prev }()

	// Use a non-repo path to hit an early error branch.
	RePushAttribution(
		context.Background(),
		"/definitely/not/a/real/git/repo/"+t.Name(),
		"abc1234",
		"checkpoint-x",
	)

	out := buf.String()
	if !strings.Contains(out, "worker: re-push:") {
		t.Errorf("expected 'worker: re-push:' prefix, output was:\n%s", out)
	}
	if strings.Contains(out, "push-remote:") {
		t.Errorf("re-push must not use 'push-remote:' prefix, output was:\n%s", out)
	}
}
