package turncapture

import (
	"fmt"
	"os"
	"path/filepath"
)

// ReadTurn loads a saved boundary and verifies its turn identity.
// Missing records or incomplete lookup keys return nil.
func (r Recorder) ReadTurn(provider, sessionID, boundaryKey, turnID string) (*Record, error) {
	if provider == "" || sessionID == "" || boundaryKey == "" || turnID == "" {
		return nil, nil
	}
	var rec Record
	if err := read(filepath.Join(r.dir(provider, sessionID), identity(boundaryKey)+".json"), &rec); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if rec.Version != 1 || rec.Provider != provider || rec.SessionID != sessionID || rec.BoundaryKey != boundaryKey || rec.TurnID != turnID {
		return nil, fmt.Errorf("turn record identity mismatch")
	}
	return &rec, nil
}
