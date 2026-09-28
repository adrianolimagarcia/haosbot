package matrix

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
	"sync/atomic"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

// Channel connects to a Matrix homeserver using the Client-Server API.
type Channel struct {
	*channels.Base
	cfg    Config
	client *http.Client
	seq    atomic.Uint64
}

func New(section channels.Section, publisher channels.InboundPublisher) (*Channel, error) {
	cfg, err := sectionConfig(section)
	if err != nil {
		return nil, err
	}
	channel := &Channel{cfg: cfg, client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	channel.Base = channels.NewBase(channel, section, publisher,
		channels.WithName(ChannelName), channels.WithDisplayName("Matrix"))
	return channel, nil
}

func (c *Channel) Start(ctx context.Context) error {
	if c.cfg.Homeserver == "" || c.cfg.AccessToken == "" || c.cfg.UserID == "" {
		return errors.New("matrix: homeserver, accessToken and userId are required")
	}
	c.SetRunning(true)
	defer c.SetRunning(false)

	if err := c.verifyIdentity(ctx); err != nil {
		return fmt.Errorf("matrix: verify account: %w", err)
	}

	var since string
	for c.IsRunning() && ctx.Err() == nil {
		if err := c.syncOnce(ctx, &since); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.Logger().Warn("Matrix sync failed; retrying", "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
			}
		}
	}
	return nil
}

func (c *Channel) Stop(context.Context) error {
	c.SetRunning(false)
	return nil
}

func (c *Channel) Send(ctx context.Context, message core.OutboundMessage) error {
	if message.ChatID == "" {
		return errors.New("matrix: room id is required")
	}
	roomID := url.PathEscape(message.ChatID)
	txnID := fmt.Sprintf("haosbot-%d-%d", time.Now().UnixMilli(), c.seq.Add(1))
	endpoint := c.cfg.Homeserver + "/_matrix/client/v3/rooms/" + roomID + "/send/m.room.message/" + url.PathEscape(txnID)
	body := map[string]any{"msgtype": "m.text", "body": message.Content}
	return c.doJSON(ctx, http.MethodPut, endpoint, body, nil)
}

func (c *Channel) verifyIdentity(ctx context.Context) error {
	var result struct {
		UserID string `json:"user_id"`
	}
	err := c.doJSON(ctx, http.MethodGet, c.cfg.Homeserver+"/_matrix/client/v3/account/whoami", nil, &result)
	if err != nil {
		return err
	}
	if result.UserID == "" || result.UserID != c.cfg.UserID {
		return fmt.Errorf("configured userId does not match homeserver account")
	}
	return nil
}

func (c *Channel) syncOnce(ctx context.Context, since *string) error {
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.SyncTimeoutMS+5_000)*time.Millisecond)
	defer cancel()

	endpoint, err := url.Parse(c.cfg.Homeserver + "/_matrix/client/v3/sync")
	if err != nil {
		return err
	}
	query := endpoint.Query()
	query.Set("timeout", fmt.Sprint(c.cfg.SyncTimeoutMS))
	if *since != "" {
		query.Set("since", *since)
	}
	endpoint.RawQuery = query.Encode()

	var response syncResponse
	if err := c.doJSON(requestCtx, http.MethodGet, endpoint.String(), nil, &response); err != nil {
		return err
	}
	if response.NextBatch == "" {
		return errors.New("Matrix sync response has no next_batch")
	}
	*since = response.NextBatch
	for roomID, room := range response.Rooms.Join {
		if !c.acceptRoom(roomID) {
			continue
		}
		for _, event := range room.Timeline.Events {
			if err := c.handleEvent(ctx, roomID, event); err != nil {
				c.Logger().Warn("Matrix event dispatch failed", "room_id", roomID, "event_id", event.EventID, "error", err)
			}
		}
	}
	return nil
}

type syncResponse struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join map[string]struct {
			Timeline struct {
				Events []roomEvent `json:"events"`
			} `json:"timeline"`
		} `json:"join"`
	} `json:"rooms"`
}

type roomEvent struct {
	EventID string `json:"event_id"`
	Sender  string `json:"sender"`
	Type    string `json:"type"`
	Content struct {
		MsgType string `json:"msgtype"`
		Body    string `json:"body"`
	} `json:"content"`
}

func (c *Channel) handleEvent(ctx context.Context, roomID string, event roomEvent) error {
	if event.Type != "m.room.message" || event.Sender == "" || event.Sender == c.cfg.UserID {
		return nil
	}
	if event.Content.MsgType != "m.text" && event.Content.MsgType != "m.notice" {
		return nil
	}
	content := strings.TrimSpace(event.Content.Body)
	if content == "" {
		return nil
	}
	return c.HandleMessage(ctx, channels.InboundRequest{
		SenderID: event.Sender,
		ChatID:   roomID,
		Content:  content,
		Metadata: map[string]any{"event_id": event.EventID, "room_id": roomID},
		IsDM:     false,
	})
}

func (c *Channel) acceptRoom(roomID string) bool {
	if len(c.cfg.Rooms) == 0 {
		return true
	}
	for _, allowed := range c.cfg.Rooms {
		if roomID == allowed {
			return true
		}
	}
	return false
}

func (c *Channel) doJSON(ctx context.Context, method, endpoint string, input any, output any) error {
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
	request.Header.Set("Authorization", "Bearer "+c.cfg.AccessToken)
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
		return errors.New("Matrix response exceeded size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Matrix API returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	if output == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode Matrix response: %w", err)
	}
	return nil
}
