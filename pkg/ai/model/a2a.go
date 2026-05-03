package model

// A2ARequest carries the data needed to call AI Processor via JSON-RPC 2.0.
// Fields are NOT serialised directly — the adapter builds the wire format.
type A2ARequest struct {
	OutgoingURL    string         // full A2A endpoint URL from agent_bot.outgoing_url
	ContactID      int64          // used for userId in JSON-RPC params
	ConversationID int64          // used for contextId in JSON-RPC params
	ApiKey         string         // used for X-API-Key header (per-event auth)
	Message        string         // aggregated buffer content (FR-15)
	Attachments    []A2AAttachment // file payloads (audio/image/etc) accumulated across the debounce window
	Metadata       map[string]any // CRM metadata passed through to processor (tools context)
}

// A2AAttachment is one file payload to be sent as a JSON-RPC FilePart.
// Data is base64-encoded.
type A2AAttachment struct {
	Name        string
	ContentType string
	Data        string
}

// jsonRPCRequest is the JSON-RPC 2.0 envelope sent to AI Processor.
type JSONRPCRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      string         `json:"id"`
	Method  string         `json:"method"`
	Params  JSONRPCParams  `json:"params"`
}

type JSONRPCParams struct {
	ContextID string         `json:"contextId"`
	UserID    string         `json:"userId"`
	Message   JSONRPCMessage `json:"message"`
	Metadata  map[string]any `json:"metadata"`
}

type JSONRPCMessage struct {
	Role  string         `json:"role"`
	Parts []JSONRPCPart  `json:"parts"`
}

type JSONRPCPart struct {
	Type string         `json:"type"`
	Text string         `json:"text,omitempty"`
	File *JSONRPCFile   `json:"file,omitempty"`
}

// JSONRPCFile mirrors the A2A FilePart "file" object — see
// evo-ai-processor a2a_routes.extract_files_from_message which expects
// `bytes` (base64) plus `name` and `mimeType`.
type JSONRPCFile struct {
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Bytes    string `json:"bytes,omitempty"`
}

// A2AResponse is the JSON-RPC 2.0 response from AI Processor.
type A2AResponse struct {
	Result *A2AResult `json:"result"`
}

type A2AResult struct {
	Artifacts []A2AArtifact `json:"artifacts"`
	Message   *A2AMessage   `json:"message"`
}

type A2AArtifact struct {
	Parts []A2APart `json:"parts"`
}

type A2AMessage struct {
	Parts []A2APart `json:"parts"`
}

type A2APart struct {
	Type string   `json:"type"`
	Text string   `json:"text,omitempty"`
	File *A2AFile `json:"file,omitempty"`
}

// A2AFile mirrors the A2A FilePart "file" object. Either `Uri` (preferred,
// short payload — typically a presigned MinIO URL) or `Bytes` (base64
// fallback) carries the audio payload. `ReplacesText` signals that the
// audio is the canonical reply for this turn — bot-runtime should skip
// the parallel text dispatch (mirror modality).
type A2AFile struct {
	Name         string `json:"name,omitempty"`
	MimeType     string `json:"mimeType,omitempty"`
	Uri          string `json:"uri,omitempty"`
	Bytes        string `json:"bytes,omitempty"`
	ReplacesText bool   `json:"replacesText,omitempty"`
}

// NormalizedResponse is the internal format after parsing A2AResponse.
// No JSON tags — this type never crosses a service boundary.
type NormalizedResponse struct {
	Content           string
	AudioURL          string // presigned URL when AI Processor stored the audio in MinIO
	AudioMimeType     string
	AudioData         string // base64 fallback when no URL is available
	AudioReplacesText bool   // when true, audio is sent INSTEAD of text (mirror mode)
}
