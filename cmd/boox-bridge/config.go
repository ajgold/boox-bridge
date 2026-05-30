package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

type config struct {
	DataDir         string
	LLMGatewayURL   string
	LLMGatewayToken string
	HWRModelDefault string
	HWRModelEscalate string
	AffineMCPURL    string
	AffineMCPToken  string
	AffineWorkspace string
	AffineParentDoc string
	MaxDailyUSD                  float64
	MaxPagesPerNote              int
	DebounceSeconds              int
	NeedsReviewIllegibleThreshold int    // prefix "[needs review]" when HWR illegible_count exceeds this
	MaxRetries                    int    // retry-spool gives up after this many attempts
	DefaultRetryAfterSeconds      int    // fallback delay when 429 doesn't include Retry-After
	WebListenAddr                 string // web UI listen address (empty = disabled)
	OwnerLabel                    string // logical owner ("claire") — also the inbox subdir
}

func loadConfig() (*config, error) {
	c := &config{
		DataDir:          envOr("BOOX_DATA_DIR", "/var/lib/boox"),
		LLMGatewayURL:    envOr("LLM_GATEWAY_URL", "https://llm.jacomail.com/anthropic"),
		LLMGatewayToken:  os.Getenv("LLM_GATEWAY_TOKEN"),
		HWRModelDefault:  envOr("HWR_MODEL_DEFAULT", "claude-sonnet-4-6"),
		HWRModelEscalate: envOr("HWR_MODEL_ESCALATE", "claude-opus-4-7"),
		AffineMCPURL:     envOr("AFFINE_MCP_URL", "http://10.0.1.21:3030/mcp"),
		AffineMCPToken:   os.Getenv("AFFINE_MCP_TOKEN"),
		AffineWorkspace:  os.Getenv("AFFINE_WORKSPACE_ID"),
		AffineParentDoc:  os.Getenv("AFFINE_PARENT_DOC_ID"),
		OwnerLabel:       envOr("OWNER_LABEL", "claire"),
		DebounceSeconds:               envInt("DEBOUNCE_SECONDS", 10),
		MaxPagesPerNote:               envInt("MAX_PAGES_PER_NOTE", 25),
		NeedsReviewIllegibleThreshold: envInt("NEEDS_REVIEW_ILLEGIBLE_THRESHOLD", 5),
		MaxRetries:                    envInt("MAX_RETRIES", 8),
		DefaultRetryAfterSeconds:      envInt("DEFAULT_RETRY_AFTER_SECONDS", 90),
		WebListenAddr:                 envOr("WEB_LISTEN_ADDR", ":3000"),
	}
	if v := os.Getenv("MAX_DAILY_LLM_USD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("MAX_DAILY_LLM_USD: %w", err)
		}
		c.MaxDailyUSD = f
	} else {
		c.MaxDailyUSD = 2.00
	}
	required := map[string]string{
		"LLM_GATEWAY_TOKEN":     c.LLMGatewayToken,
		"AFFINE_MCP_TOKEN":      c.AffineMCPToken,
		"AFFINE_WORKSPACE_ID":   c.AffineWorkspace,
		"AFFINE_PARENT_DOC_ID":  c.AffineParentDoc,
	}
	for k, v := range required {
		if v == "" {
			return nil, fmt.Errorf("%s must be set", k)
		}
	}
	return c, nil
}

func (c *config) InboxDir() string   { return filepath.Join(c.DataDir, "inbox", c.OwnerLabel) }
func (c *config) ArchiveDir() string { return filepath.Join(c.DataDir, "archive") }
func (c *config) DLQDir() string     { return filepath.Join(c.DataDir, "dlq") }
func (c *config) StateDir() string   { return filepath.Join(c.DataDir, "state") }
func (c *config) RendersDir() string { return filepath.Join(c.DataDir, "renders") }
func (c *config) RetryDir() string   { return filepath.Join(c.DataDir, "retry") }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
