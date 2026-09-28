package slack

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

// Channel connects to Slack Socket Mode and posts replies through the Web API.
type Channel struct {
	*channels.Base
	cfg     Config
	client  *http.Client
	apiBase  string
	botUserID string

	mu   sync.Mutex
	conn *websocket.Conn
}

func New(section channels.Section, publisher channels.InboundPublisher) (*Channel, error) {
	cfg, err := sectionConfig(section)
	if err != nil {
		return nil, err
	}
	channel := &Channel{
		cfg:     cfg,
		client:  &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		apiBase: "https://slack.com/api/",
	}
	channel.Base = channels.NewBase(channel, section, publisher,
		channels.WithName(ChannelName), channels.WithDisplayName("Slack"))
	return channel, nil
}

func (c *Channel) Start(ctx context.Context) error {
	if c.cfg.BotToken == "" || c.cfg.AppToken == "" {
		return errors.New("slack: botToken and appToken are required")
	}
	if err := c.verifyBot(ctx); err != nil {
		return fmt.Errorf("slack: bot authentication failed: %w", err)
	}
	c.SetRunning(true)
	defer c.SetRunning(false)

	backoff := time.Second
	for c.IsRunning() && ctx.Err() == nil {
		if err := c.runSocket(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.Logger().Warn("Slack Socket Mode disconnected; reconnecting", "error", err)
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
	channelID, threadTS := splitChatID(message.ChatID)
	if channelID == "" {
		return errors.New("slack: channel id is required")
	}
	payload := map[string]any{"channel": channelID, "text": message.Content}
	if threadTS != "" {
		payload["thread_ts"] = threadTS
	}
	return c.apiCall(ctx, c.cfg.BotToken, "chat.postMessage", payload, nil)
}

func (c *Channel) verifyBot(ctx context.Context) error {
	var response struct {
		UserID string `json:"user_id"`
	}
	if err := c.apiCall(ctx, c.cfg.BotToken, "auth.test", nil, &response); err != nil {
		return err
	}
	if response.UserID == "" {
		return errors.New("auth.test returned no bot user id")
	}
	c.botUserID = response.UserID
	return nil
}

func (c *Channel) runSocket(ctx context.Context) error {
	var response struct {
		URL string `json:"url"`
	}
	if err := c.apiCall(ctx, c.cfg.AppToken, "apps.connections.open", nil, &response); err != nil {
		return err
	}
	if response.URL == "" {
		return errors.New("apps.connections.open returned no socket URL")
	}
	conn, _, err := websocket.Dial(ctx, response.URL, nil)
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
		var envelope socketEnvelope
		if err := json.Unmarshal(data, &envelope); err != nil {
			return fmt.Errorf("decode Socket Mode envelope: %w", err)
		}
		if envelope.EnvelopeID != "" {
			ack, _ := json.Marshal(map[string]string{"envelope_id": envelope.EnvelopeID})
			if err := conn.Write(ctx, websocket.MessageText, ack); err != nil {
				return fmt.Errorf("ack Slack event: %w", err)
			}
		}
		if envelope.Type == "events_api" {
			if err := c.handleEvent(ctx, envelope.Payload.Event); err != nil {
				c.Logger().Warn("Slack event dispatch failed", "error", err)
			}
		}
	}
}

type socketEnvelope struct {
	EnvelopeID string `json:"envelope_id"`
	Type       string `json:"type"`
	Payload    struct {
		Event slackEvent `json:"event"`
	} `json:"payload"`
}

type slackEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	User      string `json:"user"`
	Channel   string `json:"channel"`
	Text      string `json:"text"`
	TS        string `json:"ts"`
	ThreadTS  string `json:"thread_ts"`
	BotID     string `json:"bot_id"`
}

func (c *Channel) handleEvent(ctx context.Context, event slackEvent) error {
	if event.Type != "message" || event.Subtype != "" || event.BotID != "" || event.User == "" || event.User == c.botUserID || event.Channel == "" {
		return nil
	}
	if !c.allowedRoom(event.Channel) {
		return nil
	}
	content := strings.TrimSpace(event.Text)
	if content == "" {
		return nil
	}
	threadTS := event.ThreadTS
	if threadTS == "" {
		threadTS = event.TS
	}
	chatID := joinChatID(event.Channel, threadTS)
	return c.HandleMessage(ctx, channels.InboundRequest{
		SenderID: event.User,
		ChatID:   chatID,
		Content:  content,
		Metadata: map[string]any{"channel_id": event.Channel, "thread_ts": threadTS},
		IsDM:     false,
	})
}

func (c *Channel) allowedRoom(channelID string) bool {
	if len(c.cfg.AllowedRooms) == 0 {
		return true
	}
	for _, allowed := range c.cfg.AllowedRooms {
		if channelID == allowed {
			return true
		}
	}
	return false
}

func joinChatID(channelID, threadTS string) string {
	if threadTS == "" {
		return channelID
	}
	return channelID + "|" + threadTS
}

func splitChatID(chatID string) (string, string) {
	channelID, threadTS, found := strings.Cut(chatID, "|")
	if !found {
		return channelID, ""
	}
	return channelID, threadTS
}

func (c *Channel) apiCall(ctx context.Context, token, method string, input any, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase+method, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
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
		return errors.New("Slack response exceeded size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Slack API returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	var envelope struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("decode Slack API response: %w", err)
	}
	if !envelope.OK {
		return fmt.Errorf("Slack API error: %s", envelope.Error)
	}
	if output != nil {
		if err := json.Unmarshal(data, output); err != nil {
			return fmt.Errorf("decode Slack response: %w", err)
		}
	}
	return nil
}
