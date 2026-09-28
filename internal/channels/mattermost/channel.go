package mattermost

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

const maxResponseBytes = 1 << 20

// Channel receives Mattermost post events over the authenticated event socket.
type Channel struct {
	*channels.Base
	cfg    Config
	client *http.Client
	userID string

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
		channels.WithName(ChannelName), channels.WithDisplayName("Mattermost"))
	return channel, nil
}

func (c *Channel) Start(ctx context.Context) error {
	if c.cfg.ServerURL == "" || c.cfg.BotToken == "" {
		return errors.New("mattermost: serverUrl and botToken are required")
	}
	if err := c.verifyBot(ctx); err != nil {
		return fmt.Errorf("mattermost: bot authentication failed: %w", err)
	}
	c.SetRunning(true)
	defer c.SetRunning(false)

	backoff := time.Second
	for c.IsRunning() && ctx.Err() == nil {
		if err := c.runSocket(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.Logger().Warn("Mattermost WebSocket disconnected; reconnecting", "error", err)
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
	channelID, rootID := splitChatID(message.ChatID)
	if channelID == "" {
		return errors.New("mattermost: channel id is required")
	}
	payload := map[string]any{"channel_id": channelID, "message": message.Content}
	if rootID != "" {
		payload["root_id"] = rootID
	}
	return c.apiCall(ctx, http.MethodPost, "/api/v4/posts", payload, nil)
}

func (c *Channel) verifyBot(ctx context.Context) error {
	var user struct {
		ID string `json:"id"`
	}
	if err := c.apiCall(ctx, http.MethodGet, "/api/v4/users/me", nil, &user); err != nil {
		return err
	}
	if user.ID == "" {
		return errors.New("Mattermost /users/me returned no user ID")
	}
	c.userID = user.ID
	return nil
}

func (c *Channel) runSocket(ctx context.Context) error {
	wsURL, err := websocketURL(c.cfg.ServerURL)
	if err != nil {
		return err
	}
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
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

	challenge, _ := json.Marshal(map[string]any{
		"seq": 1,
		"action": "authentication_challenge",
		"data": map[string]string{"token": c.cfg.BotToken},
	})
	if err := conn.Write(ctx, websocket.MessageText, challenge); err != nil {
		return fmt.Errorf("send Mattermost authentication challenge: %w", err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	var response struct {
		Status   string `json:"status"`
		SeqReply int    `json:"seq_reply"`
	}
	if err := json.Unmarshal(data, &response); err != nil || response.Status != "OK" || response.SeqReply != 1 {
		return errors.New("Mattermost rejected the WebSocket authentication challenge")
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var event websocketEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return fmt.Errorf("decode Mattermost event: %w", err)
		}
		if event.Event != "posted" {
			continue
		}
		post, err := parsePost(event.Data.Post)
		if err != nil {
			c.Logger().Warn("could not decode Mattermost post", "error", err)
			continue
		}
		if err := c.handlePost(ctx, post); err != nil {
			c.Logger().Warn("Mattermost post dispatch failed", "error", err)
		}
	}
}

func websocketURL(serverURL string) (string, error) {
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "https" {
		parsed.Scheme = "wss"
	} else if parsed.Scheme == "http" {
		parsed.Scheme = "ws"
	} else {
		return "", errors.New("Mattermost server URL must use http or https")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/api/v4/websocket"
	return parsed.String(), nil
}

type websocketEvent struct {
	Event string `json:"event"`
	Data  struct {
		Post json.RawMessage `json:"post"`
	} `json:"data"`
}

type mattermostPost struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	ChannelID string `json:"channel_id"`
	Message   string `json:"message"`
	RootID    string `json:"root_id"`
}

func parsePost(raw json.RawMessage) (mattermostPost, error) {
	var post mattermostPost
	if err := json.Unmarshal(raw, &post); err == nil && post.ID != "" {
		return post, nil
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return mattermostPost{}, err
	}
	if err := json.Unmarshal([]byte(encoded), &post); err != nil {
		return mattermostPost{}, err
	}
	return post, nil
}

func (c *Channel) handlePost(ctx context.Context, post mattermostPost) error {
	if post.ID == "" || post.UserID == "" || post.UserID == c.userID || post.ChannelID == "" || strings.TrimSpace(post.Message) == "" {
		return nil
	}
	if !c.allowedSender(post.UserID) {
		return nil
	}
	if !c.allowedChannel(post.ChannelID) {
		return nil
	}
	rootID := post.RootID
	if rootID == "" {
		rootID = post.ID
	}
	return c.HandleMessage(ctx, channels.InboundRequest{
		SenderID: post.UserID,
		ChatID:   joinChatID(post.ChannelID, rootID),
		Content:  strings.TrimSpace(post.Message),
		Metadata: map[string]any{"post_id": post.ID, "root_id": rootID},
		IsDM:     false,
	})
}

func (c *Channel) allowedSender(senderID string) bool {
	if len(c.cfg.AllowFrom) == 0 {
		return true
	}
	for _, allowed := range c.cfg.AllowFrom {
		if senderID == allowed {
			return true
		}
	}
	return false
}

func joinChatID(channelID, rootID string) string {
	if rootID == "" {
		return channelID
	}
	return channelID + "|" + rootID
}

func splitChatID(chatID string) (string, string) {
	channelID, rootID, _ := strings.Cut(chatID, "|")
	return channelID, rootID
}

func (c *Channel) allowedChannel(channelID string) bool {
	if len(c.cfg.AllowedChannels) == 0 {
		return true
	}
	for _, allowed := range c.cfg.AllowedChannels {
		if channelID == allowed {
			return true
		}
	}
	return false
}

func (c *Channel) apiCall(ctx context.Context, method, path string, input any, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.cfg.ServerURL+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.cfg.BotToken)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
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
		return errors.New("Mattermost response exceeded size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Mattermost API returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	if output != nil && len(data) > 0 {
		if err := json.Unmarshal(data, output); err != nil {
			return fmt.Errorf("decode Mattermost response: %w", err)
		}
	}
	return nil
}
