package audit

import "time"

// Record represents a single audit entry for a proxied LLM request.
type Record struct {
	RequestID     string    `json:"request_id"`
	VirtualAPIKey string    `json:"virtual_api_key"`
	ProviderName  string    `json:"provider_name"`
	ModelShowName string    `json:"model_show_name"`
	ModelRealName string    `json:"model_real_name"`
	RequestStart  time.Time `json:"request_start_at"`
	FirstByteAt   time.Time `json:"first_byte_at"`
	RequestEnd    time.Time `json:"request_end_at"`

	InputTokens    int `json:"input_tokens"`
	OutputTokens   int `json:"output_tokens"`
	CacheHitTokens int `json:"cache_hit_tokens"`

	ToolCalls  []string `json:"tool_calls"`
	IsStream   bool     `json:"is_stream"`

	RequestBody  string `json:"request_body"`
	ResponseBody string `json:"response_body"`
	Truncated    bool   `json:"truncated"`

	StatusCode   int    `json:"status_code"`
	ErrorMessage string `json:"error_message"`
}
