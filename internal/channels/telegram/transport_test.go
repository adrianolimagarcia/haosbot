package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/events"
)

type recordingPublisher struct {
	messages []core.InboundMessage
}

func (p *recordingPublisher) PublishInbound(ctx context.Context, msg core.InboundMessage) error {
	p.messages = append(p.messages, msg)
	return nil
}

func TestBotClient_Methods(t *testing.T) {
	mux := http.NewServeMux()

	var getMeCalls int32
	mux.HandleFunc("/botTESTTOKEN/getMe", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&getMeCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"id":         123456,
				"is_bot":     true,
				"first_name": "TestBot",
				"username":   "test_bot",
			},
		})
	})

	var sendMessagePayload SendMessageParams
	mux.HandleFunc("/botTESTTOKEN/sendMessage", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &sendMessagePayload)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": 999,
				"text":       sendMessagePayload.Text,
			},
		})
	})

	var editPayload EditMessageTextParams
	mux.HandleFunc("/botTESTTOKEN/editMessageText", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &editPayload)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": editPayload.MessageID,
				"text":       editPayload.Text,
			},
		})
	})

	var setCommandsPayload SetMyCommandsParams
	mux.HandleFunc("/botTESTTOKEN/setMyCommands", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &setCommandsPayload)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	})

	mux.HandleFunc("/botTESTTOKEN/deleteWebhook", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	})

	var setWebhookPayload SetWebhookParams
	mux.HandleFunc("/botTESTTOKEN/setWebhook", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &setWebhookPayload)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	})

	var chatActionPayload SendChatActionParams
	mux.HandleFunc("/botTESTTOKEN/sendChatAction", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &chatActionPayload)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	})

	var answerCallbackPayload AnswerCallbackQueryParams
	mux.HandleFunc("/botTESTTOKEN/answerCallbackQuery", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &answerCallbackPayload)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	})

	mux.HandleFunc("/botTESTTOKEN/getUpdates", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": []map[string]any{
				{
					"update_id": 1,
					"message": map[string]any{
						"message_id": 101,
						"text":       "hello",
						"from": map[string]any{
							"id":         42,
							"first_name": "Alice",
							"username":   "alice",
						},
						"chat": map[string]any{
							"id":   123,
							"type": "private",
						},
					},
				},
			},
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewBotClient("TESTTOKEN", "", WithBaseURL(srv.URL))
	ctx := context.Background()

	// 1. GetMe
	user, err := client.GetMe(ctx)
	if err != nil {
		t.Fatalf("GetMe failed: %v", err)
	}
	if user.ID != 123456 || user.Username == nil || *user.Username != "test_bot" {
		t.Fatalf("unexpected user: %+v", user)
	}

	// 2. SendMessage
	msg, err := client.SendMessage(ctx, SendMessageParams{
		ChatID:    123,
		Text:      "Test message",
		ParseMode: "HTML",
	})
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}
	if msg.MessageID != 999 || sendMessagePayload.Text != "Test message" {
		t.Fatalf("unexpected sent message: %+v, payload: %+v", msg, sendMessagePayload)
	}

	// 3. EditMessageText
	editMsg, err := client.EditMessageText(ctx, EditMessageTextParams{
		ChatID:    123,
		MessageID: 999,
		Text:      "Edited message",
	})
	if err != nil {
		t.Fatalf("EditMessageText failed: %v", err)
	}
	if editMsg.MessageID != 999 || editPayload.Text != "Edited message" {
		t.Fatalf("unexpected edit message: %+v, payload: %+v", editMsg, editPayload)
	}

	// 4. SetMyCommands
	cmds := DefaultBotCommands()
	if err := client.SetMyCommands(ctx, SetMyCommandsParams{Commands: cmds}); err != nil {
		t.Fatalf("SetMyCommands failed: %v", err)
	}
	if len(setCommandsPayload.Commands) != len(cmds) {
		t.Fatalf("commands count mismatch: got %d, want %d", len(setCommandsPayload.Commands), len(cmds))
	}

	// 5. DeleteWebhook
	if err := client.DeleteWebhook(ctx, DeleteWebhookParams{DropPendingUpdates: true}); err != nil {
		t.Fatalf("DeleteWebhook failed: %v", err)
	}

	// 6. SetWebhook
	if err := client.SetWebhook(ctx, SetWebhookParams{URL: "https://example.com/tg"}); err != nil {
		t.Fatalf("SetWebhook failed: %v", err)
	}
	if setWebhookPayload.URL != "https://example.com/tg" {
		t.Fatalf("webhook url mismatch: got %s", setWebhookPayload.URL)
	}

	// 7. SendChatAction
	if err := client.SendChatAction(ctx, SendChatActionParams{ChatID: 123, Action: "typing"}); err != nil {
		t.Fatalf("SendChatAction failed: %v", err)
	}
	if chatActionPayload.Action != "typing" {
		t.Fatalf("action mismatch: got %s", chatActionPayload.Action)
	}

	// 8. AnswerCallbackQuery
	if err := client.AnswerCallbackQuery(ctx, AnswerCallbackQueryParams{CallbackQueryID: "cb123", Text: "Done"}); err != nil {
		t.Fatalf("AnswerCallbackQuery failed: %v", err)
	}
	if answerCallbackPayload.CallbackQueryID != "cb123" || answerCallbackPayload.Text != "Done" {
		t.Fatalf("callback query mismatch: %+v", answerCallbackPayload)
	}

	// 9. GetUpdates
	updates, err := client.GetUpdates(ctx, GetUpdatesParams{Offset: 0, Limit: 10})
	if err != nil {
		t.Fatalf("GetUpdates failed: %v", err)
	}
	if len(updates) != 1 || updates[0].UpdateID != 1 {
		t.Fatalf("unexpected updates: %+v", updates)
	}
}

func TestBotClient_ErrorHandling(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/botTESTTOKEN/invalidToken", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":          false,
			"error_code":  401,
			"description": "Unauthorized",
		})
	})
	mux.HandleFunc("/botTESTTOKEN/rateLimit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":          false,
			"error_code":  429,
			"description": "Too Many Requests: retry after 5",
			"parameters": map[string]any{
				"retry_after": 5,
			},
		})
	})
	mux.HandleFunc("/botTESTTOKEN/badRequest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":          false,
			"error_code":  400,
			"description": "Bad Request: message is not modified",
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewBotClient("TESTTOKEN", "", WithBaseURL(srv.URL))
	ctx := context.Background()

	var dummy any
	err := client.do(ctx, "invalidToken", nil, &dummy)
	if err != ErrTokenInvalid {
		t.Fatalf("expected ErrTokenInvalid, got %v", err)
	}

	err = client.do(ctx, "rateLimit", nil, &dummy)
	var apiErr *APIError
	if !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected 429 error, got %v", err)
	}
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Parameters == nil || apiErr.Parameters.RetryAfter != 5 {
		t.Fatalf("expected RetryAfter 5, got %+v", apiErr)
	}
	if !apiErr.IsTransient() {
		t.Fatalf("expected 429 to be transient")
	}

	err = client.do(ctx, "badRequest", nil, &dummy)
	apiErr, ok = err.(*APIError)
	if !ok || !apiErr.IsNotModified() {
		t.Fatalf("expected IsNotModified=true, got %+v", err)
	}
}

func TestChannel_PollingAndInboundDispatch(t *testing.T) {
	mux := http.NewServeMux()

	var pollCount int32
	mux.HandleFunc("/botTESTTOKEN/getMe", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"id":         9876,
				"is_bot":     true,
				"first_name": "TestBot",
				"username":   "test_bot",
			},
		})
	})
	mux.HandleFunc("/botTESTTOKEN/setMyCommands", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	})
	mux.HandleFunc("/botTESTTOKEN/deleteWebhook", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	})

	mux.HandleFunc("/botTESTTOKEN/getUpdates", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		count := atomic.AddInt32(&pollCount, 1)
		if count == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": []map[string]any{
					{
						"update_id": 10,
						"message": map[string]any{
							"message_id": 501,
							"text":       "/new start fresh session",
							"from": map[string]any{
								"id":         111,
								"first_name": "Bob",
								"username":   "bob",
							},
							"chat": map[string]any{
								"id":   555,
								"type": "private",
							},
						},
					},
					{
						"update_id": 11,
						"message": map[string]any{
							"message_id": 502,
							"text":       "Hello nanobot",
							"from": map[string]any{
								"id":         111,
								"first_name": "Bob",
								"username":   "bob",
							},
							"chat": map[string]any{
								"id":   555,
								"type": "private",
							},
						},
					},
				},
			})
			return
		}
		// Empty poll afterwards
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": []any{},
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	bus := &recordingPublisher{}
	sec := channels.NewMapSection(map[string]any{
		"token":      "TESTTOKEN",
		"allow_from": []any{"*"}, // allow all
	})

	ch, err := New(sec, bus)
	if err != nil {
		t.Fatalf("New channel failed: %v", err)
	}

	client := NewBotClient("TESTTOKEN", "", WithBaseURL(srv.URL))
	ch.SetClient(client)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = ch.Start(ctx)
	}()

	// Wait for inbound drain
	var received int
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		if len(bus.messages) >= 2 {
			received = len(bus.messages)
			break
		}
	}

	_ = ch.Stop(ctx)
	cancel()

	if received < 2 {
		t.Fatalf("expected at least 2 inbound messages, got %d", received)
	}

	// Verify command message
	cmdMsg := bus.messages[0]
	if cmdMsg.Content != "/new start fresh session" {
		t.Errorf("expected command content, got %q", cmdMsg.Content)
	}
	if cmdMsg.SenderID != "111|bob" {
		t.Errorf("expected senderID 111|bob, got %q", cmdMsg.SenderID)
	}
	if cmdMsg.ChatID != "555" {
		t.Errorf("expected chatID 555, got %q", cmdMsg.ChatID)
	}

	// Verify text message
	textMsg := bus.messages[1]
	if textMsg.Content != "Hello nanobot" {
		t.Errorf("expected text content, got %q", textMsg.Content)
	}
}

func TestChannel_OutboundSend_ChunkingAndFormatting(t *testing.T) {
	mux := http.NewServeMux()
	var sentMessages []SendMessageParams
	mux.HandleFunc("/botTESTTOKEN/sendMessage", func(w http.ResponseWriter, r *http.Request) {
		var payload SendMessageParams
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		sentMessages = append(sentMessages, payload)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": int64(1000 + len(sentMessages)),
				"text":       payload.Text,
			},
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	bus := &recordingPublisher{}
	sec := channels.NewMapSection(map[string]any{
		"token":            "TESTTOKEN",
		"reply_to_message": true,
	})

	ch, err := New(sec, bus)
	if err != nil {
		t.Fatalf("New channel failed: %v", err)
	}
	ch.SetClient(NewBotClient("TESTTOKEN", "", WithBaseURL(srv.URL)))

	// 1. Send normal markdown text with formatting
	ctx := context.Background()
	outMsg := core.OutboundMessage{
		Channel: "telegram",
		ChatID:  "12345",
		Content: "**Bold text** and *italic*",
		Metadata: map[string]any{
			"message_id": int64(42),
		},
	}
	if err := ch.Send(ctx, outMsg); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if len(sentMessages) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(sentMessages))
	}
	if sentMessages[0].ParseMode != "HTML" {
		t.Errorf("expected parse_mode HTML, got %q", sentMessages[0].ParseMode)
	}
	if sentMessages[0].ReplyParameters == nil || sentMessages[0].ReplyParameters.MessageID != 42 {
		t.Errorf("expected reply_parameters with message_id 42, got %+v", sentMessages[0].ReplyParameters)
	}
	if !strings.Contains(sentMessages[0].Text, "<b>Bold text</b>") {
		t.Errorf("expected HTML bold conversion, got %q", sentMessages[0].Text)
	}

	// 2. Large message chunking: text > MaxMessageLen (4000)
	sentMessages = nil
	largeText := strings.Repeat("a", 3500) + "\n\n" + strings.Repeat("b", 1000)
	largeMsg := core.OutboundMessage{
		Channel: "telegram",
		ChatID:  "12345",
		Content: largeText,
	}
	if err := ch.Send(ctx, largeMsg); err != nil {
		t.Fatalf("Send large message failed: %v", err)
	}
	if len(sentMessages) != 2 {
		t.Fatalf("expected 2 chunked messages, got %d", len(sentMessages))
	}
}

func TestChannel_OutboundSend_CompactionNotices(t *testing.T) {
	mux := http.NewServeMux()
	var sentCalls []SendMessageParams
	var editCalls []EditMessageTextParams

	mux.HandleFunc("/botTESTTOKEN/sendMessage", func(w http.ResponseWriter, r *http.Request) {
		var payload SendMessageParams
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		sentCalls = append(sentCalls, payload)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": int64(777),
				"text":       payload.Text,
			},
		})
	})

	mux.HandleFunc("/botTESTTOKEN/editMessageText", func(w http.ResponseWriter, r *http.Request) {
		var payload EditMessageTextParams
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		editCalls = append(editCalls, payload)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": payload.MessageID,
				"text":       payload.Text,
			},
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	bus := &recordingPublisher{}
	sec := channels.NewMapSection(map[string]any{
		"token": "TESTTOKEN",
	})

	ch, err := New(sec, bus)
	if err != nil {
		t.Fatalf("New channel failed: %v", err)
	}
	ch.SetClient(NewBotClient("TESTTOKEN", "", WithBaseURL(srv.URL)))
	ctx := context.Background()

	// Phase 1: started -> should call sendMessage and store message_id 777
	startedMsg := core.OutboundMessage{
		Channel: "telegram",
		ChatID:  "9999",
		Content: "Compressing context...",
		Event: events.ContextCompactionEvent{
			CompactionID: "comp-1",
			Phase:        events.CompactionStarted,
		},
	}
	if err := ch.Send(ctx, startedMsg); err != nil {
		t.Fatalf("Send started compaction notice failed: %v", err)
	}
	if len(sentCalls) != 1 {
		t.Fatalf("expected 1 sendMessage call, got %d", len(sentCalls))
	}
	if sentCalls[0].Text != "Compressing context..." {
		t.Errorf("unexpected text: %q", sentCalls[0].Text)
	}

	// Phase 2: succeeded -> should call editMessageText for message 777
	succeededMsg := core.OutboundMessage{
		Channel: "telegram",
		ChatID:  "9999",
		Content: "Context compacted.",
		Event: events.ContextCompactionEvent{
			CompactionID: "comp-1",
			Phase:        events.CompactionSucceeded,
		},
	}
	if err := ch.Send(ctx, succeededMsg); err != nil {
		t.Fatalf("Send succeeded compaction notice failed: %v", err)
	}
	if len(editCalls) != 1 {
		t.Fatalf("expected 1 editMessageText call, got %d", len(editCalls))
	}
	if editCalls[0].MessageID != 777 || editCalls[0].Text != "Context compacted." {
		t.Errorf("unexpected edit params: %+v", editCalls[0])
	}
}
