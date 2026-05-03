package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	brtErrors "github.com/EvolutionAPI/evo-bot-runtime/internal/errors"
	"github.com/EvolutionAPI/evo-bot-runtime/pkg/ai/model"
)

// maxResponseBytes caps the AI Processor response body to prevent OOM on oversized payloads.
const maxResponseBytes = 1 << 20 // 1 MiB

// AIAdapter calls the AI Processor via A2A protocol (JSON-RPC 2.0).
// Swap the backend by providing a different implementation at main.go wiring.
type AIAdapter interface {
	Call(ctx context.Context, req *model.A2ARequest) (*model.NormalizedResponse, error)
}

type aiAdapter struct {
	timeoutSecs int
	client      *http.Client
}

// NewAIAdapter constructs the adapter. Returns interface (GEAR R03).
// The AI Processor URL comes from each event's outgoing_url field.
func NewAIAdapter(timeoutSecs int) AIAdapter {
	return &aiAdapter{
		timeoutSecs: timeoutSecs,
		client:      &http.Client{},
	}
}

func (a *aiAdapter) Call(ctx context.Context, req *model.A2ARequest) (*model.NormalizedResponse, error) {
	start := time.Now()

	// Wrap with timeout — inner timeout, outer ctx for pipeline cancellation.
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(a.timeoutSecs)*time.Second)
	defer cancel()

	// Use the full outgoing_url provided by the CRM (already contains the agent ID)
	url := req.OutgoingURL

	// Build the message parts. The first part is always the text buffer
	// (may be empty when the user sent only audio); each attachment is a
	// FilePart so the AI Processor's extract_files_from_message picks it
	// up and feeds it through process_files (transcription, etc).
	parts := []model.JSONRPCPart{{Type: "text", Text: req.Message}}
	for _, att := range req.Attachments {
		parts = append(parts, model.JSONRPCPart{
			Type: "file",
			File: &model.JSONRPCFile{
				Name:     att.Name,
				MimeType: att.ContentType,
				Bytes:    att.Data,
			},
		})
	}

	// Build JSON-RPC 2.0 envelope
	rpcReq := model.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      fmt.Sprintf("%d:%d", req.ContactID, req.ConversationID),
		Method:  "message/send",
		Params: model.JSONRPCParams{
			ContextID: fmt.Sprintf("%d", req.ConversationID),
			UserID:    fmt.Sprintf("%d", req.ContactID),
			Message: model.JSONRPCMessage{
				Role:  "user",
				Parts: parts,
			},
			Metadata: nonNilMetadata(req.Metadata),
		},
	}

	body, err := json.Marshal(rpcReq)
	if err != nil {
		return nil, fmt.Errorf("pipeline.ai.marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(timeoutCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("pipeline.ai.new_request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", req.ApiKey)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, brtErrors.ErrPipelineCancelled
		}
		if errors.Is(timeoutCtx.Err(), context.DeadlineExceeded) {
			slog.Warn("pipeline.ai.http.timeout",
				"contact_id", req.ContactID,
				"conversation_id", req.ConversationID,
				"timeout_secs", a.timeoutSecs,
			)
			return nil, brtErrors.ErrAITimeout
		}
		return nil, fmt.Errorf("pipeline.ai.http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pipeline.ai.status: unexpected %d from AI Processor", resp.StatusCode)
	}

	var a2aResp model.A2AResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&a2aResp); err != nil {
		return nil, fmt.Errorf("pipeline.ai.decode: %w", err)
	}

	content := extractResponseText(&a2aResp)
	audioURL, audioMime, audioBytes, replacesText := extractResponseAudio(&a2aResp)

	slog.Info("pipeline.ai.http.completed",
		"contact_id", req.ContactID,
		"conversation_id", req.ConversationID,
		"duration_ms", time.Since(start).Milliseconds(),
		"audio_url", audioURL != "",
		"audio_bytes", audioBytes != "",
		"replaces_text", replacesText,
	)

	return &model.NormalizedResponse{
		Content:           content,
		AudioURL:          audioURL,
		AudioMimeType:     audioMime,
		AudioData:         audioBytes,
		AudioReplacesText: replacesText,
	}, nil
}

// extractResponseAudio walks artifacts for the first FilePart with audio
// content. Returns (uri, mimeType, base64Bytes, replacesText). `uri` is
// preferred over `bytes`; `replacesText` is true when the audio is the
// canonical reply (mirror modality — text dispatch should be skipped).
func extractResponseAudio(resp *model.A2AResponse) (string, string, string, bool) {
	if resp.Result == nil {
		return "", "", "", false
	}
	for _, artifact := range resp.Result.Artifacts {
		for _, part := range artifact.Parts {
			if part.Type != "file" || part.File == nil {
				continue
			}
			if part.File.Uri != "" {
				return part.File.Uri, part.File.MimeType, "", part.File.ReplacesText
			}
			if part.File.Bytes != "" {
				return "", part.File.MimeType, part.File.Bytes, part.File.ReplacesText
			}
		}
	}
	return "", "", "", false
}

// extractResponseText extracts the text content from the A2A JSON-RPC response.
// Tries result.artifacts[0].parts[0].text first, then result.message.parts[0].text.
func extractResponseText(resp *model.A2AResponse) string {
	if resp.Result == nil {
		return ""
	}
	// Try artifacts first (primary response format)
	if len(resp.Result.Artifacts) > 0 {
		for _, artifact := range resp.Result.Artifacts {
			for _, part := range artifact.Parts {
				if part.Text != "" {
					return part.Text
				}
			}
		}
	}
	// Fallback to message format
	if resp.Result.Message != nil {
		for _, part := range resp.Result.Message.Parts {
			if part.Text != "" {
				return part.Text
			}
		}
	}
	return ""
}

// nonNilMetadata ensures metadata is never nil (avoids "null" in JSON).
func nonNilMetadata(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
