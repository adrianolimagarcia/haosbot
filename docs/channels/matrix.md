# Matrix channel

The Matrix transport uses the Client-Server API with a configured access token. It verifies the token's account with `whoami`, polls `/sync`, and sends plain text `m.room.message` events.

```json
{
  "channels": {
    "matrix": {
      "enabled": true,
      "homeserver": "https://matrix.example.org",
      "accessToken": "replace-with-a-private-access-token",
      "userId": "@haosbot:example.org",
      "rooms": ["!roomid:example.org"],
      "allowFrom": ["@alice:example.org"],
      "syncTimeoutMs": 30000
    }
  }
}
```

`rooms` restricts which joined rooms are read. `allowFrom` is the sender allowlist used by the common channel authorization policy; use `*` only when every member of the configured rooms should be able to invoke the agent. Keep the access token private and use HTTPS for non-local homeservers.

This initial transport handles text and notice events, ignores the bot's own events and unsupported event types, and supports plain text outbound messages. It does not implement encrypted rooms, media uploads, room invitations, or incremental streaming. The channel only reads rooms the configured Matrix account has already joined.
