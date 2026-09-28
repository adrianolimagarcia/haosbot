# Feishu / Lark channel

This Go transport uses the official HTTP event-subscription surface rather than
embedding the full Feishu SDK. Configure the app for `im.message.receive_v1`
and point its request URL at `/feishu/webhook` (or the configured path).

Required settings are `appId`, `appSecret` and `verificationToken`. Set
`domain` to `feishu` or `lark`. `allowFrom` contains sender open IDs.

For defense in depth, `signatureKey` enables validation of
`X-Lark-Signature` as SHA-256(timestamp + nonce + key + raw body). The
verification token is checked independently. URL-verification challenges,
event-ID deduplication, sender allowlisting, mention removal, tenant-token
caching and text replies through `im/v1/messages` are implemented.

Encrypted callback bodies (payloads containing `encrypt`) are intentionally
rejected rather than silently misparsed. For this lightweight webhook mode,
leave payload encryption disabled; signature verification plus the verification
token can still be enabled. Media and rich-post messages are outside this first
transport scope.
