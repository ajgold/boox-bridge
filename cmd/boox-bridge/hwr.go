package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type hwrClient struct {
	cfg   *config
	c     *http.Client
	spend *spend // set after construction (see pipeline wiring in main)
}

func newHWRClient(cfg *config) *hwrClient {
	return &hwrClient{
		cfg: cfg,
		c:   &http.Client{Timeout: 120 * time.Second},
	}
}

// hwrResult is the structured output we coerce out of Claude's response.
type hwrResult struct {
	Title          string   `json:"title"`
	BodyMarkdown   string   `json:"body_markdown"`
	Tags           []string `json:"tags"`
	Confidence     string   `json:"confidence"` // high | medium | low
	IllegibleCount int      `json:"illegible_count"`
	Model          string   `json:"-"`
	CostUSD        float64  `json:"-"`
}

const hwrSystemPrompt = `You are transcribing handwritten pages from a doctor's notebook. The handwriting is cursive and may include clinical vocabulary, drug names, dates, and abbreviations. Transcribe verbatim to clean markdown — do NOT correct apparent spelling errors (they may be technical terms or abbreviations). Preserve line breaks, lists, and emphasis. If a word is illegible or low-confidence, render it as [?word] with your best guess inside. Detect any leading '#tag' marker or known Boox tag at the top of the first page and report it separately. Output strict JSON only — no prose, no code fences, no commentary. Schema: {"title": string|null, "body_markdown": string, "tags": string[], "confidence": "high"|"medium"|"low", "illegible_count": number}.`

func (h *hwrClient) transcribe(ctx context.Context, pagePNGs [][]byte) (*hwrResult, error) {
	first, err := h.callOnce(ctx, h.cfg.HWRModelDefault, pagePNGs)
	if err != nil {
		return nil, fmt.Errorf("first-pass %s: %w", h.cfg.HWRModelDefault, err)
	}
	if first.Confidence == "high" && first.IllegibleCount <= 3 {
		return first, nil
	}
	second, err := h.callOnce(ctx, h.cfg.HWRModelEscalate, pagePNGs)
	if err != nil {
		first.Confidence = "low"
		return first, nil
	}
	if betterThan(second, first) {
		return second, nil
	}
	return first, nil
}

func betterThan(a, b *hwrResult) bool {
	rank := func(c string) int {
		switch c {
		case "high":
			return 2
		case "medium":
			return 1
		default:
			return 0
		}
	}
	ra, rb := rank(a.Confidence), rank(b.Confidence)
	if ra != rb {
		return ra > rb
	}
	return a.IllegibleCount < b.IllegibleCount
}

type contentBlock struct {
	Type   string          `json:"type"`
	Text   string          `json:"text,omitempty"`
	Source *imageSourceB64 `json:"source,omitempty"`
}

type imageSourceB64 struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicReq struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	System    string         `json:"system"`
	Messages  []anthropicMsg `json:"messages"`
}

type anthropicMsg struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type anthropicResp struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (h *hwrClient) callOnce(ctx context.Context, model string, pagePNGs [][]byte) (*hwrResult, error) {
	content := make([]contentBlock, 0, len(pagePNGs)+1)
	for _, png := range pagePNGs {
		content = append(content, contentBlock{
			Type: "image",
			Source: &imageSourceB64{
				Type:      "base64",
				MediaType: "image/png",
				Data:      pngBase64(png),
			},
		})
	}
	content = append(content, contentBlock{Type: "text", Text: "Transcribe to JSON only."})

	body, err := json.Marshal(anthropicReq{
		Model:     model,
		MaxTokens: 4096,
		System:    hwrSystemPrompt,
		Messages:  []anthropicMsg{{Role: "user", Content: content}},
	})
	if err != nil {
		return nil, err
	}

	endpoint := strings.TrimRight(h.cfg.LLMGatewayURL, "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.cfg.LLMGatewayToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("gateway %d: %s", resp.StatusCode, snippet(rb))
	}

	var ar anthropicResp
	if err := json.Unmarshal(rb, &ar); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(ar.Content) == 0 || ar.Content[0].Type != "text" {
		return nil, fmt.Errorf("unexpected response shape: %s", snippet(rb))
	}

	textPayload := stripCodeFences(ar.Content[0].Text)
	var result hwrResult
	if err := json.Unmarshal([]byte(textPayload), &result); err != nil {
		return nil, fmt.Errorf("parse model JSON: %w (text=%s)", err, snippet([]byte(textPayload)))
	}
	result.Model = ar.Model
	if h.spend != nil {
		cost, _ := h.spend.charge(ar.Model, ar.Usage.InputTokens, ar.Usage.OutputTokens)
		result.CostUSD = cost
	}
	return &result, nil
}

func stripCodeFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if nl := strings.IndexByte(s, '\n'); nl >= 0 {
			s = s[nl+1:]
		}
		if idx := strings.LastIndex(s, "```"); idx >= 0 {
			s = s[:idx]
		}
	}
	return strings.TrimSpace(s)
}

func snippet(b []byte) string {
	const max = 300
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}

// healthCheck makes a tiny single-token call to verify the gateway,
// auth, and chosen model are wired up. Called by main during startup.
func (h *hwrClient) healthCheck(ctx context.Context) error {
	body, _ := json.Marshal(anthropicReq{
		Model:     h.cfg.HWRModelDefault,
		MaxTokens: 4,
		Messages: []anthropicMsg{{
			Role:    "user",
			Content: []contentBlock{{Type: "text", Text: "ping"}},
		}},
	})
	endpoint := strings.TrimRight(h.cfg.LLMGatewayURL, "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.cfg.LLMGatewayToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(rb))
	}
	return nil
}
