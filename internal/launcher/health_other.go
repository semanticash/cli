//go:build !darwin

package launcher

import "context"

// daemonServiceHealth is a no-op outside darwin: the LWCR/code-signing
// spawn-refusal class is launchd-specific. Other platforms report zero
// health and rely on ServiceState alone.
func daemonServiceHealth(ctx context.Context) ServiceHealth {
	return ServiceHealth{}
}
