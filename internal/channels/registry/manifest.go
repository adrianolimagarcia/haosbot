// Package registry is the single source of truth for built-in channel
// constructors and their public setup contracts.
package registry

import (
	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels/discord"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels/email"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels/linear"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels/matrix"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels/mattermost"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels/napcat"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels/slack"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels/signal"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels/telegram"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels/websocket"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels/whatsapp"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels/weixin"
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
			Build: func(section channels.Section, publisher channels.InboundPublisher) (channels.Channel, error) {
				return telegram.New(section, publisher)
			},
			Setup:       telegramSetup(),
		},
		{
			ID:          websocket.ChannelName,
			Name:        "WebSocket",
			Description: "Integração customizada via conexão persistente",
			Build:       adapt(websocket.New),
			Setup:       websocket.PublicSetup(),
		},
		{
			ID:          email.ChannelName,
			Name:        "Email",
			Description: "IMAP inbox + SMTP replies",
			Build:       adapt(email.New),
			Setup:       email.PublicSetup(),
		},
		{
			ID:          linear.ChannelName,
			Name:        "Linear",
			Description: "Issue comments via signed webhooks + GraphQL",
			Build:       adapt(linear.New),
			Setup:       linear.PublicSetup(),
		},
		{
			ID:          matrix.ChannelName,
			Name:        "Matrix",
			Description: "Mensageria federada",
			Build:       adapt(matrix.New),
			Setup:       matrix.PublicSetup(),
		},
		{
			ID:          slack.ChannelName,
			Name:        "Slack",
			Description: "Mensagens de equipes via Socket Mode",
			Build:       adapt(slack.New),
			Setup:       slack.PublicSetup(),
		},
		{
			ID:          discord.ChannelName,
			Name:        "Discord",
			Description: "Mensagens e comunidades",
			Build:       adapt(discord.New),
			Setup:       discord.PublicSetup(),
		},
		{
			ID:          napcat.ChannelName,
			Name:        "Napcat",
			Description: "Gateway compatível com OneBot 11",
			Build:       adapt(napcat.New),
			Setup:       napcat.PublicSetup(),
		},
		{
			ID:          napcat.QQChannelName,
			Name:        "QQ via OneBot",
			Description: "QQ conectado a um gateway OneBot 11, como Napcat",
			Build:       adapt(napcat.NewQQ),
			Setup:       napcat.PublicSetupFor(napcat.QQChannelName),
		},
		{
			ID:          mattermost.ChannelName,
			Name:        "Mattermost",
			Description: "Mensageria auto-hospedada com WebSocket",
			Build:       adapt(mattermost.New),
			Setup:       mattermost.PublicSetup(),
		},
		{
			ID:          signal.ChannelName,
			Name:        "Signal",
			Description: "Mensageria privada via signal-cli-rest-api",
			Build:       adapt(signal.New),
			Setup:       signal.PublicSetup(),
		},
		{
			ID:          weixin.ChannelName,
			Name:        "WeChat / Weixin",
			Description: "Personal WeChat via iLink HTTP long-poll",
			Build:       adapt(weixin.New),
			Setup:       weixin.PublicSetup(),
		},
		{
			ID:          whatsapp.ChannelName,
			Name:        "WhatsApp Cloud API",
			Description: "Mensagens via webhook oficial e Graph API",
			Build:       adapt(whatsapp.New),
			Setup:       whatsapp.PublicSetup(),
		},
	}
}

func telegramSetup() map[string]any {
	setup := telegram.LookupSetupSpec(telegram.ChannelName).ToPublicDict(telegram.ChannelName)
	setup["verifies_connection"] = true
	return setup
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

// adapt converts concrete channel constructors to the common runtime signature.
func adapt[T channels.Channel](build func(channels.Section, channels.InboundPublisher) (T, error)) Builder {
	return func(section channels.Section, publisher channels.InboundPublisher) (channels.Channel, error) {
		return build(section, publisher)
	}
}
