package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	brtErrors "github.com/EvolutionAPI/evo-bot-runtime/internal/errors"
	"github.com/EvolutionAPI/evo-bot-runtime/pkg/pipeline/model"
)

// DispatchEngine segments the AI response, appends the message signature,
// and sends each part sequentially via HTTP postback.
// Swap the dispatch backend by providing a different implementation at main.go wiring.
type DispatchEngine interface {
	Dispatch(
		ctx               context.Context,
		contactID         int64,
		conversationID    int64,
		content           string,
		audioURL          string,
		audioMime         string,
		audioData         string,
		audioReplacesText bool,
		cfg               model.BotConfig,
		postbackURL       string,
	) error
}

// postbackRequest is the JSON body for each HTTP POST to the postback endpoint.
// AudioURL is preferred over AudioData (smaller payload). The CRM postback
// handler downloads the URL or decodes the base64 fallback to attach to the
// outgoing Message.
type postbackRequest struct {
	Content       string `json:"content,omitempty"`
	MessageType   string `json:"message_type"`
	ContentType   string `json:"content_type"`
	AudioURL      string `json:"audio_url,omitempty"`
	AudioMimeType string `json:"audio_mime_type,omitempty"`
	AudioData     string `json:"audio_data,omitempty"`
}

type dispatchEngineImpl struct {
	client *http.Client
	secret string
}

// postbackClientTimeout is the maximum time allowed for a single HTTP postback call.
// The per-call context (propagated via Dispatch) can still cancel earlier.
const postbackClientTimeout = 30 * time.Second

// NewDispatchEngine constructs the engine. Returns interface (GEAR R03).
// secret is the BOT_RUNTIME_SECRET sent as X-Bot-Runtime-Secret header on postback.
func NewDispatchEngine(secret string) DispatchEngine {
	return &dispatchEngineImpl{
		client: &http.Client{Timeout: postbackClientTimeout},
		secret: secret,
	}
}

func (d *dispatchEngineImpl) Dispatch(
	ctx               context.Context,
	contactID         int64,
	conversationID    int64,
	content           string,
	audioURL          string,
	audioMime         string,
	audioData         string,
	audioReplacesText bool,
	cfg               model.BotConfig,
	postbackURL       string,
) error {
	hasAudio := audioURL != "" || audioData != ""
	// In mirror modality (audioReplacesText), the audio IS the reply —
	// don't also send the text. The text travels as the audio postback's
	// caption so the CRM still records the words in chat history.
	skipText := audioReplacesText && hasAudio

	parts := segmentContent(content, cfg)
	if cfg.MessageSignature != "" && len(parts) > 0 {
		parts[0] = cfg.MessageSignature + parts[0]
	}

	start := time.Now()
	textParts := 0

	if !skipText {
		for i, part := range parts {
			select {
			case <-ctx.Done():
				slog.Info("pipeline.dispatch.interrupted",
					"contact_id", contactID,
					"conversation_id", conversationID,
					"parts_sent", i,
				)
				return brtErrors.ErrDispatchInterrupted
			default:
			}

			if err := d.sendTextPart(ctx, postbackURL, part); err != nil {
				return fmt.Errorf("pipeline.dispatch.send[%d]: %w", i, err)
			}
			textParts++

			if i < len(parts)-1 && cfg.DelayPerCharacter > 0 {
				delayMs := time.Duration(cfg.DelayPerCharacter*float64(utf8.RuneCountInString(part))) * time.Millisecond
				select {
				case <-ctx.Done():
					slog.Info("pipeline.dispatch.interrupted",
						"contact_id", contactID,
						"conversation_id", conversationID,
						"parts_sent", i+1,
					)
					return brtErrors.ErrDispatchInterrupted
				case <-time.After(delayMs):
				}
			}
		}
	}

	// Audio postback — when skipText is true the full text rides along as
	// the audio's caption so the CRM message history still has it.
	audioParts := 0
	if hasAudio {
		select {
		case <-ctx.Done():
			slog.Info("pipeline.dispatch.interrupted_before_audio",
				"contact_id", contactID, "conversation_id", conversationID)
			return brtErrors.ErrDispatchInterrupted
		default:
		}
		caption := ""
		if skipText {
			caption = content
		}
		if err := d.sendAudioPart(ctx, postbackURL, caption, audioURL, audioMime, audioData); err != nil {
			slog.Error("pipeline.dispatch.audio_failed",
				"contact_id", contactID, "conversation_id", conversationID, "error", err)
		} else {
			audioParts = 1
		}
	}

	slog.Info("pipeline.dispatch.completed",
		"contact_id",      contactID,
		"conversation_id", conversationID,
		"duration_ms",     time.Since(start).Milliseconds(),
		"parts_total",     textParts+audioParts,
		"text_parts",      textParts,
		"audio_parts",     audioParts,
		"skip_text",       skipText,
	)
	return nil
}

// sendTextPart sends a single text segment to the postback URL.
func (d *dispatchEngineImpl) sendTextPart(ctx context.Context, postbackURL, content string) error {
	return d.send(ctx, postbackURL, postbackRequest{
		Content:     content,
		MessageType: "outgoing",
		ContentType: "text",
	})
}

// sendAudioPart sends a synthesized audio reply (URL preferred, base64
// fallback). `caption` is the text the audio carries — non-empty in
// mirror modality so the CRM message has the text in its history even
// though no separate text dispatch was sent.
func (d *dispatchEngineImpl) sendAudioPart(ctx context.Context, postbackURL, caption, audioURL, audioMime, audioData string) error {
	mime := audioMime
	if mime == "" {
		mime = "audio/mpeg"
	}
	return d.send(ctx, postbackURL, postbackRequest{
		Content:       caption,
		MessageType:   "outgoing",
		ContentType:   "audio",
		AudioURL:      audioURL,
		AudioMimeType: mime,
		AudioData:     audioData,
	})
}

func (d *dispatchEngineImpl) send(ctx context.Context, postbackURL string, payload postbackRequest) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, postbackURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new_request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if d.secret != "" {
		req.Header.Set("X-Bot-Runtime-Secret", d.secret)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status: %d", resp.StatusCode)
	}
	return nil
}

// segmentContent splits content into parts of at most TextSegmentationLimit characters,
// respecting word boundaries, then merges any part shorter than TextSegmentationMinSize
// with its predecessor.
func segmentContent(content string, cfg model.BotConfig) []string {
	if !cfg.TextSegmentationEnabled || cfg.TextSegmentationLimit <= 0 {
		return []string{content}
	}

	limit := cfg.TextSegmentationLimit
	words := strings.Fields(content)
	if len(words) == 0 {
		return []string{content} // preserve empty or whitespace-only content
	}

	// Build raw parts greedily by word (rune-aware — fixes non-ASCII content)
	var rawParts []string
	var current strings.Builder
	currentRunes := 0
	for _, word := range words {
		wordRunes := utf8.RuneCountInString(word)
		if currentRunes == 0 {
			current.WriteString(word)
			currentRunes = wordRunes
		} else if currentRunes+1+wordRunes <= limit {
			current.WriteByte(' ')
			current.WriteString(word)
			currentRunes += 1 + wordRunes
		} else {
			rawParts = append(rawParts, current.String())
			current.Reset()
			current.WriteString(word)
			currentRunes = wordRunes
		}
	}
	if currentRunes > 0 {
		rawParts = append(rawParts, current.String())
	}

	// Merge parts shorter than TextSegmentationMinSize into previous part,
	// only when the merged result stays within limit (prevents overflow).
	if cfg.TextSegmentationMinSize <= 0 || len(rawParts) <= 1 {
		return rawParts
	}

	merged := make([]string, 0, len(rawParts))
	lastPartRunes := 0
	for _, p := range rawParts {
		pRunes := utf8.RuneCountInString(p)
		if len(merged) > 0 && pRunes < cfg.TextSegmentationMinSize && lastPartRunes+1+pRunes <= limit {
			merged[len(merged)-1] += " " + p
			lastPartRunes += 1 + pRunes
		} else {
			merged = append(merged, p)
			lastPartRunes = pRunes
		}
	}
	return merged
}
