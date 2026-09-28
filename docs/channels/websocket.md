# WebSocket channel

The WebSocket transport lets a client exchange text with HAOSbot over one persistent connection. It uses the normal channel manager, so inbound messages enter agent sessions and outbound replies use the configured delivery and retry path.

## Configuration

Enable the channel in `config.json` and set a private token:

```json
{
  "channels": {
    "websocket": {
      "enabled": true,
      "host": "127.0.0.1",
      "port": 8765,
      "path": "/",
      "token": "replace-with-a-long-random-secret",
      "allowFrom": ["desktop-client"],
      "streaming": true,
      "maxMessageBytes": 1048576,
      "maxConnections": 64
    }
  }
}
```

The server binds to loopback by default. It refuses to start without a token, checks that token before upgrading the connection, and authorizes each `client_id` through `allowFrom` or the existing pairing store. Put a TLS reverse proxy in front of it before accepting connections from another machine. The token is sent in the query string to support browser WebSocket clients, so configure proxies and access logs to redact the `token` parameter.

The implementation caps each frame at 8 MiB and defaults to 1 MiB; the outbound queue holds at most 64 frames per client and the server accepts at most 256 connections (default 64). Set tighter limits for small deployments.

## Client protocol

Connect to `ws://127.0.0.1:8765/?client_id=desktop-client&token=...`. The server sends a `ready` event containing the generated `chat_id` and client ID. Send plain text or a JSON object with a `content`, `text`, or `message` string:

```json
{"text":"Hello"}
```

Replies arrive as JSON `message` events. Streaming replies arrive as `delta` events with `stream_id` and `stream_end` fields. The server accepts one chat per connection in this first version.

```json
{"event":"message","chat_id":"…","text":"Hello!"}
```

```json
{"event":"delta","chat_id":"…","text":"Hel","stream_id":"…","stream_end":false}
```

## Current scope

This first transport release supports text messages, token authentication, sender allowlists, streaming text, bounded queues, connection limits, and orderly shutdown. It does not yet implement the nanobot WebSocket terminal envelopes, multiple chats on one connection, short-lived token issuance, Unix sockets, built-in TLS, file URL signing, or the WebSocket-specific operational panels. Those are follow-up work after the channel registry and lifecycle endpoints are generalized.
