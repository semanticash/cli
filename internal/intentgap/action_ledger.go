package intentgap

// ActionLedger is the action-side view consumed by retrieval and
// verifier packet assembly. All preserves bundle order; ByID is a
// flat lookup for citation validation; ByFile groups action_ids by
// the file path each action recorded. Actions whose FilePath is
// empty (Bash with no derivable target, etc.) only appear in All
// and ByID — they cannot anchor a file-scoped lookup.
type ActionLedger struct {
	All    []BundleAgentAction
	ByID   map[string]BundleAgentAction
	ByFile map[string][]string
}

// BuildActionLedger indexes bundle.AgentActions for downstream
// consumers. Deterministic; no LLM. The slice copy keeps the
// ledger's state independent of mutations on the source bundle.
func BuildActionLedger(actions []BundleAgentAction) ActionLedger {
	ledger := ActionLedger{
		ByID:   make(map[string]BundleAgentAction, len(actions)),
		ByFile: map[string][]string{},
	}
	if len(actions) == 0 {
		return ledger
	}
	ledger.All = make([]BundleAgentAction, len(actions))
	copy(ledger.All, actions)
	for _, a := range ledger.All {
		ledger.ByID[a.ActionID] = a
		if a.FilePath == "" {
			continue
		}
		ledger.ByFile[a.FilePath] = append(ledger.ByFile[a.FilePath], a.ActionID)
	}
	return ledger
}
