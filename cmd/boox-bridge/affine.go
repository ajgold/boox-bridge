package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// affineClient talks to the local Affine MCP server (running alongside
// the Affine VM at http://10.0.1.21:3030/mcp) using the streamable-HTTP
// MCP transport. The MCP server handles the BlockSuite/Y.Doc CRDT
// encoding that Affine's own GraphQL doesn't expose.
type affineClient struct {
	cfg *config

	connectOnce sync.Once
	connectErr  error
	session     *mcp.ClientSession
}

func newAffineClient(cfg *config) *affineClient {
	return &affineClient{cfg: cfg}
}

func (a *affineClient) connect(ctx context.Context) error {
	a.connectOnce.Do(func() {
		transport := &mcp.StreamableClientTransport{
			Endpoint:             a.cfg.AffineMCPURL,
			HTTPClient:           a.authHTTPClient(),
			DisableStandaloneSSE: true,
		}
		client := mcp.NewClient(&mcp.Implementation{
			Name:    "boox-bridge",
			Version: "0.1",
		}, nil)
		session, err := client.Connect(ctx, transport, nil)
		if err != nil {
			a.connectErr = fmt.Errorf("mcp connect: %w", err)
			return
		}
		a.session = session
	})
	return a.connectErr
}

// authHTTPClient builds an http.Client that injects the bearer token on
// every request.
func (a *affineClient) authHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 60 * time.Second,
		Transport: &authRoundTripper{
			base:  http.DefaultTransport,
			token: a.cfg.AffineMCPToken,
		},
	}
}

type authRoundTripper struct {
	base  http.RoundTripper
	token string
}

func (a *authRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	// Clone to avoid mutating the caller's request.
	r2 := r.Clone(r.Context())
	r2.Header.Set("Authorization", "Bearer "+a.token)
	return a.base.RoundTrip(r2)
}

type publishReq struct {
	WorkspaceID  string // required; resolved per-note by routes.Resolve
	ParentDocID  string // optional; "" = create at workspace root
	Title        string
	BodyMarkdown string
	Tags         []string
	PagePNGs     [][]byte
}

// publish creates the doc, uploads page images, appends image blocks,
// and tags the doc. Returns the new docId.
func (a *affineClient) publish(ctx context.Context, p publishReq) (string, error) {
	if err := a.connect(ctx); err != nil {
		return "", err
	}
	if p.WorkspaceID == "" {
		return "", fmt.Errorf("publish: empty WorkspaceID")
	}
	docID, err := a.createDoc(ctx, p.WorkspaceID, p.ParentDocID, p.Title, p.BodyMarkdown)
	if err != nil {
		return "", fmt.Errorf("create_doc: %w", err)
	}
	for i, png := range p.PagePNGs {
		// Content-addressed filename — Affine's blob store keys by filename
		// and does NOT overwrite duplicates, so any reused name would silently
		// resolve to the first-uploaded content. Hashing makes every distinct
		// page unique and dedupes identical pages across notes.
		sum := sha256.Sum256(png)
		filename := fmt.Sprintf("boox-page-%d-%s.png", i+1, hex.EncodeToString(sum[:8]))
		sourceID, err := a.uploadBlob(ctx, p.WorkspaceID, filename, "image/png", png)
		if err != nil {
			return docID, fmt.Errorf("upload_blob page %d: %w", i+1, err)
		}
		if err := a.appendImage(ctx, p.WorkspaceID, docID, sourceID); err != nil {
			return docID, fmt.Errorf("append_block page %d: %w", i+1, err)
		}
	}
	for _, tag := range p.Tags {
		if tag == "" {
			continue
		}
		// Tag failures are best-effort — log via parent context but don't fail publish.
		_ = a.addTag(ctx, p.WorkspaceID, docID, tag)
	}
	return docID, nil
}

func (a *affineClient) callTool(ctx context.Context, name string, args map[string]any, out any) error {
	res, err := a.session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("tool returned error: %s", firstText(res.Content))
	}
	if out == nil {
		return nil
	}
	// Prefer structured content; fall back to first text block parsed as JSON.
	if res.StructuredContent != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return fmt.Errorf("marshal structured: %w", err)
		}
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("decode structured: %w body=%s", err, snippet(b))
		}
		return nil
	}
	text := firstText(res.Content)
	if text == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		return fmt.Errorf("decode text result: %w body=%s", err, snippet([]byte(text)))
	}
	return nil
}

func firstText(cs []mcp.Content) string {
	for _, c := range cs {
		if t, ok := c.(*mcp.TextContent); ok {
			return t.Text
		}
	}
	return ""
}

// --- Tool wrappers ---

func (a *affineClient) createDoc(ctx context.Context, workspaceID, parentDocID, title, markdown string) (string, error) {
	args := map[string]any{
		"workspaceId": workspaceID,
		"title":       title,
		"markdown":    markdown,
	}
	// Preserve the existing "set only when non-empty" guard — Affine MCP
	// rejects an explicit empty-string parentDocId.
	if parentDocID != "" {
		args["parentDocId"] = parentDocID
	}
	var out struct {
		DocID string `json:"docId"`
	}
	if err := a.callTool(ctx, "create_doc_from_markdown", args, &out); err != nil {
		return "", err
	}
	if out.DocID == "" {
		return "", fmt.Errorf("create_doc_from_markdown returned no docId")
	}
	return out.DocID, nil
}

func (a *affineClient) uploadBlob(ctx context.Context, workspaceID, filename, contentType string, content []byte) (string, error) {
	args := map[string]any{
		"workspaceId": workspaceID,
		"filename":    filename,
		"contentType": contentType,
		"content":     pngBase64(content),
	}
	// MCP returns `{id, key, workspaceId, filename, contentType, size, uploadedAt}`.
	// Either `key` (Affine's blob key — content-hash style) or `id` (often the
	// same as key) is accepted by append_block's sourceId field. Probed live
	// against affine.jacomail.com 2026-05-30.
	var out struct {
		Key string `json:"key"`
		ID  string `json:"id"`
	}
	if err := a.callTool(ctx, "upload_blob", args, &out); err != nil {
		return "", err
	}
	if out.Key != "" {
		return out.Key, nil
	}
	if out.ID != "" {
		return out.ID, nil
	}
	return "", fmt.Errorf("upload_blob returned no key/id")
}

func (a *affineClient) appendImage(ctx context.Context, workspaceID, docID, sourceID string) error {
	args := map[string]any{
		"workspaceId": workspaceID,
		"docId":       docID,
		"type":        "image",
		"sourceId":    sourceID,
	}
	return a.callTool(ctx, "append_block", args, nil)
}

func (a *affineClient) addTag(ctx context.Context, workspaceID, docID, tag string) error {
	args := map[string]any{
		"workspaceId": workspaceID,
		"docId":       docID,
		"tag":         strings.TrimSpace(tag),
	}
	return a.callTool(ctx, "add_tag_to_doc", args, nil)
}

// --- v0.2: workspace + doc discovery for the routing UI ---

type wsSummary struct {
	ID        string `json:"id"`
	Public    bool   `json:"public,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
}

type docSummary struct {
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
}

func (a *affineClient) listWorkspaces(ctx context.Context) ([]wsSummary, error) {
	if err := a.connect(ctx); err != nil {
		return nil, err
	}
	// list_workspaces returns the raw array as the text/structured content.
	var out []wsSummary
	if err := a.callTool(ctx, "list_workspaces", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// docsPage is the GraphQL-shaped paginated list returned by list_docs.
type docsPage struct {
	TotalCount int `json:"totalCount"`
	PageInfo   struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Edges []struct {
		Cursor string `json:"cursor"`
		Node   struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			CreatedAt string `json:"createdAt"`
		} `json:"node"`
	} `json:"edges"`
}

func (a *affineClient) listDocs(ctx context.Context, workspaceID string) ([]docSummary, error) {
	if err := a.connect(ctx); err != nil {
		return nil, err
	}
	const pageSize = 200
	const safetyCap = 50 // 200 * 50 = 10k docs is more than enough for a home workspace
	out := []docSummary{}
	after := ""
	for i := 0; i < safetyCap; i++ {
		args := map[string]any{
			"workspaceId": workspaceID,
			"first":       pageSize,
		}
		if after != "" {
			args["after"] = after
		}
		var page docsPage
		if err := a.callTool(ctx, "list_docs", args, &page); err != nil {
			return nil, err
		}
		for _, e := range page.Edges {
			out = append(out, docSummary{
				ID:        e.Node.ID,
				Title:     e.Node.Title,
				CreatedAt: e.Node.CreatedAt,
			})
		}
		if !page.PageInfo.HasNextPage || page.PageInfo.EndCursor == "" {
			break
		}
		after = page.PageInfo.EndCursor
	}
	return out, nil
}

// healthCheck connects and lists tools to verify the MCP server is up.
func (a *affineClient) healthCheck(ctx context.Context) error {
	if err := a.connect(ctx); err != nil {
		return err
	}
	if _, err := a.session.ListTools(ctx, nil); err != nil {
		return fmt.Errorf("list_tools: %w", err)
	}
	return nil
}
