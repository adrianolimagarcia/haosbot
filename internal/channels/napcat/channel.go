package napcat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

const maxResponseBytes = 1 << 20

// Channel connects to a Napcat OneBot 11 gateway.
type Channel struct {
	*channels.Base
	cfg    Config
	client *http.Client

	mu   sync.Mutex
	conn *websocket.Conn
}

func New(section channels.Section, publisher channels.InboundPublisher) (*Channel, error) {
	cfg, err := sectionConfig(section)
	if err != nil {
		return nil, err
	}
	channel := &Channel{
		cfg: cfg,
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	channel.Base = channels.NewBase(channel, section, publisher,
		channels.WithName(ChannelName), channels.WithDisplayName("Napcat / OneBot"))
	return channel, nil
}

func (c *Channel) Start(ctx context.Context) error {
	if c.cfg.WebSocketURL == "" || c.cfg.APIBase == "" || c.cfg.AccessToken == "" {
		return errors.New("napcat: websocketUrl, apiBase and accessToken are required")
	}
	c.SetRunning(true)
	defer c.SetRunning(false)

	backoff := time.Second
	for c.IsRunning() && ctx.Err() == nil {
		if err := c.runSocket(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.Logger().Warn("Napcat WebSocket disconnected; reconnecting", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
	return nil
}

func (c *Channel) Stop(context.Context) error {
	c.SetRunning(false)
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn != nil {
		_ = conn.CloseNow()
	}
	return nil
}

func (c *Channel) Send(ctx context.Context, message core.OutboundMessage) error {
	kind, targetID, ok := splitChatID(message.ChatID)
	if !ok {
		return errors.New("napcat: chat id must start with private: or group:")
	}
	payload := map[string]any{
		"message_type": kind,
		"message":      message.Content,
	}
	if kind == "private" {
		payload["user_id"] = targetID
	} else {
		payload["group_id"] = targetID
	}
	return c.apiCall(ctx, "send_msg", payload)
}

func (c *Channel) runSocket(ctx context.Context) error {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.cfg.AccessToken)
	conn, _, err := websocket.Dial(ctx, c.cfg.WebSocketURL, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return err
	}
	conn.SetReadLimit(maxResponseBytes)
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.mu.Unlock()
		_ = conn.CloseNow()
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var event oneBotEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return fmt.Errorf("decode OneBot event: %w", err)
		}
		if err := c.handleEvent(ctx, event); err != nil {
			c.Logger().Warn("OneBot message dispatch failed", "error", err)
		}
	}
}

type oneBotEvent struct {
	PostType    string          `json:"post_type"`
	MessageType string          `json:"message_type"`
	SelfID      json.Number     `json:"self_id"`
	UserID      json.Number     `json:"user_id"`
	GroupID     json.Number     `json:"group_id"`
	MessageID   json.Number     `json:"message_id"`
	RawMessage  string          `json:"raw_message"`
	Message     json.RawMessage `json:"message"`
}

func (c *Channel) handleEvent(ctx context.Context, event oneBotEvent) error {
	if event.PostType != "message" || (event.MessageType != "private" && event.MessageType != "group") {
		return nil
	}
	userID := event.UserID.String()
	if userID == "" || userID == c.cfg.SelfID {
		return nil
	}
	var chatID string
	if event.MessageType == "private" {
		chatID = "private:" + userID
	} else {
		groupID := event.GroupID.String()
		if groupID == "" || !c.allowedGroup(groupID) {
			return nil
		}
		chatID = "group:" + groupID
	}
	content := event.RawMessage
	if len(event.Message) != 0 {
		if text := textFromSegments(event.Message); text != "" {
			content = text
		}
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	return c.HandleMessage(ctx, channels.InboundRequest{
		SenderID: userID,
		ChatID:   chatID,
		Content:  content,
		Metadata: map[string]any{"message_id": event.MessageID.String(), "group_id": event.GroupID.String()},
		IsDM:     event.MessageType == "private",
	})
}

func textFromSegments(raw json.RawMessage) string {
	var text string
	var segments []struct {
		Type string `json:"type"`
		Data struct {
			Text string `json:"text"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &segments); err != nil {
		return ""
	}
	for _, segment := range segments {
		if segment.Type == "text" {
			text += segment.Data.Text
		}
	}
	return text
}

func splitChatID(chatID string) (kind, target string, ok bool) {
	kind, target, found := strings.Cut(chatID, ":")
	if !found || target == "" || (kind != "private" && kind != "group") {
		return "", "", false
	}
	return kind, target, true
}

func (c *Channel) allowedGroup(groupID string) bool {
	if len(c.cfg.AllowedGroups) == 0 {
		return true
	}
	for _, allowed := range c.cfg.AllowedGroups {
		if groupID == allowed {
			return true
		}
	}
	return false
}

func (c *Channel) apiCall(ctx context.Context, method string, input any) error {
	encoded, err := json.Marshal(input)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIBase+"/"+method, bytes.NewReader(encoded))
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
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxResponseBytes {
		return errors.New("Napcat response exceeded size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Napcat API returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	var result struct {
		RetCode int `json:"retcode"`
		Message string `json:"message"`
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &result); err != nil {
			return fmt.Errorf("decode Napcat response: %w", err)
		}
	}
	if result.RetCode != 0 {
		return fmt.Errorf("Napcat API error %d: %s", result.RetCode, result.Message)
	}
	return nil
}
