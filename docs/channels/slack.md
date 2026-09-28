# Slack channel

The Slack transport uses Socket Mode for inbound events and the Slack Web API for replies. Create a Slack app, enable Socket Mode, subscribe to `message.channels`, `message.groups`, `message.im` and `message.mpim`, and grant the bot `chat:write`, `channels:history`, `groups:history`, `im:history` and `mpim:history` scopes as needed. Create an app-level token with `connections:write` and a bot token with the required chat scopes.

```json
{
  "channels": {
    "slack": {
      "enabled": true,
      "botToken": "xoxb-private-bot-token",
      "appToken": "xapp-private-app-token",
      "allowFrom": ["U0123456789"],
      "allowedRooms": ["C0123456789"]
    }
  }
}
```

`allowFrom` uses Slack user IDs and is checked by the shared authorization policy. Set it to `*` only when every user in the selected workspaces should be able to invoke the agent. `allowedRooms` optionally restricts inbound events to channel IDs. Thread replies keep separate agent sessions and are posted back into their Slack thread.

This initial transport handles plain text events and messages. It ignores bot messages and subtypes, and does not yet handle files, interactive events, edits/deletes, or streamed updates. Keep both tokens private; the bot token posts messages and the app token opens Socket Mode connections.
