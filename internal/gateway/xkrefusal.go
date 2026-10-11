package gateway

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/yetone/magpie/internal/gateway/xk/attach"
	"github.com/yetone/magpie/internal/provider"
)

// Refusal markers are protocol evidence that the native text decoder does
// not render. Read only those fields, keeping user text and tool arguments
// out of this structural check.
func xkRefusal(proto provider.Protocol, probe attach.ProbeResult) string {
	var refusal string
	responsesDeltaSeen := false
	read := func(_ string, data string) error {
		var doc struct {
			Type         string         `json:"type"`
			Refusal      string         `json:"refusal"`
			StopReason   string         `json:"stop_reason"`
			Delta        xkRefusalDelta `json:"delta"`
			ContentBlock struct {
				Type    string `json:"type"`
				Refusal string `json:"refusal"`
				Text    string `json:"text"`
			} `json:"content_block"`
			Content []struct {
				Type    string `json:"type"`
				Refusal string `json:"refusal"`
				Text    string `json:"text"`
			} `json:"content"`
			Choices []struct {
				Message struct {
					Refusal string `json:"refusal"`
				} `json:"message"`
				Delta struct {
					Refusal string `json:"refusal"`
				} `json:"delta"`
			} `json:"choices"`
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Type    string `json:"type"`
					Refusal string `json:"refusal"`
				} `json:"content"`
			} `json:"output"`
			IncompleteDetails struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
			Response struct {
				IncompleteDetails struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
				PromptFeedback struct {
					BlockReason        string `json:"blockReason"`
					BlockReasonMessage string `json:"blockReasonMessage"`
				} `json:"promptFeedback"`
				Candidates []struct {
					FinishReason  string `json:"finishReason"`
					FinishMessage string `json:"finishMessage"`
				} `json:"candidates"`
				Output []struct {
					Type    string `json:"type"`
					Content []struct {
						Type    string `json:"type"`
						Refusal string `json:"refusal"`
					} `json:"content"`
				} `json:"output"`
			} `json:"response"`
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
			PromptFeedback struct {
				BlockReason        string `json:"blockReason"`
				BlockReasonMessage string `json:"blockReasonMessage"`
			} `json:"promptFeedback"`
			Candidates []struct {
				FinishReason  string `json:"finishReason"`
				FinishMessage string `json:"finishMessage"`
			} `json:"candidates"`
		}
		if json.Unmarshal([]byte(data), &doc) != nil {
			return nil
		}
		switch proto {
		case provider.Chat:
			for _, choice := range doc.Choices {
				refusal += choice.Message.Refusal + choice.Delta.Refusal
			}
		case provider.Responses:
			if doc.Type == "response.refusal.delta" {
				refusal += doc.Delta.Value()
				responsesDeltaSeen = true
			}
			if doc.Type == "response.refusal.done" && !responsesDeltaSeen {
				refusal += doc.Refusal
			}
			appendRefusalParts := func(items []struct {
				Type    string `json:"type"`
				Content []struct {
					Type    string `json:"type"`
					Refusal string `json:"refusal"`
				} `json:"content"`
			}) {
				for _, item := range items {
					if item.Type != "message" {
						continue
					}
					for _, part := range item.Content {
						if part.Type == "refusal" {
							refusal += part.Refusal
						}
					}
				}
			}
			appendRefusalParts(doc.Output)
			appendRefusalParts(doc.Response.Output)
			if strings.EqualFold(doc.IncompleteDetails.Reason, "content_filter") || strings.EqualFold(doc.Response.IncompleteDetails.Reason, "content_filter") || strings.EqualFold(doc.Error.Code, "content_filter") {
				refusal += "content_filter"
			}
		case provider.Anthropic:
			found := false
			if doc.ContentBlock.Type == "refusal" {
				if value := firstNonEmpty(doc.ContentBlock.Refusal, doc.ContentBlock.Text); value != "" {
					refusal += value
					found = true
				}
			}
			for _, part := range doc.Content {
				if part.Type == "refusal" {
					if value := firstNonEmpty(part.Refusal, part.Text); value != "" {
						refusal += value
						found = true
					}
				}
			}
			if value := doc.Delta.Refusal; value != "" {
				refusal += value
				found = true
			}
			if !found && (strings.EqualFold(doc.StopReason, "refusal") || strings.EqualFold(doc.StopReason, "content_filter") || strings.EqualFold(doc.Delta.StopReason, "refusal") || strings.EqualFold(doc.Delta.StopReason, "content_filter")) {
				refusal += firstNonEmpty(doc.Delta.StopReason, doc.StopReason)
			}
		case provider.Gemini:
			appendGeminiRefusal := func(blockReason, blockMessage string, candidates []struct {
				FinishReason  string `json:"finishReason"`
				FinishMessage string `json:"finishMessage"`
			}) {
				if reason := geminiRefusalReason(blockReason); reason != "" {
					refusal += firstNonEmpty(blockMessage, reason)
				}
				for _, candidate := range candidates {
					if reason := geminiRefusalReason(candidate.FinishReason); reason != "" {
						refusal += firstNonEmpty(candidate.FinishMessage, reason)
					}
				}
			}
			appendGeminiRefusal(doc.PromptFeedback.BlockReason, doc.PromptFeedback.BlockReasonMessage, doc.Candidates)
			appendGeminiRefusal(doc.Response.PromptFeedback.BlockReason, doc.Response.PromptFeedback.BlockReasonMessage, doc.Response.Candidates)
		}
		return nil
	}
	if probe.Stream || xkIsSSE(probe) {
		_ = readSSE(bytes.NewReader(probe.Body), read)
	} else {
		_ = read("", string(probe.Body))
	}
	return refusal
}

// xkRefusalDelta accepts both Responses' documented string delta and the
// object-shaped delta used by a few compatibility layers. A malformed or
// unrelated delta must not make the whole event unreadable.
type xkRefusalDelta struct {
	Raw        string
	Refusal    string `json:"refusal"`
	StopReason string `json:"stop_reason"`
	Text       string `json:"text"`
}

func (d *xkRefusalDelta) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	var raw string
	if json.Unmarshal(data, &raw) == nil {
		d.Raw = raw
		return nil
	}
	var obj struct {
		Refusal    string `json:"refusal"`
		StopReason string `json:"stop_reason"`
		Text       string `json:"text"`
	}
	if json.Unmarshal(data, &obj) != nil {
		return nil
	}
	d.Refusal, d.StopReason, d.Text = obj.Refusal, obj.StopReason, obj.Text
	return nil
}

func (d xkRefusalDelta) Value() string {
	return firstNonEmpty(d.Raw, d.Refusal, d.Text, d.StopReason)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// Gemini's normal STOP/length/tool-call reasons are not policy refusals.
// Keep this list in step with stopFromGemini's safety branch, but do not treat
// OTHER or malformed tool calls as a refusal: those can be ordinary model
// termination or a recoverable tool-shape error.
func geminiRefusalReason(reason string) string {
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "MODEL_ARMOR":
		return reason
	default:
		return ""
	}
}
