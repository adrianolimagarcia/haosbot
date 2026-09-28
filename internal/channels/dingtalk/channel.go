package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

const (
	connectionPath = "/v1.0/gateway/connections/open"
	botMessageTopic = "/v1.0/im/bot/messages/get"
	maxFrameBytes = 4 << 20
)

type sessionRef struct {
	Webhook string
	ExpiresAt time.Time
}

type Channel struct {
	*channels.Base
	cfg Config
	client *http.Client

	mu sync.RWMutex
	conn *websocket.Conn
	refs map[string]sessionRef
}

func New(section channels.Section, publisher channels.InboundPublisher) (*Channel, error) {
	cfg, err := sectionConfig(section)
	if err != nil { return nil, err }
	c := &Channel{
		cfg: cfg,
		client: &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		refs: make(map[string]sessionRef),
	}
	c.Base = channels.NewBase(c, section, publisher,
		channels.WithName(ChannelName), channels.WithDisplayName("DingTalk"))
	return c, nil
}

func (c *Channel) ProgressTransportDefaults() (bool, bool, bool) { return false, false, true }

func (c *Channel) Start(ctx context.Context) error {
	if err := c.cfg.validateRuntime(); err != nil { return err }
	c.SetRunning(true)
	defer c.SetRunning(false)

	backoff := time.Second
	for ctx.Err() == nil && c.IsRunning() {
		err := c.connectAndRun(ctx)
		if ctx.Err() != nil || !c.IsRunning() { return nil }
		if err != nil { c.Logger().Warn("DingTalk stream disconnected", "error", err) }
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second { backoff = 30*time.Second }
		}
	}
	return nil
}

func (c *Channel) Stop(context.Context) error {
	c.SetRunning(false)
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn != nil { return conn.Close(websocket.StatusNormalClosure, "channel stopping") }
	return nil
}

func (c *Channel) Send(ctx context.Context, msg core.OutboundMessage) error {
	content := strings.TrimSpace(msg.Content)
	if content == "" { return nil }
	c.mu.RLock()
	ref, ok := c.refs[msg.ChatID]
	c.mu.RUnlock()
	if !ok || ref.Webhook == "" {
		return fmt.Errorf("dingtalk: no active session webhook for chat %q; wait for a new inbound message", msg.ChatID)
	}
	if !ref.ExpiresAt.IsZero() && time.Now().After(ref.ExpiresAt) {
		return fmt.Errorf("dingtalk: session webhook expired for chat %q", msg.ChatID)
	}
	if !trustedSessionWebhook(ref.Webhook) {
		return errors.New("dingtalk: refusing untrusted session webhook")
	}
	body, _ := json.Marshal(map[string]any{
		"msgtype": "text",
		"text": map[string]string{"content": content},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ref.Webhook, bytes.NewReader(body))
	if err != nil { return err }
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil { return err }
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("dingtalk: session webhook HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

type endpointRequest struct {
	ClientID string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	Subscriptions []subscription `json:"subscriptions"`
	UserAgent string `json:"ua"`
	Extras map[string]string `json:"extras"`
}
type subscription struct { Type string `json:"type"`; Topic string `json:"topic"` }
type endpointResponse struct { Endpoint string `json:"endpoint"`; Ticket string `json:"ticket"` }

type dataFrame struct {
	SpecVersion string `json:"specVersion"`
	Type string `json:"type"`
	Time int64 `json:"time"`
	Headers map[string]string `json:"headers"`
	Data string `json:"data"`
}
type dataFrameResponse struct {
	Code int `json:"code"`
	Headers map[string]string `json:"headers"`
	Message string `json:"message"`
	Data string `json:"data"`
}

type botCallback struct {
	ConversationID string `json:"conversationId"`
	MsgID string `json:"msgId"`
	SenderNick string `json:"senderNick"`
	SenderStaffID string `json:"senderStaffId"`
	SenderID string `json:"senderId"`
	SessionWebhookExpiredTime int64 `json:"sessionWebhookExpiredTime"`
	ConversationType string `json:"conversationType"`
	SessionWebhook string `json:"sessionWebhook"`
	Msgtype string `json:"msgtype"`
	Text struct{ Content string `json:"content"` } `json:"text"`
}

func (c *Channel) connectAndRun(ctx context.Context) error {
	endpoint, err := c.getEndpoint(ctx)
	if err != nil { return err }
	u, err := url.Parse(endpoint.Endpoint)
	if err != nil || (u.Scheme != "wss" && u.Scheme != "ws") || u.Host == "" {
		return errors.New("dingtalk: invalid stream endpoint")
	}
	q := u.Query()
	q.Set("ticket", endpoint.Ticket)
	u.RawQuery = q.Encode()

	conn, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPClient: c.client,
	})
	if resp != nil && resp.Body != nil { _ = resp.Body.Close() }
	if err != nil { return fmt.Errorf("dingtalk: websocket dial: %w", err) }
	conn.SetReadLimit(maxFrameBytes)
	c.mu.Lock()
	if c.conn != nil { _ = c.conn.CloseNow() }
	c.conn = conn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.conn == conn { c.conn = nil }
		c.mu.Unlock()
		_ = conn.CloseNow()
	}()

	for {
		_, raw, err := conn.Read(ctx)
		if err != nil { return err }
		var frame dataFrame
		if err := json.Unmarshal(raw, &frame); err != nil {
			c.Logger().Warn("DingTalk frame decode failed", "error", err)
			continue
		}
		if frame.Headers == nil { frame.Headers = map[string]string{} }
		resp := dataFrameResponse{
			Code: 200,
			Headers: map[string]string{
				"contentType": "application/json",
				"messageId": frame.Headers["messageId"],
			},
			Message: "ok",
		}
		switch {
		case frame.Type == "SYSTEM" && frame.Headers["topic"] == "ping":
			resp.Data = frame.Data
		case frame.Type == "SYSTEM" && frame.Headers["topic"] == "disconnect":
			if err := c.writeAck(ctx, conn, resp); err != nil { return err }
			return errors.New("dingtalk: server requested disconnect")
		case frame.Type == "CALLBACK" && frame.Headers["topic"] == botMessageTopic:
			if err := c.handleBotCallback(ctx, frame.Data); err != nil {
				resp.Code = 500
				resp.Message = err.Error()
			}
		default:
			resp.Code = 404
			resp.Message = "handler not found"
		}
		if err := c.writeAck(ctx, conn, resp); err != nil { return err }
	}
}

func (c *Channel) writeAck(parent context.Context, conn *websocket.Conn, resp dataFrameResponse) error {
	raw, err := json.Marshal(resp)
	if err != nil { return err }
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, raw)
}

func (c *Channel) getEndpoint(ctx context.Context) (endpointResponse, error) {
	input := endpointRequest{
		ClientID: c.cfg.ClientID,
		ClientSecret: c.cfg.ClientSecret,
		UserAgent: "haosbot/dingtalk-go",
		Subscriptions: []subscription{
			{Type: "SYSTEM", Topic: "ping"},
			{Type: "SYSTEM", Topic: "disconnect"},
			{Type: "CALLBACK", Topic: botMessageTopic},
		},
		Extras: map[string]string{},
	}
	body, _ := json.Marshal(input)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.OpenAPIHost+connectionPath, bytes.NewReader(body))
	if err != nil { return endpointResponse{}, err }
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil { return endpointResponse{}, err }
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil { return endpointResponse{}, err }
	if resp.StatusCode != http.StatusOK {
		return endpointResponse{}, fmt.Errorf("dingtalk: endpoint HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out endpointResponse
	if err := json.Unmarshal(data, &out); err != nil { return endpointResponse{}, err }
	if out.Endpoint == "" || out.Ticket == "" { return endpointResponse{}, errors.New("dingtalk: endpoint response missing endpoint/ticket") }
	return out, nil
}

func (c *Channel) handleBotCallback(ctx context.Context, raw string) error {
	var message botCallback
	if err := json.Unmarshal([]byte(raw), &message); err != nil { return err }
	if message.Msgtype != "" && message.Msgtype != "text" { return nil }
	content := strings.TrimSpace(message.Text.Content)
	if content == "" { return nil }
	sender := strings.TrimSpace(message.SenderStaffID)
	if sender == "" { sender = strings.TrimSpace(message.SenderID) }
	if sender == "" || !c.IsAllowed(sender) { return nil }

	isGroup := message.ConversationType == "2" && message.ConversationID != ""
	if !isGroup && c.cfg.DisablePrivateChat { return nil }
	chatID := sender
	if isGroup { chatID = "group:" + message.ConversationID }

	if message.SessionWebhook != "" && trustedSessionWebhook(message.SessionWebhook) {
		expiry := time.Time{}
		if message.SessionWebhookExpiredTime > 0 {
			// DingTalk currently emits epoch milliseconds.
			expiry = time.UnixMilli(message.SessionWebhookExpiredTime)
		}
		c.mu.Lock()
		c.refs[chatID] = sessionRef{Webhook: message.SessionWebhook, ExpiresAt: expiry}
		c.mu.Unlock()
	}
	var sessionOverride *string
	if isGroup && c.cfg.GroupUserIsolation {
		key := ChannelName + ":group:" + message.ConversationID + ":" + sender
		sessionOverride = &key
	}
	return c.HandleMessage(ctx, channels.InboundRequest{
		SenderID: sender,
		ChatID: chatID,
		Content: content,
		Metadata: map[string]any{
			"message_id": message.MsgID,
			"sender_name": message.SenderNick,
			"conversation_type": message.ConversationType,
		},
		IsDM: !isGroup,
		SessionKey: sessionOverride,
	})
}

func trustedSessionWebhook(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil { return false }
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	return host == "dingtalk.com" || strings.HasSuffix(host, ".dingtalk.com")
}
