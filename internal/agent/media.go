package agent

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

const (
	maxTurnMediaFiles = 8
	maxImageMediaBytes = 20 << 20
	maxTextMediaBytes  = 512 << 10
)

func userMessageForModel(msg core.InboundMessage) (core.Message, error) {
	if len(msg.Media) == 0 {
		return *core.NewMessage(core.RoleUser, msg.Content), nil
	}
	if len(msg.Media) > maxTurnMediaFiles {
		return core.Message{}, fmt.Errorf("too many attachments: %d > %d", len(msg.Media), maxTurnMediaFiles)
	}
	blocks := make([]core.ContentBlock, 0, len(msg.Media)+1)
	if strings.TrimSpace(msg.Content) != "" {
		blocks = append(blocks, core.ContentBlock{Type: "text", Text: msg.Content})
	}
	for _, rawPath := range msg.Media {
		path := strings.TrimSpace(rawPath)
		if path == "" { continue }
		info, err := os.Lstat(path)
		if err != nil { return core.Message{}, fmt.Errorf("attachment %s: %w", filepath.Base(path), err) }
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return core.Message{}, fmt.Errorf("attachment %s is not a regular file", filepath.Base(path))
		}
		if info.Size() > maxImageMediaBytes {
			return core.Message{}, fmt.Errorf("attachment %s exceeds %d bytes", filepath.Base(path), maxImageMediaBytes)
		}
		f, err := os.Open(path)
		if err != nil { return core.Message{}, err }
		head := make([]byte, 512)
		n, _ := f.Read(head)
		_ = f.Close()
		mime := http.DetectContentType(head[:n])
		switch mime {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
			data, err := os.ReadFile(path)
			if err != nil { return core.Message{}, err }
			url := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
			imageURL, _ := json.Marshal(map[string]any{"url": url})
			blocks = append(blocks, core.ContentBlock{Type: "image_url", ImageURL: imageURL})
		default:
			if info.Size() <= maxTextMediaBytes {
				data, err := os.ReadFile(path)
				if err != nil { return core.Message{}, err }
				if utf8.Valid(data) && (strings.HasPrefix(mime, "text/") || mime == "application/json" || mime == "application/xml" || mime == "application/octet-stream") {
					blocks = append(blocks, core.ContentBlock{Type: "text", Text: "[Attachment: " + filepath.Base(path) + "]\n" + string(data)})
					continue
				}
			}
			blocks = append(blocks, core.ContentBlock{Type: "text", Text: "[Binary attachment: " + filepath.Base(path) + " (" + mime + "). The binary contents are not injected into the model context.]"})
		}
	}
	if len(blocks) == 0 {
		return core.Message{}, errors.New("attachments produced no model content")
	}
	return core.Message{Role: core.RoleUser, Content: core.BlockContent(blocks)}, nil
}
