package broker

import (
	"context"
	"database/sql"
	"fmt"
)

// verifyRetainedRow rejects conflicting identities and repairs missing evidence
// references left by older writers. The comparison runs in the write transaction.
func verifyRetainedRow(ctx context.Context, tx *sql.Tx, repoID, sessionID string, ev RawEvent, tools, source string) error {
	var repo, session, kind, role, toolUses, eventSource string
	var ts int64
	var tokensIn, tokensOut, cacheRead, cacheCreate sql.NullInt64
	var payload, provenance, turn, toolUseID, toolName, providerID, summary sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT repository_id, session_id, ts, kind,
		coalesce(role,''), coalesce(tool_uses,''), event_source, payload_hash,
		provenance_hash, turn_id, tool_use_id, tool_name, provider_event_id, summary,
		tokens_in, tokens_out, tokens_cache_read, tokens_cache_create
		FROM agent_events WHERE event_id = ?`, ev.EventID).Scan(
		&repo, &session, &ts, &kind, &role, &toolUses, &eventSource, &payload,
		&provenance, &turn, &toolUseID, &toolName, &providerID, &summary, &tokensIn, &tokensOut, &cacheRead, &cacheCreate)
	if err != nil {
		return err
	}
	if repo != repoID || session != sessionID || ts != ev.Timestamp || kind != ev.Kind ||
		role != ev.Role || toolUses != tools || eventSource != source || turn.String != ev.TurnID ||
		toolUseID.String != ev.ToolUseID || toolName.String != ev.ToolName || providerID.String != ev.ProviderEventID || summary.String != ev.Summary ||
		tokensIn != nullableToken(ev.TokensIn, ev.TokenUsageValid) || tokensOut != nullableToken(ev.TokensOut, ev.TokenUsageValid) ||
		cacheRead != nullableToken(ev.TokensCacheRead, ev.TokenUsageValid) || cacheCreate != nullableToken(ev.TokensCacheCreate, ev.TokenUsageValid) ||
		(payload.String != "" && payload.String != ev.PayloadHash) || (provenance.String != "" && provenance.String != ev.ProvenanceHash) {
		return fmt.Errorf("event %s content mismatch in destination", ev.EventID)
	}
	_, err = tx.ExecContext(ctx, `UPDATE agent_events SET payload_hash = nullif(?, ''), provenance_hash = nullif(?, '') WHERE event_id = ?`, ev.PayloadHash, ev.ProvenanceHash, ev.EventID)
	return err
}
