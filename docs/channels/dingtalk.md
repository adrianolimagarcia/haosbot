# DingTalk channel

The DingTalk transport implements Stream Mode directly in Go. It requests a
gateway endpoint from `/v1.0/gateway/connections/open`, subscribes to system
ping/disconnect frames and `/v1.0/im/bot/messages/get`, then connects with the
returned ticket over WebSocket.

Required settings are `clientId` and `clientSecret`; `allowFrom` contains
DingTalk staff/user IDs. Group chats are routed as
`group:<conversationId>`. `groupUserIsolation` optionally gives each sender
inside a group a separate HAOS session, and `disablePrivateChat` is a hard DM
kill switch.

Replies use the signed `sessionWebhook` supplied by DingTalk in the authenticated
Stream callback. HAOSbot accepts only HTTPS hosts under `dingtalk.com` and
honors `sessionWebhookExpiredTime`; after expiry or process restart, a fresh
inbound message is required before that conversation can receive a reply.

This first transport is text-first. It intentionally does not pull the full
DingTalk SDK into the HAOS binary; the implemented wire contract mirrors the
SDK's connection request, data-frame ACK and chatbot callback structures.
