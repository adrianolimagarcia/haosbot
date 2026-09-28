package whatsapp

import (
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
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

const maxWebhookBytes = 2 << 20

// Channel integrates the official WhatsApp Cloud API. Meta delivers inbound
// messages to the configured webhook, while HAOSbot sends text through Graph.
type Channel struct {
	*channels.Base
	cfg    Config
	client *http.Client

	mu       sync.Mutex
	server   *http.Server
	listener net.Listener
	seen     map[string]struct{}
	seenFIFO []string
}

func New(section channels.Section, publisher channels.InboundPublisher) (*Channel, error) {
	cfg, err := sectionConfig(section)
	if err != nil {
		return nil, err
	}
	c := &Channel{
		cfg:    cfg,
		client: &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		seen:   make(map[string]struct{}),
	}
	c.Base = channels.NewBase(c, section, publisher,
		channels.WithName(ChannelName), channels.WithDisplayName("WhatsApp Cloud API"))
	return c, nil
}

func (c *Channel) Start(ctx context.Context) error {
	if c.cfg.PhoneNumberID == "" || c.cfg.AccessToken == "" || c.cfg.AppSecret == "" || c.cfg.VerifyToken == "" || c.cfg.GraphVersion == "" {
		return errors.New("whatsapp: phoneNumberId, accessToken, appSecret, verifyToken and graphVersion are required")
	}
	listener, err := net.Listen("tcp", c.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("whatsapp: listen: %w", err)
	}
	server := &http.Server{
		Handler:           c.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
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
	shutdownDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdownCtx)
		case <-shutdownDone:
		}
	}()
	err = server.Serve(listener)
	close(shutdownDone)
	if errors.Is(err, http.ErrServerClosed) || ctx.Err() != nil {
		return nil
	}
	return err
}

func (c *Channel) Stop(ctx context.Context) error {
	c.SetRunning(false)
	c.mu.Lock()
	server := c.server
	c.mu.Unlock()
	if server == nil {
		return nil
	}
	return server.Shutdown(ctx)
}

func (c *Channel) Send(ctx context.Context, message core.OutboundMessage) error {
	if strings.TrimSpace(message.ChatID) == "" {
		return errors.New("whatsapp: recipient is required")
	}
	base, err := url.Parse(c.cfg.GraphBase)
	if err != nil {
		return err
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" + c.cfg.GraphVersion + "/" + url.PathEscape(c.cfg.PhoneNumberID) + "/messages"
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"recipient_type":   "individual",
		"to":                message.ChatID,
		"type":              "text",
		"text":              map[string]any{"preview_url": false, "body": message.Content},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String(), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.cfg.AccessToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxWebhookBytes+1))
	if err != nil {
		return err
	}
	if len(responseBody) > maxWebhookBytes {
		return errors.New("whatsapp: Graph API response exceeded size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("whatsapp: Graph API returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	return nil
}

func (c *Channel) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(c.cfg.WebhookPath, c.handleWebhook)
	return mux
}

func (c *Channel) handleWebhook(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		c.verifyWebhook(w, r)
	case http.MethodPost:
		c.receiveWebhook(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (c *Channel) verifyWebhook(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	mode := query.Get("hub.mode")
	token := query.Get("hub.verify_token")
	if mode != "subscribe" || subtle.ConstantTimeCompare([]byte(token), []byte(c.cfg.VerifyToken)) != 1 {
		http.Error(w, "Webhook verification failed", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, query.Get("hub.challenge"))
}

func (c *Channel) receiveWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBytes))
	if err != nil {
		http.Error(w, "Webhook payload too large or unreadable", http.StatusBadRequest)
		return
	}
	if !c.validSignature(body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "Invalid webhook signature", http.StatusUnauthorized)
		return
	}
	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "Invalid webhook payload", http.StatusBadRequest)
		return
	}
	if payload.Object != "whatsapp_business_account" {
		w.WriteHeader(http.StatusOK)
		return
	}
	dispatchFailed := false
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			if change.Value.Metadata.PhoneNumberID != "" && change.Value.Metadata.PhoneNumberID != c.cfg.PhoneNumberID {
				continue
			}
			for _, message := range change.Value.Messages {
				if message.ID == "" || message.From == "" || c.wasSeen(message.ID) {
					continue
				}
				content := message.textContent()
				if content == "" || !c.allowedSender(message.From) {
					continue
				}
				if err := c.HandleMessage(r.Context(), channels.InboundRequest{
					SenderID: message.From, ChatID: message.From, Content: content,
					Metadata: map[string]any{"message_id": message.ID, "phone_number_id": change.Value.Metadata.PhoneNumberID}, IsDM: true,
				}); err != nil {
					c.forgetSeen(message.ID)
					dispatchFailed = true
					c.Logger().Warn("WhatsApp webhook message dispatch failed", "error", err)
				}
			}
		}
	}
	if dispatchFailed {
		http.Error(w, "Temporary message dispatch failure", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (c *Channel) validSignature(body []byte, signature string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return false
	}
	supplied, err := hex.DecodeString(strings.TrimPrefix(signature, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(c.cfg.AppSecret))
	_, _ = mac.Write(body)
	return hmac.Equal(supplied, mac.Sum(nil))
}

func (c *Channel) allowedSender(sender string) bool {
	if len(c.cfg.AllowFrom) == 0 {
		return true
	}
	for _, allowed := range c.cfg.AllowFrom {
		if sender == allowed {
			return true
		}
	}
	return false
}

func (c *Channel) wasSeen(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.seen[id]; exists {
		return true
	}
	c.seen[id] = struct{}{}
	c.seenFIFO = append(c.seenFIFO, id)
	if len(c.seenFIFO) > 10_000 {
		delete(c.seen, c.seenFIFO[0])
		c.seenFIFO = c.seenFIFO[1:]
	}
	return false
}

func (c *Channel) forgetSeen(id string) {
	c.mu.Lock()
	delete(c.seen, id)
	for i, seenID := range c.seenFIFO {
		if seenID == id {
			c.seenFIFO = append(c.seenFIFO[:i], c.seenFIFO[i+1:]...)
			break
		}
	}
	c.mu.Unlock()
}

type webhookPayload struct {
	Object string `json:"object"`
	Entry  []struct {
		Changes []struct {
			Value struct {
				Metadata struct {
					PhoneNumberID string `json:"phone_number_id"`
				} `json:"metadata"`
				Messages []inboundMessage `json:"messages"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

type inboundMessage struct {
	ID   string `json:"id"`
	From string `json:"from"`
	Type string `json:"type"`
	Text struct {
		Body string `json:"body"`
	} `json:"text"`
	Interactive struct {
		ButtonReply struct {
			Title string `json:"title"`
		} `json:"button_reply"`
		ListReply struct {
			Title string `json:"title"`
		} `json:"list_reply"`
	} `json:"interactive"`
}

func (m inboundMessage) textContent() string {
	switch m.Type {
	case "text":
		return strings.TrimSpace(m.Text.Body)
	case "interactive":
		if m.Interactive.ButtonReply.Title != "" {
			return strings.TrimSpace(m.Interactive.ButtonReply.Title)
		}
		return strings.TrimSpace(m.Interactive.ListReply.Title)
	default:
		return ""
	}
}
