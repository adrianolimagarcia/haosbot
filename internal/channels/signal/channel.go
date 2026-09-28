package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

const maxMessageBytes = 4 << 20

// Channel adapts the signal-cli-rest-api service to HAOSbot's channel runtime.
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
	c := &Channel{cfg: cfg, client: &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	c.Base = channels.NewBase(c, section, publisher,
		channels.WithName(ChannelName), channels.WithDisplayName("Signal"))
	return c, nil
}

func (c *Channel) Start(ctx context.Context) error {
	if c.cfg.APIBase == "" || c.cfg.Number == "" {
		return errors.New("signal: apiBase and number are required")
	}
	c.SetRunning(true)
	defer c.SetRunning(false)
	backoff := time.Second
	for c.IsRunning() && ctx.Err() == nil {
		if err := c.receive(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.Logger().Warn("Signal receive socket disconnected; reconnecting", "error", err)
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
	if strings.TrimSpace(message.ChatID) == "" {
		return errors.New("signal: recipient is required")
	}
	payload := map[string]any{
		"number": c.cfg.Number,
		"recipients": []string{message.ChatID},
		"message": message.Content,
	}
	return c.apiCall(ctx, http.MethodPost, "/v2/send", payload)
}

func (c *Channel) receive(ctx context.Context) error {
	wsURL, err := receiveURL(c.cfg.APIBase, c.cfg.Number)
	if err != nil {
		return err
	}
	header := http.Header{}
	if c.cfg.APIToken != "" {
		header.Set("Authorization", "Bearer "+c.cfg.APIToken)
	}
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: c.client, HTTPHeader: header})
	if err != nil {
		return err
	}
	conn.SetReadLimit(maxMessageBytes)
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
		if err := c.dispatch(ctx, data); err != nil {
			c.Logger().Warn("could not dispatch Signal event", "error", err)
		}
	}
}

func receiveURL(apiBase, number string) (string, error) {
	parsed, err := url.Parse(apiBase)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "https" {
		parsed.Scheme = "wss"
	} else if parsed.Scheme == "http" {
		parsed.Scheme = "ws"
	} else {
		return "", errors.New("Signal apiBase must use http or https")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/v1/receive/" + number
	return parsed.String(), nil
}

type receiveEvent struct {
	Envelope struct {
		Source       string `json:"source"`
		SourceNumber string `json:"sourceNumber"`
		DataMessage  struct {
			Message   string `json:"message"`
			GroupInfo *struct {
				GroupID string `json:"groupId"`
			} `json:"groupInfo"`
		} `json:"dataMessage"`
		SyncMessage json.RawMessage `json:"syncMessage"`
	} `json:"envelope"`
	Account string `json:"account"`
}

func (c *Channel) dispatch(ctx context.Context, data []byte) error {
	var events []receiveEvent
	if err := json.Unmarshal(data, &events); err != nil {
		var event receiveEvent
		if singleErr := json.Unmarshal(data, &event); singleErr != nil {
			return fmt.Errorf("decode Signal receive event: %w", err)
		}
		events = []receiveEvent{event}
	}
	for _, event := range events {
		if event.Envelope.SyncMessage != nil {
			continue
		}
		message := strings.TrimSpace(event.Envelope.DataMessage.Message)
		sender := event.Envelope.SourceNumber
		if sender == "" {
			sender = event.Envelope.Source
		}
		if sender == "" || sender == c.cfg.Number || message == "" || !c.allowedSender(sender) {
			continue
		}
		chatID := sender
		isDM := true
		if group := event.Envelope.DataMessage.GroupInfo; group != nil && group.GroupID != "" {
			chatID = group.GroupID
			isDM = false
		}
		if err := c.HandleMessage(ctx, channels.InboundRequest{
			SenderID: sender, ChatID: chatID, Content: message,
			Metadata: map[string]any{"account": event.Account}, IsDM: isDM,
		}); err != nil {
			return err
		}
	}
	return nil
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

func (c *Channel) apiCall(ctx context.Context, method, path string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, method, c.cfg.APIBase+path, strings.NewReader(string(encoded)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if c.cfg.APIToken != "" {
		request.Header.Set("Authorization", "Bearer "+c.cfg.APIToken)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Signal API returned HTTP %d", response.StatusCode)
	}
	return nil
}
