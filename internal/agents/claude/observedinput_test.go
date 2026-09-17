package claude

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/semanticash/cli/internal/observedinput"
)

const knownPDFHash = "507573469c56b000ffd29fa5152b0aaac08b3e776cf0fd844acff0bf1944f97e"

func normalizeFixture(t *testing.T, name string) Normalized {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "observedinput", name))
	if err != nil {
		t.Fatal(err)
	}
	var recs []json.RawMessage
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		if len(line) > 0 {
			recs = append(recs, json.RawMessage(line))
		}
	}
	n, err := NormalizeObservedInputs(NormalizeInput{Provider: "claude_code", SessionID: "sess", Locator: name, Records: recs})
	if err != nil {
		t.Fatalf("normalize %s: %v", name, err)
	}
	for _, ev := range n.Turns {
		if err := observedinput.Validate(ev); err != nil {
			t.Fatalf("%s turn %s invalid: %v", name, ev.TurnID, err)
		}
	}
	return n
}

func findTurn(n Normalized, turnID string) *observedinput.Evidence {
	for i := range n.Turns {
		if n.Turns[i].TurnID == turnID {
			return &n.Turns[i]
		}
	}
	return nil
}

func findObs(ev *observedinput.Evidence, deliveryID string) *observedinput.ObservedInput {
	for i := range ev.Observations {
		if ev.Observations[i].DeliveryID == deliveryID {
			return &ev.Observations[i]
		}
	}
	return nil
}

func TestNormalize_PDFAttachmentAndRead(t *testing.T) {
	n := normalizeFixture(t, "captured_pdf_attachment.jsonl")
	if len(n.Turns) != 1 {
		t.Fatalf("want one turn, got %d", len(n.Turns))
	}
	ev := &n.Turns[0]

	// Preserve the attachment's exact PDF bytes.
	var att *observedinput.ObservedInput
	for i := range ev.Observations {
		if ev.Observations[i].Acquisition == observedinput.AcquisitionAttachment {
			att = &ev.Observations[i]
		}
	}
	if att == nil {
		t.Fatal("no attachment observation")
	}
	if att.Scope != observedinput.ScopeRequestEnvelope {
		t.Fatalf("attachment scope = %q, want request_envelope", att.Scope)
	}
	if att.Representation.ContentRef != knownPDFHash || att.Representation.ContentSize != 1849 {
		t.Fatalf("PDF content identity wrong: ref=%s size=%d", att.Representation.ContentRef, att.Representation.ContentSize)
	}
	if got, ok := n.Contents[knownPDFHash]; !ok || sha256Hex(got) != knownPDFHash {
		t.Fatal("PDF bytes not persisted under their content hash")
	}
	// Parent linkage establishes envelope membership.
	if len(ev.RequestLinks) != 1 || ev.RequestLinks[0].Basis != observedinput.BasisParentLink {
		t.Fatalf("attachment not linked by provider parent structure: %+v", ev.RequestLinks)
	}

	// PDF reads remain observed context.
	var read *observedinput.ObservedInput
	for i := range ev.Observations {
		if ev.Observations[i].Acquisition == observedinput.AcquisitionToolResult {
			read = &ev.Observations[i]
		}
	}
	if read == nil || read.Scope != observedinput.ScopeObservedContext {
		t.Fatalf("PDF read is not observed context: %+v", read)
	}
}

func TestNormalize_TextAttachment(t *testing.T) {
	n := normalizeFixture(t, "captured_text_attachment.jsonl")
	ev := &n.Turns[0]
	att := firstAttachment(ev)
	if att == nil || att.Scope != observedinput.ScopeRequestEnvelope {
		t.Fatalf("text attachment not an envelope input: %+v", att)
	}
	if att.Representation.ContentSize != 326 || att.Representation.MediaType != "text/plain" {
		t.Fatalf("text attachment representation wrong: %+v", att.Representation)
	}
	if att.Representation.TotalLines == 0 {
		t.Fatal("line range not preserved")
	}
}

func TestNormalize_WebFetchSummaryDistinctFromBytes(t *testing.T) {
	n := normalizeFixture(t, "captured_webfetch.jsonl")
	ev := findTurnWithObs(t, n, observedinput.AcquisitionToolResult)
	res := firstToolResult(ev)
	if res.Scope != observedinput.ScopeObservedContext || res.ToolCallID == "" {
		t.Fatalf("webfetch not observed context with tool-call join: %+v", res)
	}
	if res.Representation.ContentSize != 1397 {
		t.Fatalf("webfetch summary size = %d, want 1397", res.Representation.ContentSize)
	}
	// Keep fetched size separate from summary size.
	if res.Representation.SourceDigest != "fetched_bytes=63305" {
		t.Fatalf("fetched byte count not recorded separately: %q", res.Representation.SourceDigest)
	}
}

func TestNormalize_NullAttachmentKeepsLinkPlusGap(t *testing.T) {
	n := normalizeFixture(t, "synthetic_null_attachment.jsonl")
	ev := &n.Turns[0]
	att := firstAttachment(ev)
	if att.Scope != observedinput.ScopeRequestEnvelope {
		t.Fatal("null attachment lost its envelope membership")
	}
	if att.Representation.State != observedinput.RepUnavailable {
		t.Fatalf("null attachment representation = %q, want unavailable", att.Representation.State)
	}
	if !gapPresent(att.Gaps, observedinput.GapMissingBody) {
		t.Fatalf("missing-body gap not recorded: %+v", att.Gaps)
	}
}

func TestNormalize_LateAttachmentBelongsToItsRequest(t *testing.T) {
	n := normalizeFixture(t, "synthetic_multirequest.jsonl")
	if len(n.Turns) != 2 {
		t.Fatalf("want two turns, got %d", len(n.Turns))
	}
	t1 := findTurn(n, "req:req-1")
	t2 := findTurn(n, "req:req-2")
	if t1 == nil || t2 == nil {
		t.Fatalf("turns not separated: %+v", n.Turns)
	}
	if findObs(t1, "att:att-late") == nil {
		t.Fatal("late attachment not assigned to its own request R1")
	}
	if findObs(t2, "att:att-late") != nil {
		t.Fatal("late attachment leaked into the later request R2")
	}
}

func TestNormalize_DelegationHasOwnOrigin(t *testing.T) {
	n := normalizeFixture(t, "synthetic_delegation.jsonl")
	child := findTurn(n, "req:child-1")
	if child == nil || len(child.Requests) != 1 {
		t.Fatalf("child request turn missing: %+v", n.Turns)
	}
	if child.Requests[0].Origin != observedinput.OriginDelegator {
		t.Fatalf("delegated request origin = %q, want delegator", child.Requests[0].Origin)
	}
	if findObs(child, "att:att-child") == nil {
		t.Fatal("child envelope input not retained in the child turn")
	}
	parent := findTurn(n, "req:parent-1")
	if parent == nil || parent.Requests[0].Origin != observedinput.OriginHuman {
		t.Fatal("parent request should stay human")
	}
}

func TestNormalize_UserToolResultIsNotRequest(t *testing.T) {
	recs := []json.RawMessage{
		json.RawMessage(`{"type":"user","uuid":"u1","message":{"role":"user","content":"real prompt"}}`),
		json.RawMessage(`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_x","name":"Read","input":{"file_path":"/x"}}]}}`),
		json.RawMessage(`{"type":"user","uuid":"u2","parentUuid":"a1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_x","content":"result"}]},"toolUseResult":{"type":"text","file":{"filePath":"/x","content":"body","numLines":1,"totalLines":1}}}`),
	}
	n, err := NormalizeObservedInputs(NormalizeInput{Provider: "claude_code", SessionID: "sess", Locator: "inline", Records: recs})
	if err != nil {
		t.Fatal(err)
	}
	ev := findTurn(n, "req:u1")
	if ev == nil || len(ev.Requests) != 1 {
		t.Fatalf("real prompt not a request: %+v", n.Turns)
	}
	for _, r := range ev.Requests {
		if r.ProviderEventID == "u2" {
			t.Fatal("tool result became a request event")
		}
	}
	// Keep the result as context linked to its call.
	if findObs(ev, "res:u2#0") == nil {
		t.Fatal("user tool result not retained as observed context")
	}
}

func TestNormalize_Deterministic(t *testing.T) {
	a := normalizeFixture(t, "captured_pdf_attachment.jsonl")
	b := normalizeFixture(t, "captured_pdf_attachment.jsonl")
	if !reflect.DeepEqual(a, b) {
		t.Fatal("normalization is not deterministic")
	}
}

func sha256Hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func firstAttachment(ev *observedinput.Evidence) *observedinput.ObservedInput {
	for i := range ev.Observations {
		if ev.Observations[i].Acquisition == observedinput.AcquisitionAttachment {
			return &ev.Observations[i]
		}
	}
	return nil
}

func firstToolResult(ev *observedinput.Evidence) *observedinput.ObservedInput {
	for i := range ev.Observations {
		if ev.Observations[i].Acquisition == observedinput.AcquisitionToolResult {
			return &ev.Observations[i]
		}
	}
	return nil
}

func findTurnWithObs(t *testing.T, n Normalized, acq observedinput.Acquisition) *observedinput.Evidence {
	t.Helper()
	for i := range n.Turns {
		for _, o := range n.Turns[i].Observations {
			if o.Acquisition == acq {
				return &n.Turns[i]
			}
		}
	}
	t.Fatalf("no turn with acquisition %q", acq)
	return nil
}

func gapPresent(gaps []observedinput.Gap, reason observedinput.GapReason) bool {
	for _, g := range gaps {
		if g.Reason == reason {
			return true
		}
	}
	return false
}

// Serialization order must not change call ownership.
func TestNormalize_ToolResultOwnedByOriginatingCall(t *testing.T) {
	// The assistant call record is parented to R1 but serialized after R2 begins.
	recs := []json.RawMessage{
		json.RawMessage(`{"type":"user","uuid":"r1","message":{"role":"user","content":"first"}}`),
		json.RawMessage(`{"type":"user","uuid":"r2","parentUuid":"r1","message":{"role":"user","content":"second"}}`),
		json.RawMessage(`{"type":"assistant","uuid":"a1","parentUuid":"r1","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/f"}}]}}`),
		json.RawMessage(`{"type":"user","uuid":"res","parentUuid":"a1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"body"}]},"toolUseResult":{"type":"text","file":{"filePath":"/f","content":"body","numLines":1,"totalLines":1}}}`),
	}
	n, err := NormalizeObservedInputs(NormalizeInput{Provider: "claude_code", SessionID: "sess", Locator: "inline", Records: recs})
	if err != nil {
		t.Fatal(err)
	}
	t1 := findTurn(n, "req:r1")
	t2 := findTurn(n, "req:r2")
	if t1 == nil || findObs(t1, "res:res#0") == nil {
		t.Fatalf("late tool result not owned by the originating call's turn: %+v", n.Turns)
	}
	if t2 != nil && findObs(t2, "res:res#0") != nil {
		t.Fatal("tool result leaked into the later request")
	}
}

func TestNormalize_TextBlockRequest(t *testing.T) {
	recs := []json.RawMessage{
		json.RawMessage(`{"type":"user","uuid":"u1","message":{"role":"user","content":[{"type":"text","text":"a text-block prompt"}]}}`),
	}
	n, err := NormalizeObservedInputs(NormalizeInput{Provider: "claude_code", SessionID: "sess", Locator: "inline", Records: recs})
	if err != nil {
		t.Fatal(err)
	}
	ev := findTurn(n, "req:u1")
	if ev == nil || len(ev.Requests) != 1 || ev.Requests[0].InstructionRef == "" {
		t.Fatalf("text-block request dropped: %+v", n.Turns)
	}
}

func TestNormalize_MalformedResultIsValidGap(t *testing.T) {
	recs := []json.RawMessage{
		json.RawMessage(`{"type":"user","uuid":"u1","message":{"role":"user","content":"prompt"}}`),
		json.RawMessage(`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/f.pdf"}}]}}`),
		json.RawMessage(`{"type":"user","uuid":"res","parentUuid":"a1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1"}]},"toolUseResult":{"type":"pdf","file":{"filePath":"/f.pdf","base64":"!!not base64!!"}}}`),
	}
	n := mustNormalize(t, recs)
	ev := findTurn(n, "req:u1")
	res := firstToolResult(ev)
	if res == nil || res.Representation.State != observedinput.RepUnavailable {
		t.Fatalf("malformed result not marked unavailable: %+v", res)
	}
	if !gapPresent(res.Gaps, observedinput.GapMalformed) {
		t.Fatalf("malformed result without explicit gap: %+v", res.Gaps)
	}
	// A malformed result must produce valid evidence with a gap.
	if err := observedinput.Validate(*ev); err != nil {
		t.Fatalf("malformed result broke document validity: %v", err)
	}
}

func mustNormalize(t *testing.T, recs []json.RawMessage) Normalized {
	t.Helper()
	n, err := NormalizeObservedInputs(NormalizeInput{Provider: "claude_code", SessionID: "sess", Locator: "inline", Records: recs})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range n.Turns {
		if err := observedinput.Validate(ev); err != nil {
			t.Fatalf("turn %s invalid: %v", ev.TurnID, err)
		}
	}
	return n
}

// Two tool results in one record are two deliveries, each owned by its own call.
func TestNormalize_MultipleResultsAreDistinctDeliveries(t *testing.T) {
	recs := []json.RawMessage{
		json.RawMessage(`{"type":"user","uuid":"u1","message":{"role":"user","content":"prompt"}}`),
		json.RawMessage(`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read"},{"type":"tool_use","id":"toolu_2","name":"Read"}]}}`),
		json.RawMessage(`{"type":"user","uuid":"res","parentUuid":"a1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"first body"},{"type":"tool_result","tool_use_id":"toolu_2","content":"second body"}]}}`),
	}
	n := mustNormalize(t, recs)
	ev := findTurn(n, "req:u1")
	a := findObs(ev, "res:res#0")
	b := findObs(ev, "res:res#1")
	if a == nil || b == nil {
		t.Fatalf("results not split into distinct deliveries: %+v", ev.Observations)
	}
	if a.ToolCallID != "toolu_1" || b.ToolCallID != "toolu_2" {
		t.Fatalf("results not attributed to their own calls: %s %s", a.ToolCallID, b.ToolCallID)
	}
	if a.Representation.ContentSize == b.Representation.ContentSize && a.Representation.ContentRef == b.Representation.ContentRef {
		t.Fatal("distinct result bodies were merged")
	}
}

// A late call retains the ownership established by its parent.
func TestNormalize_LateCallOwnedByParent(t *testing.T) {
	recs := []json.RawMessage{
		json.RawMessage(`{"type":"user","uuid":"r1","message":{"role":"user","content":"first"}}`),
		json.RawMessage(`{"type":"user","uuid":"r2","parentUuid":"r1","message":{"role":"user","content":"second"}}`),
		json.RawMessage(`{"type":"assistant","uuid":"a1","parentUuid":"r1","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read"}]}}`),
		json.RawMessage(`{"type":"user","uuid":"res","parentUuid":"a1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"b"}]}}`),
	}
	n := mustNormalize(t, recs)
	if findObs(findTurn(n, "req:r1"), "res:res#0") == nil {
		t.Fatal("late call's result not owned by its parent request")
	}
	if t2 := findTurn(n, "req:r2"); t2 != nil && len(t2.Observations) != 0 {
		t.Fatalf("later request absorbed the late call: %+v", t2.Observations)
	}
}

func TestNormalize_ErrorResultRetainsFailure(t *testing.T) {
	recs := []json.RawMessage{
		json.RawMessage(`{"type":"user","uuid":"u1","message":{"role":"user","content":"prompt"}}`),
		json.RawMessage(`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash"}]}}`),
		json.RawMessage(`{"type":"user","uuid":"res","parentUuid":"a1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":"command failed"}]}}`),
	}
	n := mustNormalize(t, recs)
	res := firstToolResult(findTurn(n, "req:u1"))
	if res == nil || !gapPresent(res.Gaps, observedinput.GapFailure) {
		t.Fatalf("error result lost its failure status: %+v", res)
	}
}

func TestNormalize_NestedTextResultPreserved(t *testing.T) {
	// A tool_result whose content is a nested text-block array, with no metadata.
	recs := []json.RawMessage{
		json.RawMessage(`{"type":"user","uuid":"u1","message":{"role":"user","content":"prompt"}}`),
		json.RawMessage(`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read"}]}}`),
		json.RawMessage(`{"type":"user","uuid":"res","parentUuid":"a1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"nested body"}]}]}}`),
	}
	n := mustNormalize(t, recs)
	res := firstToolResult(findTurn(n, "req:u1"))
	if res == nil || res.Representation.State != observedinput.RepPresent || res.Representation.ContentSize != int64(len("nested body")) {
		t.Fatalf("nested text result dropped: %+v", res)
	}
}

// Partially supported results retain both captured text and an omission gap.
func TestNormalize_MixedContentKeepsGap(t *testing.T) {
	recs := []json.RawMessage{
		json.RawMessage(`{"type":"user","uuid":"u1","message":{"role":"user","content":"prompt"}}`),
		json.RawMessage(`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read"}]}}`),
		json.RawMessage(`{"type":"user","uuid":"res","parentUuid":"a1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"caption"},{"type":"image","source":{"data":"..."}}]}]}}`),
	}
	n := mustNormalize(t, recs)
	res := firstToolResult(findTurn(n, "req:u1"))
	if res == nil || res.Representation.State != observedinput.RepPresent {
		t.Fatalf("captured text lost when mixed with an image: %+v", res)
	}
	if !gapPresent(res.Gaps, observedinput.GapUnsupportedShape) {
		t.Fatalf("omitted image did not record a gap: %+v", res.Gaps)
	}
}
