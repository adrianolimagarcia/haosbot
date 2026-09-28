package discord

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
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

const (
	defaultIntents   = 1 | (1 << 9) | (1 << 12) | (1 << 15) // GUILDS, GUILD_MESSAGES, DIRECT_MESSAGES, MESSAGE_CONTENT
	maxResponseBytes = 1 << 20
)

// Channel connects to the Discord Gateway and sends messages through REST.
type Channel struct {
	*channels.Base
	cfg      Config
	client   *http.Client
	apiBase  string
	gateway  string
	botID    string
	sequence atomic.Int64

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
		apiBase: "https://discord.com/api/v10",
		gateway: "https://discord.com/api/v10/gateway/bot",
	}
	channel.sequence.Store(-1)
	channel.Base = channels.NewBase(channel, section, publisher,
		channels.WithName(ChannelName), channels.WithDisplayName("Discord"))
	return channel, nil
}

func (c *Channel) Start(ctx context.Context) error {
	if c.cfg.BotToken == "" {
		return errors.New("discord: botToken is required")
	}
	if err := c.verifyBot(ctx); err != nil {
		return fmt.Errorf("discord: bot authentication failed: %w", err)
	}
	c.SetRunning(true)
	defer c.SetRunning(false)

	backoff := time.Second
	for c.IsRunning() && ctx.Err() == nil {
		if err := c.runGateway(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.Logger().Warn("Discord Gateway disconnected; reconnecting", "error", err)
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
	if message.ChatID == "" {
		return errors.New("discord: channel id is required")
	}
	if !utf8.ValidString(message.Content) {
		return errors.New("discord: message is not valid UTF-8")
	}
	endpoint := c.apiBase + "/channels/" + url.PathEscape(message.ChatID) + "/messages"
	payload := map[string]any{"content": message.Content}
	if message.ReplyTo != nil && *message.ReplyTo != "" {
		payload["message_reference"] = map[string]string{"message_id": *message.ReplyTo}
	}
	return c.apiCall(ctx, http.MethodPost, endpoint, payload, nil)
}

func (c *Channel) verifyBot(ctx context.Context) error {
	var user struct {
		ID  string `json:"id"`
		Bot bool   `json:"bot"`
	}
	if err := c.apiCall(ctx, http.MethodGet, c.apiBase+"/users/@me", nil, &user); err != nil {
		return err
	}
	if user.ID == "" || !user.Bot {
		return errors.New("configured account is not a Discord bot")
	}
	c.botID = user.ID
	return nil
}

func (c *Channel) runGateway(ctx context.Context) error {
	var response struct {
		URL string `json:"url"`
	}
	if err := c.apiCall(ctx, http.MethodGet, c.gateway, nil, &response); err != nil {
		return err
	}
	if response.URL == "" {
		return errors.New("gateway/bot returned no URL")
	}
	gatewayURL, err := url.Parse(response.URL)
	if err != nil || (gatewayURL.Scheme != "wss" && gatewayURL.Scheme != "ws") || gatewayURL.Host == "" {
		return errors.New("gateway/bot returned an invalid WebSocket URL")
	}
	query := gatewayURL.Query()
	query.Set("v", "10")
	query.Set("encoding", "json")
	gatewayURL.RawQuery = query.Encode()
	c.sequence.Store(-1)
	conn, _, err := websocket.Dial(ctx, gatewayURL.String(), nil)
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

	_, data, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	var hello gatewayPayload
	if err := json.Unmarshal(data, &hello); err != nil || hello.Op != 10 {
		return errors.New("Discord Gateway did not send a valid Hello payload")
	}
	var helloData struct {
		HeartbeatInterval int `json:"heartbeat_interval"`
	}
	if err := json.Unmarshal(hello.Data, &helloData); err != nil || helloData.HeartbeatInterval <= 0 {
		return errors.New("Discord Gateway Hello has no heartbeat interval")
	}
	identify, _ := json.Marshal(map[string]any{
		"op": 2,
		"d": map[string]any{
			"token":   c.cfg.BotToken,
			"intents": defaultIntents,
			"properties": map[string]string{
				"os": "haosbot", "browser": "haosbot", "device": "haosbot",
			},
		},
	})
	if err := conn.Write(ctx, websocket.MessageText, identify); err != nil {
		return err
	}

	heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
	heartbeatNow := make(chan struct{}, 1)
	var acknowledged atomic.Bool
	acknowledged.Store(true)
	heartbeatDone := make(chan struct{})
	go c.heartbeat(heartbeatCtx, conn, helloData.HeartbeatInterval, heartbeatNow, &acknowledged, heartbeatDone)
	defer func() {
		cancelHeartbeat()
		<-heartbeatDone
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var payload gatewayPayload
		if err := json.Unmarshal(data, &payload); err != nil {
			return fmt.Errorf("decode Discord Gateway payload: %w", err)
		}
		if payload.Sequence != nil {
			c.sequence.Store(*payload.Sequence)
		}
		switch payload.Op {
		case 0:
			if payload.Type == "MESSAGE_CREATE" {
				var message discordMessage
				if err := json.Unmarshal(payload.Data, &message); err != nil {
					return fmt.Errorf("decode Discord message: %w", err)
				}
				if err := c.handleMessage(ctx, message); err != nil {
					c.Logger().Warn("Discord message dispatch failed", "error", err)
				}
			}
		case 1:
			select {
			case heartbeatNow <- struct{}{}:
			default:
			}
		case 7, 9:
			return fmt.Errorf("Discord requested gateway reconnect (op %d)", payload.Op)
		case 11:
			acknowledged.Store(true)
		}
	}
}

func (c *Channel) heartbeat(ctx context.Context, conn *websocket.Conn, intervalMS int, immediate <-chan struct{}, acknowledged *atomic.Bool, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(time.Duration(intervalMS) * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-immediate:
		}
		if !acknowledged.Load() {
			_ = conn.CloseNow()
			return
		}
		acknowledged.Store(false)
		sequence := c.sequence.Load()
		var data any
		if sequence >= 0 {
			data = sequence
		}
		encoded, _ := json.Marshal(map[string]any{"op": 1, "d": data})
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := conn.Write(writeCtx, websocket.MessageText, encoded)
		cancel()
		if err != nil {
			_ = conn.CloseNow()
			return
		}
	}
}

type gatewayPayload struct {
	Op       int             `json:"op"`
	Sequence *int64          `json:"s"`
	Type     string          `json:"t"`
	Data     json.RawMessage `json:"d"`
}

type discordMessage struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`
	Content   string `json:"content"`
	Author    struct {
		ID  string `json:"id"`
		Bot bool   `json:"bot"`
	} `json:"author"`
}

func (c *Channel) handleMessage(ctx context.Context, message discordMessage) error {
	if message.Author.Bot || message.Author.ID == "" || message.Author.ID == c.botID || message.ChannelID == "" {
		return nil
	}
	if !c.allowedChannel(message.ChannelID) {
		return nil
	}
	if strings.TrimSpace(message.Content) == "" {
		return nil
	}
	return c.HandleMessage(ctx, channels.InboundRequest{
		SenderID: message.Author.ID,
		ChatID:   message.ChannelID,
		Content:  message.Content,
		Metadata: map[string]any{"message_id": message.ID, "guild_id": message.GuildID},
		IsDM:     message.GuildID == "",
	})
}

func (c *Channel) allowedChannel(channelID string) bool {
	if len(c.cfg.AllowedChannels) == 0 {
		return true
	}
	for _, allowed := range c.cfg.AllowedChannels {
		if allowed == channelID {
			return true
		}
	}
	return false
}

func (c *Channel) apiCall(ctx context.Context, method, endpoint string, input any, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bot "+c.cfg.BotToken)
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
		return errors.New("Discord response exceeded size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Discord API returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	if output == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode Discord response: %w", err)
	}
	return nil
}
