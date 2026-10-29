// Package model defines data structures for the substore-ui-go API.
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

// StatsSnapshot is the zero-alloc struct return for Stats.Snapshot().
type StatsSnapshot struct {
	Requests    int64 `json:"requests"`
	Errors      int64 `json:"errors"`
	RateLimited int64 `json:"rateLimited"`
}

// DashboardStats holds aggregated dashboard data
type DashboardStats struct {
	Service  string            `json:"service"`
	Uptime   int64             `json:"uptime"`
	Stats    StatsSnapshot     `json:"stats"`
	Upstream map[string]string `json:"upstream"`

	Nodes   NodeSummary    `json:"nodes"`
	Sources SourceSummary  `json:"sources"`
	Status  PipelineStatus `json:"status"`
}

// NodeSummary holds node distribution stats
type NodeSummary struct {
	Total     int            `json:"total"`
	ByType    map[string]int `json:"byType"`
	Available int            `json:"available"`
	AvgDelay  float64        `json:"avgDelay"`
	MinDelay  float64        `json:"minDelay"`
	MaxDelay  float64        `json:"maxDelay"`
}

// SourceSummary holds subscription source stats
type SourceSummary struct {
	Total   int      `json:"total"`
	Enabled int      `json:"enabled"`
	List    []Source `json:"list"`
}

// PipelineStatus holds the upstream subs-check pipeline status
type PipelineStatus struct {
	Checking   bool `json:"checking"`
	Available  int  `json:"available"`
	ProxyCount int  `json:"proxyCount"`
	SpeedPass  int  `json:"speedPass"`
}
