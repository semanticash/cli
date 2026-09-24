package claude

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/semanticash/cli/internal/observedinput"
)

// maxObservedContentBytes limits each representation; oversized bodies record a gap.
const maxObservedContentBytes = 8 << 20

// NormalizeInput contains captured Claude records in source order.
// StartOffset is the number of transcript records preceding this batch.
type NormalizeInput struct {
	Provider    string
	SessionID   string
	Locator     string
	StartOffset int64
	Records     []json.RawMessage
}

// Normalized contains input evidence, content by hash, and provider links
// for resolving ownership across batches.
type Normalized struct {
	Turns      []observedinput.Evidence
	Contents   map[string][]byte
	CallOwners map[string]string // Tool-call ID -> issuing record UUID.
	Ancestry   map[string]string // Record UUID -> parent UUID.
	// AttachmentAncestry maps attachment UUIDs to parents for envelope resolution.
	AttachmentAncestry map[string]string
}

type claudeRecord struct {
	Type        string          `json:"type"`
	UUID        string          `json:"uuid"`
	ParentUUID  *string         `json:"parentUuid"`
	SessionID   string          `json:"sessionId"`
	IsSidechain bool            `json:"isSidechain"`
	Message     json.RawMessage `json:"message"`
	Attachment  *rawAttachment  `json:"attachment"`
	ToolResult  json.RawMessage `json:"toolUseResult"`
}

type rawAttachment struct {
	Type     string                `json:"type"`
	Filename string                `json:"filename"`
	Content  *rawAttachmentContent `json:"content"`
}

type rawAttachmentContent struct {
	Type string   `json:"type"`
	File *rawFile `json:"file"`
}

type rawFile struct {
	FilePath     string `json:"filePath"`
	Content      string `json:"content"`
	Base64       string `json:"base64"`
	OriginalSize *int64 `json:"originalSize"`
	StartLine    int    `json:"startLine"`
	NumLines     int    `json:"numLines"`
	TotalLines   int    `json:"totalLines"`
}

type rawMessage struct {
	Role    string          `json:"role"`
	ID      string          `json:"id"`
	Content json.RawMessage `json:"content"`
}

type rawBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Text      string          `json:"text"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// turnBuilder accumulates one request envelope and its observed context.
type turnBuilder struct {
	turnID   string
	requests []observedinput.RequestEvent
	obs      []observedinput.ObservedInput
	reqLinks []observedinput.RequestInputLink
	toolLink []observedinput.ToolCallLink
	gaps     []observedinput.Gap
}

// NormalizeObservedInputs extracts evidence from supplied records without I/O.
// Parent links establish envelope membership; tool results remain observed context.
func NormalizeObservedInputs(in NormalizeInput) (Normalized, error) {
	contents := map[string][]byte{}
	put := func(b []byte) string {
		sum := sha256.Sum256(b)
		h := hex.EncodeToString(sum[:])
		contents[h] = b
		return h
	}

	// Index requests before resolving attachments serialized out of order.
	requestByUUID := map[string]*observedinput.RequestEvent{}
	turns := map[string]*turnBuilder{}
	var order []string
	turnFor := func(id string) *turnBuilder {
		tb := turns[id]
		if tb == nil {
			tb = &turnBuilder{turnID: id}
			turns[id] = tb
			order = append(order, id)
		}
		return tb
	}

	type positioned struct {
		rec claudeRecord
		pos int64
	}
	records := make([]positioned, 0, len(in.Records))
	parentByUUID := map[string]string{}
	attachmentAncestry := map[string]string{}
	var malformedGaps []observedinput.Gap
	for idx, raw := range in.Records {
		// Blank and malformed records still count toward source positions.
		pos := in.StartOffset + int64(idx) + 1
		if len(bytes.TrimSpace(raw)) == 0 {
			continue // blank line: preserves position, not a record or a gap
		}
		var rec claudeRecord
		if json.Unmarshal(raw, &rec) != nil {
			malformedGaps = append(malformedGaps, observedinput.Gap{Reason: observedinput.GapMalformed, Detail: fmt.Sprintf("unparseable record at position %d", pos)})
			continue
		}
		records = append(records, positioned{rec: rec, pos: pos})
		if rec.UUID != "" && rec.ParentUUID != nil {
			parentByUUID[rec.UUID] = *rec.ParentUUID
			// Keep envelope links separate from general conversation ancestry.
			if rec.Type == "attachment" {
				attachmentAncestry[rec.UUID] = *rec.ParentUUID
			}
		}
		if rec.Type == "user" && rec.UUID != "" {
			if req, ok := userRequest(in, rec, pos); ok {
				r := req
				requestByUUID[rec.UUID] = &r
			}
		}
	}
	// Return the request ID, if found in this batch, and the last ancestor visited.
	ownerTurn := func(uuid string) (string, string) {
		seen := map[string]bool{}
		terminal := uuid
		for uuid != "" && !seen[uuid] {
			if req, ok := requestByUUID[uuid]; ok {
				return req.ID, uuid
			}
			seen[uuid] = true
			terminal = uuid
			uuid = parentByUUID[uuid]
		}
		return "", terminal
	}

	// Associate results with their calls and calls with their request ancestry.
	toolCallTurn := map[string]string{}
	callAncestor := map[string]string{}
	var unresolved []observedinput.ObservedInput
	var unresolvedGaps []observedinput.Gap
	assign := func(owner string, obs observedinput.ObservedInput, link observedinput.ToolCallLink) {
		if owner == "" {
			obs.Scope = observedinput.ScopeUnresolved
			gap := observedinput.Gap{Subject: obs.DeliveryID, Reason: observedinput.GapUnresolvedParent, Detail: "tool result has no captured owning call"}
			obs.Gaps = append(obs.Gaps, gap)
			unresolved = append(unresolved, obs)
			unresolvedGaps = append(unresolvedGaps, gap)
			return
		}
		tb := turnFor(owner)
		obs.TurnID = owner
		link.TurnID = owner
		tb.obs = append(tb.obs, obs)
		tb.toolLink = append(tb.toolLink, link)
	}
	for _, ir := range records {
		ord := ir.pos
		rec := ir.rec
		switch rec.Type {
		case "user":
			if req, ok := requestByUUID[rec.UUID]; ok {
				req.InstructionRef = putUserInstruction(in, rec, put)
				tb := turnFor(req.ID)
				req.TurnID = req.ID
				tb.requests = append(tb.requests, *req)
				continue
			}
			// Each result block is a separate delivery associated with its call.
			for _, r := range userToolResults(in, rec, ord, put) {
				assign(toolCallTurn[r.link.ToolCallID], r.obs, r.link)
			}
		case "attachment":
			obs, link, gap, parentID := attachmentObserved(in, rec, ord, requestByUUID, put)
			if parentID != "" {
				tb := turnFor(parentID)
				obs.TurnID = parentID
				tb.obs = append(tb.obs, obs)
				tb.reqLinks = append(tb.reqLinks, link)
			} else {
				unresolved = append(unresolved, obs)
				unresolvedGaps = append(unresolvedGaps, gap)
			}
		case "assistant":
			// A call is owned by the request it descends from, not the latest one.
			owner, _ := ownerTurn(rec.UUID)
			for _, id := range assistantToolUseIDs(rec.Message) {
				toolCallTurn[id] = owner
				callAncestor[id] = rec.UUID // Retain the issuing record for ancestry resolution.
			}
			for _, r := range assistantToolResults(in, rec, ord, put) {
				assign(toolCallTurn[r.link.ToolCallID], r.obs, r.link)
			}
		}
	}

	out := Normalized{Contents: contents, CallOwners: callAncestor, Ancestry: parentByUUID, AttachmentAncestry: attachmentAncestry}
	for _, id := range order {
		tb := turns[id]
		ev := observedinput.Evidence{
			Version:       observedinput.EvidenceVersion,
			Provider:      in.Provider,
			SessionID:     in.SessionID,
			TurnID:        tb.turnID,
			Requests:      tb.requests,
			Observations:  tb.obs,
			RequestLinks:  tb.reqLinks,
			ToolCallLinks: tb.toolLink,
			Gaps:          tb.gaps,
		}
		out.Turns = append(out.Turns, ev)
	}
	// Unresolved inputs and malformed records have no established request envelope.
	if len(unresolved) > 0 || len(malformedGaps) > 0 {
		ev := observedinput.Evidence{
			Version: observedinput.EvidenceVersion, Provider: in.Provider,
			SessionID: in.SessionID, TurnID: "unresolved",
			Observations: unresolved, Gaps: append(unresolvedGaps, malformedGaps...),
		}
		out.Turns = append(out.Turns, ev)
	}
	sort.SliceStable(out.Turns, func(i, j int) bool { return out.Turns[i].TurnID < out.Turns[j].TurnID })
	return out, nil
}

// userPromptText extracts string or text-block instructions.
// Records containing tool_result blocks are not requests.
func userPromptText(raw json.RawMessage) (string, bool) {
	var msg rawMessage
	if json.Unmarshal(raw, &msg) != nil {
		return "", false
	}
	var s string
	if json.Unmarshal(msg.Content, &s) == nil {
		return s, true
	}
	var blocks []rawBlock
	if json.Unmarshal(msg.Content, &blocks) != nil {
		return "", false
	}
	var text string
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return "", false
		}
		if b.Type == "text" {
			text += b.Text
		}
	}
	return text, text != ""
}

// userRequest extracts a request, excluding user-role tool results.
func userRequest(in NormalizeInput, rec claudeRecord, ord int64) (observedinput.RequestEvent, bool) {
	if _, ok := userPromptText(rec.Message); !ok {
		return observedinput.RequestEvent{}, false
	}
	origin, evidence := observedinput.OriginHuman, "user prompt record"
	if rec.IsSidechain {
		origin, evidence = observedinput.OriginDelegator, "sidechain dispatch"
	}
	req := observedinput.RequestEvent{
		ID: "req:" + rec.UUID, ProviderEventID: rec.UUID, Provider: in.Provider,
		SessionID: in.SessionID, Origin: origin, OriginEvidence: evidence, Ordinal: ord,
		Source: observedinput.SourceRef{Locator: in.Locator, Position: ord, Native: rec.UUID},
	}
	if rec.ParentUUID != nil {
		req.ParentEventID = *rec.ParentUUID
	}
	return req, true
}

func putUserInstruction(in NormalizeInput, rec claudeRecord, put func([]byte) string) string {
	text, ok := userPromptText(rec.Message)
	if !ok || text == "" {
		return ""
	}
	return put([]byte(text))
}

// attachmentObserved resolves attachment membership through parentUuid.
func attachmentObserved(in NormalizeInput, rec claudeRecord, ord int64, byUUID map[string]*observedinput.RequestEvent, put func([]byte) string) (observedinput.ObservedInput, observedinput.RequestInputLink, observedinput.Gap, string) {
	obs := observedinput.ObservedInput{
		DeliveryID: "att:" + rec.UUID, Provider: in.Provider, SessionID: in.SessionID,
		Acquisition: observedinput.AcquisitionAttachment, Ordinal: ord,
		Source: observedinput.SourceRef{Locator: in.Locator, Position: ord, Native: rec.UUID},
	}
	obs.Representation = attachmentRepresentation(rec.Attachment, &obs, put)

	parentID := ""
	parentUUID := ""
	if rec.ParentUUID != nil {
		parentUUID = *rec.ParentUUID
		if req, ok := byUUID[parentUUID]; ok {
			parentID = req.ID
		}
	}
	if parentID == "" {
		obs.Scope = observedinput.ScopeUnresolved
		// Preserve the parent ID for later resolution.
		obs.UnresolvedParentID = parentUUID
		gap := observedinput.Gap{Subject: obs.DeliveryID, Reason: observedinput.GapUnresolvedParent, Detail: "attachment parent " + parentUUID + " not resolved"}
		obs.Gaps = append(obs.Gaps, gap)
		return obs, observedinput.RequestInputLink{}, gap, ""
	}
	obs.Scope = observedinput.ScopeRequestEnvelope
	link := observedinput.RequestInputLink{
		RequestID: parentID, DeliveryID: obs.DeliveryID, Relationship: observedinput.RelAttachedTo,
		Basis: observedinput.BasisParentLink, Source: observedinput.SourceRef{Locator: in.Locator, Position: ord, Native: rec.UUID},
	}
	return obs, link, observedinput.Gap{}, parentID
}

// attachmentRepresentation records the supplied body or an explicit content gap.
func attachmentRepresentation(att *rawAttachment, obs *observedinput.ObservedInput, put func([]byte) string) (rep observedinput.Representation) {
	obs.InputSource.Kind = "unknown"
	if att != nil && att.Filename != "" {
		obs.InputSource = observedinput.InputSource{Kind: "file", Locator: att.Filename}
	}
	var file *rawFile
	if att != nil && att.Content != nil {
		file = att.Content.File
	}
	defer func() { fileFidelity(file, "", obs, &rep) }()
	if att == nil || att.Content == nil || att.Content.File == nil {
		obs.Gaps = append(obs.Gaps, observedinput.Gap{Subject: obs.DeliveryID, Reason: observedinput.GapMissingBody, Detail: "attachment carried no body"})
		return observedinput.Representation{State: observedinput.RepUnavailable}
	}
	f := att.Content.File
	switch {
	case f.Base64 != "":
		raw, err := base64.StdEncoding.DecodeString(f.Base64)
		if err != nil {
			obs.Gaps = append(obs.Gaps, observedinput.Gap{Subject: obs.DeliveryID, Reason: observedinput.GapMalformed, Detail: "undecodable base64 body"})
			return observedinput.Representation{State: observedinput.RepUnavailable}
		}
		if int64(len(raw)) > maxObservedContentBytes {
			obs.Gaps = append(obs.Gaps, observedinput.Gap{Subject: obs.DeliveryID, Reason: observedinput.GapSizeLimit})
			return observedinput.Representation{State: observedinput.RepReferenceOnly}
		}
		return observedinput.Representation{State: observedinput.RepPresent, ContentRef: put(raw), ContentSize: int64(len(raw)), MediaType: mediaType(att.Content.Type)}
	case f.Content != "":
		if int64(len(f.Content)) > maxObservedContentBytes {
			obs.Gaps = append(obs.Gaps, observedinput.Gap{Subject: obs.DeliveryID, Reason: observedinput.GapSizeLimit})
			return observedinput.Representation{State: observedinput.RepReferenceOnly}
		}
		return observedinput.Representation{
			State: observedinput.RepPresent, ContentRef: put([]byte(f.Content)), ContentSize: int64(len(f.Content)),
			MediaType: "text/plain", LineStart: f.StartLine, LineCount: f.NumLines, TotalLines: f.TotalLines,
		}
	default:
		obs.Gaps = append(obs.Gaps, observedinput.Gap{Subject: obs.DeliveryID, Reason: observedinput.GapReferenceOnly, Detail: "attachment referenced without a body"})
		return observedinput.Representation{State: observedinput.RepReferenceOnly}
	}
}

type observedResult struct {
	obs  observedinput.ObservedInput
	link observedinput.ToolCallLink
}

// resultBlock holds one tool result's call ID, text, and content status.
type resultBlock struct {
	toolUseID   string
	text        string
	isError     bool
	unsupported bool
}

// userToolResults extracts one delivery per result block.
// Record-level source metadata is used only when a single result makes it unambiguous.
func userToolResults(in NormalizeInput, rec claudeRecord, ord int64, put func([]byte) string) []observedResult {
	blocks := toolResultBlocks(rec.Message)
	if len(blocks) == 0 {
		if len(rec.ToolResult) == 0 {
			return nil
		}
		blocks = []resultBlock{{}} // metadata-only result with no explicit block
	}
	single := len(blocks) == 1
	out := make([]observedResult, 0, len(blocks))
	for i, blk := range blocks {
		obs := observedinput.ObservedInput{
			DeliveryID: fmt.Sprintf("res:%s#%d", rec.UUID, i), Provider: in.Provider, SessionID: in.SessionID,
			Acquisition: observedinput.AcquisitionToolResult, Scope: observedinput.ScopeObservedContext,
			ToolCallID: blk.toolUseID, Ordinal: ord,
			Source: observedinput.SourceRef{Locator: in.Locator, Position: ord, Native: rec.UUID},
		}
		var source json.RawMessage
		if single {
			source = rec.ToolResult
		}
		obs.Representation = toolResultRepresentation(source, blk.text, &obs, put)
		if blk.isError {
			// Preserve the provider's failure status.
			obs.Gaps = append(obs.Gaps, gap(&obs, observedinput.GapFailure, "tool reported an error result"))
		}
		// Captured text can coexist with omitted content. Unavailable results
		// already carry a gap from toolResultRepresentation.
		if blk.unsupported && obs.Representation.State == observedinput.RepPresent {
			obs.Gaps = append(obs.Gaps, gap(&obs, observedinput.GapUnsupportedShape, "unsupported tool_result component omitted"))
		}
		link := observedinput.ToolCallLink{DeliveryID: obs.DeliveryID, ToolCallID: blk.toolUseID, SessionID: in.SessionID, Source: obs.Source}
		out = append(out, observedResult{obs: obs, link: link})
	}
	return out
}

// assistantToolResults reads a result attached to an assistant record's
// toolUseResult metadata.
func assistantToolResults(in NormalizeInput, rec claudeRecord, ord int64, put func([]byte) string) []observedResult {
	if len(rec.ToolResult) == 0 {
		return nil
	}
	toolUseID := firstAssistantToolUseID(rec.Message)
	obs := observedinput.ObservedInput{
		DeliveryID: fmt.Sprintf("res:%s#0", rec.UUID), Provider: in.Provider, SessionID: in.SessionID,
		Acquisition: observedinput.AcquisitionToolResult, Scope: observedinput.ScopeObservedContext,
		ToolCallID: toolUseID, Ordinal: ord,
		Source: observedinput.SourceRef{Locator: in.Locator, Position: ord, Native: rec.UUID},
	}
	obs.Representation = toolResultRepresentation(rec.ToolResult, "", &obs, put)
	link := observedinput.ToolCallLink{DeliveryID: obs.DeliveryID, ToolCallID: toolUseID, SessionID: in.SessionID, Source: obs.Source}
	return []observedResult{{obs: obs, link: link}}
}

// toolResultBlocks extracts call IDs and text, flagging unsupported content.
func toolResultBlocks(message json.RawMessage) []resultBlock {
	var msg rawMessage
	if json.Unmarshal(message, &msg) != nil {
		return nil
	}
	var blocks []rawBlock
	if json.Unmarshal(msg.Content, &blocks) != nil {
		return nil
	}
	var out []resultBlock
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		rb := resultBlock{toolUseID: b.ToolUseID, isError: b.IsError}
		var s string
		switch {
		case json.Unmarshal(b.Content, &s) == nil:
			rb.text = s
		default:
			var nested []rawBlock
			if json.Unmarshal(b.Content, &nested) == nil {
				for _, nb := range nested {
					if nb.Type == "text" {
						rb.text += nb.Text
					} else {
						rb.unsupported = true
					}
				}
			} else if len(b.Content) > 0 {
				rb.unsupported = true
			}
		}
		out = append(out, rb)
	}
	return out
}

// toolResultRepresentation extracts supported PDF or text content and records gaps.
func toolResultRepresentation(raw json.RawMessage, agentFacing string, obs *observedinput.ObservedInput, put func([]byte) string) (rep observedinput.Representation) {
	var tr struct {
		Type   string   `json:"type"`
		File   *rawFile `json:"file"`
		Result string   `json:"result"`
		Bytes  *int64   `json:"bytes"`
		URL    string   `json:"url"`
	}
	obs.InputSource.Kind = "unknown"
	defer func() {
		fileFidelity(tr.File, agentFacing, obs, &rep)
		if tr.URL != "" {
			obs.InputSource = observedinput.InputSource{Kind: "url", Locator: tr.URL}
			rep.ReportedSourceBytes = tr.Bytes
			if tr.Result != "" && rep.State == observedinput.RepPresent {
				rep.Transformation, rep.Extent = "summarized", "partial"
			}
		}
	}()
	if len(raw) > 0 && json.Unmarshal(raw, &tr) != nil {
		obs.Gaps = append(obs.Gaps, gap(obs, observedinput.GapMalformed, "undecodable tool result metadata"))
		return observedinput.Representation{State: observedinput.RepUnavailable}
	}

	switch {
	case tr.File != nil && tr.File.Base64 != "":
		b, err := base64.StdEncoding.DecodeString(tr.File.Base64)
		if err != nil {
			obs.Gaps = append(obs.Gaps, gap(obs, observedinput.GapMalformed, "undecodable base64 result"))
			return observedinput.Representation{State: observedinput.RepUnavailable}
		}
		if int64(len(b)) > maxObservedContentBytes {
			obs.Gaps = append(obs.Gaps, gap(obs, observedinput.GapSizeLimit, ""))
			return observedinput.Representation{State: observedinput.RepReferenceOnly}
		}
		return observedinput.Representation{State: observedinput.RepPresent, ContentRef: put(b), ContentSize: int64(len(b)), MediaType: "application/pdf"}
	case tr.File != nil && tr.File.Content != "":
		return boundedText(agentFacing, tr.File.Content, obs, put)
	case tr.Result != "":
		return boundedText(agentFacing, tr.Result, obs, put)
	case agentFacing != "":
		return boundedText(agentFacing, "", obs, put)
	}
	obs.Gaps = append(obs.Gaps, gap(obs, observedinput.GapUnsupportedShape, "tool result with no recognized body"))
	return observedinput.Representation{State: observedinput.RepUnavailable}
}

// fileFidelity preserves provider metadata even when the body is unavailable.
func fileFidelity(file *rawFile, agentFacing string, obs *observedinput.ObservedInput, rep *observedinput.Representation) {
	rep.Transformation, rep.Extent = "unknown", "unknown"
	if file == nil {
		return
	}
	obs.InputSource.Kind = "file"
	if file.FilePath != "" {
		obs.InputSource = observedinput.InputSource{Kind: "file", Locator: file.FilePath}
	}
	rep.ReportedSourceBytes = file.OriginalSize
	rep.LineStart, rep.LineCount, rep.TotalLines = file.StartLine, file.NumLines, file.TotalLines
	if rep.State != observedinput.RepPresent {
		return
	}
	if file.Base64 != "" {
		rep.Transformation = "none"
		if file.OriginalSize != nil && *file.OriginalSize == rep.ContentSize {
			rep.Extent = "complete"
		}
		return
	}
	if file.Content != "" {
		rep.Transformation = "none"
		if agentFacing != "" && agentFacing != file.Content {
			rep.Transformation = "extracted"
		}
		if file.StartLine >= 1 && file.NumLines > 0 && file.TotalLines >= file.NumLines && file.StartLine-1 <= file.TotalLines-file.NumLines {
			rep.Extent = "partial"
			if file.StartLine == 1 && file.NumLines == file.TotalLines {
				rep.Extent = "complete"
			}
		}
	}
}

// boundedText bounds delivered text and stores differing source text separately.
func boundedText(agentFacing, source string, obs *observedinput.ObservedInput, put func([]byte) string) observedinput.Representation {
	primary := agentFacing
	if primary == "" {
		primary = source
	}
	if int64(len(primary)) > maxObservedContentBytes {
		obs.Gaps = append(obs.Gaps, gap(obs, observedinput.GapSizeLimit, ""))
		return observedinput.Representation{State: observedinput.RepReferenceOnly}
	}
	rep := observedinput.Representation{
		State: observedinput.RepPresent, ContentRef: put([]byte(primary)),
		ContentSize: int64(len(primary)), MediaType: "text/plain",
	}
	if source != "" && source != primary {
		if int64(len(source)) > maxObservedContentBytes {
			obs.Gaps = append(obs.Gaps, gap(obs, observedinput.GapSizeLimit, "source representation omitted"))
		} else {
			rep.SourceContentRef = put([]byte(source))
			rep.SourceContentSize = int64(len(source))
		}
	}
	return rep
}

func gap(obs *observedinput.ObservedInput, reason observedinput.GapReason, detail string) observedinput.Gap {
	return observedinput.Gap{Subject: obs.DeliveryID, Reason: reason, Detail: detail}
}

// assistantToolUseIDs lists the tool-call IDs issued in an assistant record.
func assistantToolUseIDs(message json.RawMessage) []string {
	var msg rawMessage
	if json.Unmarshal(message, &msg) != nil {
		return nil
	}
	var blocks []rawBlock
	if json.Unmarshal(msg.Content, &blocks) != nil {
		return nil
	}
	var ids []string
	for _, b := range blocks {
		if b.Type == "tool_use" && b.ID != "" {
			ids = append(ids, b.ID)
		}
	}
	return ids
}

func firstAssistantToolUseID(message json.RawMessage) string {
	var msg rawMessage
	if json.Unmarshal(message, &msg) != nil {
		return ""
	}
	var blocks []rawBlock
	if json.Unmarshal(msg.Content, &blocks) != nil {
		return ""
	}
	for _, b := range blocks {
		if b.Type == "tool_use" && b.ID != "" {
			return b.ID
		}
	}
	return ""
}

func mediaType(attContentType string) string {
	if attContentType == "pdf" {
		return "application/pdf"
	}
	return "application/octet-stream"
}
