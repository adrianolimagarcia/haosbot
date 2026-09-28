# Discord channel

The Discord transport uses the official Gateway for inbound messages and the REST API for replies. Create a bot in the Discord Developer Portal, invite it with message permissions, and enable the privileged **Message Content Intent** for the application. The gateway requests only `GUILDS`, `GUILD_MESSAGES`, `DIRECT_MESSAGES`, and `MESSAGE_CONTENT` intents.

```json
{
  "channels": {
    "discord": {
      "enabled": true,
      "botToken": "replace-with-a-private-bot-token",
      "allowFrom": ["123456789012345678"],
      "allowedChannels": ["234567890123456789"]
    }
  }
}
```

`allowFrom` matches Discord user IDs through the shared channel authorization policy. Direct messages can use the existing pairing flow. For guild channels, configure the sender allowlist or `*`. `allowedChannels` optionally narrows inbound messages to selected channel IDs.

This initial transport sends and receives text, identifies the bot before connecting, acknowledges Gateway heartbeats, and reconnects with backoff. It does not yet handle attachments, message edits/deletes, forum posts, voice, or incremental streaming. The token must be kept private.
