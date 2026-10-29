package service

import (
	"testing"
)

func TestParseNodesFromYaml_Empty(t *testing.T) {
	result := ParseNodesFromYaml("")
	if len(result) != 0 {
		t.Errorf("Expected empty result for empty input, got %d nodes", len(result))
	}
}

func TestParseNodesFromYaml_Standard(t *testing.T) {
	yaml := `proxies:
  - name: "node1"
    type: vless
    server: 1.2.3.4
    port: 443
    tls: true
    network: ws
  - name: "node2"
    type: trojan
    server: 5.6.7.8
    port: 8443
    tls: true
`
	result := ParseNodesFromYaml(yaml)
	if len(result) != 2 {
		t.Errorf("Expected 2 nodes, got %d", len(result))
	}
	if result[0].Name != "node1" || result[0].Port != 443 {
		t.Errorf("Unexpected first node: %+v", result[0])
	}
	if result[1].Name != "node2" || result[1].Port != 8443 {
		t.Errorf("Unexpected second node: %+v", result[1])
	}
}

func TestParseNodesFromYaml_Fallback(t *testing.T) {
	yaml := `- name: "fallback-node"
  type: vless
  server: 9.9.9.9
  port: 8080
`
	result := ParseNodesFromYaml(yaml)
	if len(result) != 1 {
		t.Errorf("Expected 1 fallback node, got %d", len(result))
	}
	if result[0].Server != "9.9.9.9" || result[0].Port != 8080 {
		t.Errorf("Unexpected fallback node: %+v", result[0])
	}
}

func TestParseNodesFromYaml_InvalidPort(t *testing.T) {
	yaml := `proxies:
  - name: "bad-port"
    type: vless
    server: 1.1.1.1
    port: 99999
`
	result := ParseNodesFromYaml(yaml)
	if len(result) != 0 {
		t.Errorf("Expected 0 nodes for port out of range, got %d", len(result))
	}
}

func TestParseSubUrls_Standard(t *testing.T) {
	cfg := `sub-urls:
  - "https://example.com/sub1"
  - "https://example.com/sub2"
other-key: value
`
	result := ParseSubUrls(cfg)
	if len(result) != 2 {
		t.Errorf("Expected 2 sources, got %d", len(result))
	}
	if result[0].URL != "https://example.com/sub1" {
		t.Errorf("Unexpected URL: %s", result[0].URL)
	}
	if result[1].URL != "https://example.com/sub2" {
		t.Errorf("Unexpected URL: %s", result[1].URL)
	}
}

func TestParseSubUrls_EmptyArray(t *testing.T) {
	cfg := `sub-urls: []
other-key: value
`
	result := ParseSubUrls(cfg)
	if len(result) != 0 {
		t.Errorf("Expected 0 sources for empty array, got %d", len(result))
	}
}

func TestParseSubUrls_NoSubUrls(t *testing.T) {
	cfg := `other-key: value
another-key: value2
`
	result := ParseSubUrls(cfg)
	if len(result) != 0 {
		t.Errorf("Expected 0 sources when no sub-urls key, got %d", len(result))
	}
}

func TestValidateURL(t *testing.T) {
	tests := []struct {
		url      string
		expected bool
	}{
		{"https://example.com", true},
		{"http://example.com", true},
		{"ftp://example.com", false},
		{"https://", false},
		{"http", false},
		{"", false},
		{"not-a-url", false},
		{"https://example.com/path?query=1", true},
	}

	for _, tt := range tests {
		result := ValidateURL(tt.url)
		if result != tt.expected {
			t.Errorf("ValidateURL(%q) = %v, expected %v", tt.url, result, tt.expected)
		}
	}
}

func TestEscapeHtml(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"<script>", "&lt;script&gt;"},
		{"a & b", "a &amp; b"},
		{"\"quoted\"", "&quot;quoted&quot;"},
		{"normal text", "normal text"},
		{"", ""},
	}

	for _, tt := range tests {
		result := EscapeHtml(tt.input)
		if result != tt.expected {
			t.Errorf("EscapeHtml(%q) = %q, expected %q", tt.input, result, tt.expected)
		}
	}
}

func TestComputeETag(t *testing.T) {
	data := []byte("hello world")
	etag := ComputeETag(data)
	if len(etag) < 2 {
		t.Errorf("ETag too short: %s", etag)
	}
	if etag[0] != '"' || etag[len(etag)-1] != '"' {
		t.Errorf("ETag not wrapped in quotes: %s", etag)
	}
}

func TestFormatError(t *testing.T) {
	resp := FormatError(400, "test error")
	if resp.Code != 400 || resp.Message != "test error" {
		t.Errorf("Unexpected FormatError result: %+v", resp)
	}
	if resp.Data != nil {
		t.Errorf("Expected nil data, got %v", resp.Data)
	}
}

func TestFormatOK(t *testing.T) {
	resp := FormatOK("test data")
	if resp.Code != 0 || resp.Message != "ok" {
		t.Errorf("Unexpected FormatOK result: %+v", resp)
	}
	if resp.Data != "test data" {
		t.Errorf("Expected 'test data', got %v", resp.Data)
	}
}
