// Package registry is the single source of truth for built-in channel
// constructors and their public setup contracts.
package registry

import (
	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels/matrix"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels/telegram"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels/websocket"
)

// Builder constructs one channel runtime from its decoded configuration.
type Builder func(channels.Section, channels.InboundPublisher) (channels.Channel, error)

// Manifest describes a transport to both the runtime and management UI.
type Manifest struct {
	ID          string
	Name        string
	Description string
	Build       Builder
	Setup       map[string]any
}

// All returns the registered transports in stable UI order.
func All() []Manifest {
	return []Manifest{
		{
			ID:          telegram.ChannelName,
			Name:        "Telegram",
			Description: "Bot API · polling ou webhook",
			Build:       telegram.New,
			Setup:       telegram.LookupSetupSpec(telegram.ChannelName).ToPublicDict(telegram.ChannelName),
		},
		{
			ID:          websocket.ChannelName,
			Name:        "WebSocket",
			Description: "Integração customizada via conexão persistente",
			Build:       websocket.New,
			Setup:       websocket.PublicSetup(),
		},
		{
			ID:          matrix.ChannelName,
			Name:        "Matrix",
			Description: "Mensageria federada",
			Build:       matrix.New,
			Setup:       matrix.PublicSetup(),
		},
	}
}

// Lookup returns the manifest for a registered transport.
func Lookup(id string) (Manifest, bool) {
	for _, manifest := range All() {
		if manifest.ID == id {
			return manifest, true
		}
	}
	return Manifest{}, false
}

// Builders returns a fresh map for the gateway channel manager.
func Builders() map[string]Builder {
	result := make(map[string]Builder)
	for _, manifest := range All() {
		result[manifest.ID] = manifest.Build
	}
	return result
}
