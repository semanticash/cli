package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

func (r *Repo) HeadCommitHash(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = r.root

	out, err := cmd.Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", fmt.Errorf("git rev-parse HEAD failed: %w: %s", err, string(ee.Stderr))
		}
		return "", fmt.Errorf("git rev-parse HEAD failed: %w", err)
	}

	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", fmt.Errorf("empty HEAD sha")
	}
	return sha, nil
}
