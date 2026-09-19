package triggers

type RunRecord struct {
	RunAtMS int64 `json:"runAtMs"`
	Status string `json:"status"`
	Error string `json:"error,omitempty"`
	DeliveryID string `json:"deliveryId,omitempty"`
}
type Trigger struct {
	ID string `json:"id"`; Name string `json:"name"`; Enabled bool `json:"enabled"`
	Channel string `json:"channel"`; ChatID string `json:"chatId"`; SessionKey string `json:"sessionKey"`; SenderID string `json:"senderId"`
	OriginMetadata map[string]any `json:"originMetadata,omitempty"`
	CreatedAtMS int64 `json:"createdAtMs"`; UpdatedAtMS int64 `json:"updatedAtMs"`; LastMessage string `json:"lastMessage,omitempty"`
	LastRunAtMS *int64 `json:"lastRunAtMs,omitempty"`; LastStatus string `json:"lastStatus,omitempty"`; LastError string `json:"lastError,omitempty"`
	RunHistory []RunRecord `json:"runHistory,omitempty"`
}
type Delivery struct {
	ID string `json:"id"`; TriggerID string `json:"triggerId"`; Content string `json:"content"`; CreatedAtMS int64 `json:"createdAtMs"`
	Attempts int `json:"attempts"`; LastError string `json:"lastError,omitempty"`; Path string `json:"-"`
}
