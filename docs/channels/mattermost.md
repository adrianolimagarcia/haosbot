# Mattermost channel

The Mattermost transport verifies a bot token with `/api/v4/users/me`, receives `posted` events on `/api/v4/websocket`, and sends replies with `/api/v4/posts`. It authenticates the WebSocket using Mattermost's authentication challenge.

```json
{
  "channels": {
    "mattermost": {
      "enabled": true,
      "serverUrl": "https://chat.example.org",
      "botToken": "replace-with-a-private-bot-token",
      "allowFrom": ["mattermost-user-id"],
      "allowedChannels": ["mattermost-channel-id"]
    }
  }
}
```

`allowFrom` matches Mattermost user IDs through the shared sender policy. `allowedChannels` optionally limits which channel IDs are read. Replies stay in the original post's thread. Use a bot account with access only to the channels it needs; keep its token private.

This first version handles text posts and threaded replies. It ignores the bot's own posts and does not support file uploads, post edits/deletes, direct-message pairing, or streaming.
