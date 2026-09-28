# WeCom channel

The WeCom transport implements the Enterprise WeChat AI Bot long-connection
protocol directly on HAOSbot's existing `coder/websocket` dependency. No
public webhook endpoint and no separate Python/Node SDK process are required.

Required settings are `botId` and `secret`; `allowFrom` contains WeCom
user IDs. The runtime connects to `wss://openws.work.weixin.qq.com`, sends the
`aibot_subscribe` authentication frame, maintains application-level `ping`
heartbeats and reconnects with bounded exponential backoff.

Inbound text, transcribed voice and the text parts of mixed messages are
forwarded to HAOSbot. Image/file/video messages currently surface as textual
placeholders instead of being downloaded. Message IDs are deduplicated with a
bounded cache.

For a conversation with a fresh callback frame, the final answer uses
`aibot_respond_msg` with a finished stream reply. If no callback frame is
available (for example after restart or for proactive delivery), HAOSbot uses
`aibot_send_msg` with Markdown. Both paths wait for WeCom's req_id ACK and
propagate delivery errors to the ChannelManager retry policy.

The channel intentionally disables HAOS progress/tool-hint events by default so
the one-reply-per-request protocol is not flooded with intermediate updates.
