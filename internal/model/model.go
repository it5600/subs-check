package model

// Node represents a proxy node parsed from all.yaml
type Node struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Server  string `json:"server"`
	Port    int    `json:"port"`
	TLS     bool   `json:"tls"`
	IPv6    bool   `json:"ipv6"`
	Network string `json:"network"`
}

// Source represents a subscription source URL
type Source struct {
	Index   int    `json:"index"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
}

// Response is the unified API response format
type Response struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data"`
}
