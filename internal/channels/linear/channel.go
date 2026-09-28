package linear

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

const (
	maxWebhookBytes = 2 << 20
	maxWebhookAge   = 5 * time.Minute
)

type Channel struct {
	*channels.Base
	cfg Config
	client *http.Client

	mu sync.Mutex
	server *http.Server
	listener net.Listener
	viewerID string
	seen map[string]struct{}
	seenFIFO []string
}

func New(section channels.Section, publisher channels.InboundPublisher) (*Channel, error) {
	cfg, err := sectionConfig(section)
	if err != nil { return nil, err }
	c := &Channel{
		cfg: cfg,
		client: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		seen: make(map[string]struct{}),
	}
	c.Base = channels.NewBase(c, section, publisher,
		channels.WithName(ChannelName), channels.WithDisplayName("Linear"))
	return c, nil
}

func (c *Channel) Start(ctx context.Context) error {
	if err := c.cfg.validateRuntime(); err != nil { return err }
	viewerID, err := c.fetchViewerID(ctx)
	if err != nil { return fmt.Errorf("linear: API authentication failed: %w", err) }
	c.viewerID = viewerID

	listener, err := net.Listen("tcp", c.cfg.ListenAddr)
	if err != nil { return fmt.Errorf("linear: listen: %w", err) }
	server := &http.Server{
		Handler: c.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 20 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout: 60 * time.Second,
	}
	c.mu.Lock()
	c.listener, c.server = listener, server
	c.mu.Unlock()
	c.SetRunning(true)
	defer func() {
		c.SetRunning(false)
		c.mu.Lock()
		c.listener, c.server = nil, nil
		c.mu.Unlock()
	}()

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdownCtx)
		case <-done:
		}
	}()
	err = server.Serve(listener)
	close(done)
	if errors.Is(err, http.ErrServerClosed) || ctx.Err() != nil { return nil }
	return err
}

func (c *Channel) Stop(ctx context.Context) error {
	c.SetRunning(false)
	c.mu.Lock()
	server := c.server
	c.mu.Unlock()
	if server == nil { return nil }
	return server.Shutdown(ctx)
}

func (c *Channel) Send(ctx context.Context, message core.OutboundMessage) error {
	issueID := strings.TrimSpace(message.ChatID)
	if issueID == "" { return errors.New("linear: issue id is required") }
	query := `mutation CommentCreate($input: CommentCreateInput!) {
  commentCreate(input: $input) { success comment { id } }
}`
	var response struct {
		Data struct {
			CommentCreate struct {
				Success bool `json:"success"`
				Comment struct { ID string `json:"id"` } `json:"comment"`
			} `json:"commentCreate"`
		} `json:"data"`
		Errors []struct { Message string `json:"message"` } `json:"errors"`
	}
	if err := c.graphQL(ctx, query, map[string]any{
		"input": map[string]any{"issueId": issueID, "body": message.Content},
	}, &response); err != nil {
		return err
	}
	if len(response.Errors) != 0 {
		return fmt.Errorf("linear: GraphQL error: %s", response.Errors[0].Message)
	}
	if !response.Data.CommentCreate.Success {
		return errors.New("linear: commentCreate returned success=false")
	}
	return nil
}

func (c *Channel) fetchViewerID(ctx context.Context) (string, error) {
	var response struct {
		Data struct { Viewer struct { ID string `json:"id"` } `json:"viewer"` } `json:"data"`
		Errors []struct { Message string `json:"message"` } `json:"errors"`
	}
	if err := c.graphQL(ctx, "query { viewer { id } }", nil, &response); err != nil {
		return "", err
	}
	if len(response.Errors) != 0 { return "", errors.New(response.Errors[0].Message) }
	if response.Data.Viewer.ID == "" { return "", errors.New("viewer query returned no id") }
	return response.Data.Viewer.ID, nil
}

func (c *Channel) graphQL(ctx context.Context, query string, variables map[string]any, output any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil { return err }
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIBase, bytes.NewReader(body))
	if err != nil { return err }
	req.Header.Set("Authorization", c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil { return err }
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxWebhookBytes+1))
	if err != nil { return err }
	if len(data) > maxWebhookBytes { return errors.New("linear: API response exceeded size limit") }
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("linear: API returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if output != nil && len(data) != 0 {
		if err := json.Unmarshal(data, output); err != nil { return fmt.Errorf("linear: decode API response: %w", err) }
	}
	return nil
}

func (c *Channel) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(c.cfg.WebhookPath, c.handleWebhook)
	return mux
}

type webhookPayload struct {
	Action string `json:"action"`
	Type string `json:"type"`
	WebhookTimestamp int64 `json:"webhookTimestamp"`
	Data struct {
		ID string `json:"id"`
		Body string `json:"body"`
		Issue struct {
			ID string `json:"id"`
			Identifier string `json:"identifier"`
			Title string `json:"title"`
		} `json:"issue"`
		User struct { ID string `json:"id"` } `json:"user"`
		Creator struct { ID string `json:"id"` } `json:"creator"`
	} `json:"data"`
}

func (c *Channel) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBytes))
	if err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if !c.validSignature(body, r.Header.Get("Linear-Signature")) {
		http.Error(w, "Invalid signature", http.StatusUnauthorized)
		return
	}
	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if payload.WebhookTimestamp == 0 || time.Since(time.UnixMilli(payload.WebhookTimestamp)) > maxWebhookAge || time.Until(time.UnixMilli(payload.WebhookTimestamp)) > maxWebhookAge {
		http.Error(w, "Stale webhook", http.StatusUnauthorized)
		return
	}
	deliveryID := strings.TrimSpace(r.Header.Get("Linear-Delivery"))
	if deliveryID == "" || len(deliveryID) > 200 {
		http.Error(w, "Missing delivery id", http.StatusBadRequest)
		return
	}
	if c.wasSeen(deliveryID) {
		w.WriteHeader(http.StatusOK)
		return
	}
	if payload.Type != "Comment" || (payload.Action != "create" && payload.Action != "created") {
		w.WriteHeader(http.StatusOK)
		return
	}
	senderID := payload.Data.User.ID
	if senderID == "" { senderID = payload.Data.Creator.ID }
	if senderID == "" || senderID == c.viewerID || payload.Data.Issue.ID == "" || strings.TrimSpace(payload.Data.Body) == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if !c.IsAllowed(senderID) {
		w.WriteHeader(http.StatusOK)
		return
	}
	content := strings.TrimSpace(payload.Data.Body)
	if payload.Data.Issue.Identifier != "" || payload.Data.Issue.Title != "" {
		content = fmt.Sprintf("[%s] %s\n\n%s",
			strings.TrimSpace(payload.Data.Issue.Identifier),
			strings.TrimSpace(payload.Data.Issue.Title),
			content,
		)
	}
	if err := c.HandleMessage(r.Context(), channels.InboundRequest{
		SenderID: senderID,
		ChatID: payload.Data.Issue.ID,
		Content: content,
		Metadata: map[string]any{
			"delivery_id": deliveryID,
			"comment_id": payload.Data.ID,
			"issue_id": payload.Data.Issue.ID,
			"issue_identifier": payload.Data.Issue.Identifier,
		},
		IsDM: false,
	}); err != nil {
		c.forgetSeen(deliveryID)
		http.Error(w, "Temporary dispatch failure", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (c *Channel) validSignature(body []byte, signature string) bool {
	signature = strings.TrimSpace(signature)
	raw, err := hex.DecodeString(signature)
	if err != nil || len(raw) != sha256.Size { return false }
	mac := hmac.New(sha256.New, []byte(c.cfg.WebhookSigningSecret))
	_, _ = mac.Write(body)
	expected := mac.Sum(nil)
	return subtle.ConstantTimeCompare(raw, expected) == 1
}

func (c *Channel) wasSeen(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.seen[id]; ok { return true }
	c.seen[id] = struct{}{}
	c.seenFIFO = append(c.seenFIFO, id)
	if len(c.seenFIFO) > 10000 {
		delete(c.seen, c.seenFIFO[0])
		c.seenFIFO = c.seenFIFO[1:]
	}
	return false
}

func (c *Channel) forgetSeen(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.seen, id)
	for i, item := range c.seenFIFO {
		if item == id {
			c.seenFIFO = append(c.seenFIFO[:i], c.seenFIFO[i+1:]...)
			break
		}
	}
}
