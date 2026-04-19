package builder

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// fakeBlobPutter records every Put call and returns predictable
// hashes. Matches the pattern used in the provider hook tests.
type fakeBlobPutter struct {
	stored map[string][]byte
}

func newFakeBlobPutter() *fakeBlobPutter {
	return &fakeBlobPutter{stored: make(map[string][]byte)}
}

func (f *fakeBlobPutter) Put(_ context.Context, b []byte) (string, int64, error) {
	h := "hash_" + string(rune(len(f.stored)+'a'))
	f.stored[h] = append([]byte(nil), b...)
	return h, int64(len(b)), nil
}

// failingBlobPutter always returns an error. Every hash-returning
// helper must degrade to "" when the blob store fails.
type failingBlobPutter struct{}

func (failingBlobPutter) Put(_ context.Context, _ []byte) (string, int64, error) {
	return "", 0, errors.New("blob store unavailable")
}

// --- PutAndHash ---

func TestPutAndHash_HappyPath(t *testing.T) {
	bs := newFakeBlobPutter()
	h := PutAndHash(context.Background(), bs, []byte("hello"))
	if h == "" {
		t.Fatal("expected non-empty hash")
	}
	if string(bs.stored[h]) != "hello" {
		t.Errorf("stored blob = %q, want hello", bs.stored[h])
	}
}

func TestPutAndHash_NilStoreReturnsEmpty(t *testing.T) {
	if h := PutAndHash(context.Background(), nil, []byte("hello")); h != "" {
		t.Errorf("PutAndHash(nil, ...) = %q, want empty", h)
	}
}

func TestPutAndHash_StoreErrorReturnsEmpty(t *testing.T) {
	if h := PutAndHash(context.Background(), failingBlobPutter{}, []byte("hello")); h != "" {
		t.Errorf("PutAndHash on failing store = %q, want empty", h)
	}
}

// --- StorePromptPayload ---

func TestStorePromptPayload_Empty(t *testing.T) {
	bs := newFakeBlobPutter()
	if h := StorePromptPayload(context.Background(), bs, ""); h != "" {
		t.Errorf("empty prompt should not store; got hash %q", h)
	}
	if len(bs.stored) != 0 {
		t.Errorf("empty prompt should not touch the store; got %d entries", len(bs.stored))
	}
}

func TestStorePromptPayload_NonEmpty(t *testing.T) {
	bs := newFakeBlobPutter()
	h := StorePromptPayload(context.Background(), bs, "hello world")
	if h == "" {
		t.Fatal("expected a hash")
	}
	if string(bs.stored[h]) != "hello world" {
		t.Errorf("stored bytes = %q, want 'hello world'", bs.stored[h])
	}
}

// --- SynthesizeAssistantBlob ---

func TestSynthesizeAssistantBlob_Shape(t *testing.T) {
	bs := newFakeBlobPutter()
	input := json.RawMessage(`{"file_path":"/repo/a.go","content":"x"}`)
	h := SynthesizeAssistantBlob(context.Background(), bs, "Write", input)
	if h == "" {
		t.Fatal("expected a hash")
	}

	var decoded struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(bs.stored[h], &decoded); err != nil {
		t.Fatalf("unmarshal stored blob: %v", err)
	}
	if decoded.Type != "assistant" {
		t.Errorf("type = %q, want assistant", decoded.Type)
	}
	if len(decoded.Message.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(decoded.Message.Content))
	}
	block := decoded.Message.Content[0]
	if block.Type != "tool_use" || block.Name != "Write" {
		t.Errorf("block: type=%q name=%q, want tool_use/Write", block.Type, block.Name)
	}
	if string(block.Input) != `{"file_path":"/repo/a.go","content":"x"}` {
		t.Errorf("block input = %q", block.Input)
	}
}

func TestSynthesizeAssistantBlob_NilStore(t *testing.T) {
	h := SynthesizeAssistantBlob(
		context.Background(), nil, "Write",
		json.RawMessage(`{"file_path":"/repo/a.go"}`),
	)
	if h != "" {
		t.Errorf("nil store must yield empty hash, got %q", h)
	}
}

func TestSynthesizeAssistantBlob_StoreError(t *testing.T) {
	h := SynthesizeAssistantBlob(
		context.Background(), failingBlobPutter{}, "Write",
		json.RawMessage(`{"file_path":"/repo/a.go"}`),
	)
	if h != "" {
		t.Errorf("failing store must yield empty hash, got %q", h)
	}
}

// An invalid json.RawMessage fails json.Marshal inside the helper.
// The silent-degradation contract requires the helper to return an
// empty hash and leave the blob store untouched, so callers keep
// assembling a well-formed event with an empty PayloadHash.
func TestSynthesizeAssistantBlob_MarshalErrorReturnsEmpty(t *testing.T) {
	bs := newFakeBlobPutter()
	h := SynthesizeAssistantBlob(
		context.Background(), bs, "Write",
		json.RawMessage("not valid json"),
	)
	if h != "" {
		t.Errorf("marshal failure must yield empty hash, got %q", h)
	}
	if len(bs.stored) != 0 {
		t.Errorf("marshal failure must not touch the blob store; got %d entries", len(bs.stored))
	}
}

// --- StoreWrappedHookProvenance ---

func TestStoreWrappedHookProvenance_WithResponse(t *testing.T) {
	bs := newFakeBlobPutter()
	toolInput := json.RawMessage(`{"file_path":"/repo/a.go","content":"x"}`)
	toolResponse := json.RawMessage(`{"success":true}`)

	h := StoreWrappedHookProvenance(context.Background(), bs, toolInput, toolResponse)
	if h == "" {
		t.Fatal("expected a hash")
	}

	var decoded struct {
		ToolInput    json.RawMessage `json:"tool_input"`
		ToolResponse json.RawMessage `json:"tool_response"`
	}
	if err := json.Unmarshal(bs.stored[h], &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(decoded.ToolInput) != string(toolInput) {
		t.Errorf("tool_input = %q, want %q", decoded.ToolInput, toolInput)
	}
	if string(decoded.ToolResponse) != string(toolResponse) {
		t.Errorf("tool_response = %q, want %q", decoded.ToolResponse, toolResponse)
	}
}

// When tool_response is empty, the field is omitted rather than
// serialized as null. This preserves the existing wire shape that
// downstream consumers parse.
func TestStoreWrappedHookProvenance_OmitsEmptyResponse(t *testing.T) {
	bs := newFakeBlobPutter()
	h := StoreWrappedHookProvenance(
		context.Background(), bs,
		json.RawMessage(`{"file_path":"/repo/a.go"}`),
		nil,
	)
	if h == "" {
		t.Fatal("expected a hash")
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(bs.stored[h], &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := decoded["tool_response"]; present {
		t.Errorf("tool_response should be omitted when empty; got payload %s", string(bs.stored[h]))
	}
	if _, present := decoded["tool_input"]; !present {
		t.Errorf("tool_input should always be present; got payload %s", string(bs.stored[h]))
	}
}

func TestStoreWrappedHookProvenance_NilStore(t *testing.T) {
	h := StoreWrappedHookProvenance(
		context.Background(), nil,
		json.RawMessage(`{}`), nil,
	)
	if h != "" {
		t.Errorf("nil store must yield empty hash, got %q", h)
	}
}

// Marshal-failure contract for the wrapper helper: an invalid
// tool_input RawMessage fails encoding and the helper must return
// "" without touching the blob store.
func TestStoreWrappedHookProvenance_MarshalErrorReturnsEmpty(t *testing.T) {
	bs := newFakeBlobPutter()
	h := StoreWrappedHookProvenance(
		context.Background(), bs,
		json.RawMessage("not valid json"),
		nil,
	)
	if h != "" {
		t.Errorf("marshal failure must yield empty hash, got %q", h)
	}
	if len(bs.stored) != 0 {
		t.Errorf("marshal failure must not touch the blob store; got %d entries", len(bs.stored))
	}
}

// Same contract when the failure is in the tool_response field.
// Exercising both positions keeps the test honest about which slot
// triggers the error; today the wrapper marshals both.
func TestStoreWrappedHookProvenance_MarshalErrorFromResponse(t *testing.T) {
	bs := newFakeBlobPutter()
	h := StoreWrappedHookProvenance(
		context.Background(), bs,
		json.RawMessage(`{"x":1}`),
		json.RawMessage("not valid json"),
	)
	if h != "" {
		t.Errorf("marshal failure must yield empty hash, got %q", h)
	}
	if len(bs.stored) != 0 {
		t.Errorf("marshal failure must not touch the blob store; got %d entries", len(bs.stored))
	}
}
