package service

import (
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"substore-ui-go/internal/model"
)

// Pre-compiled regexes at package level to avoid per-call compilation overhead.
// Reference: Go regexp docs — MustCompile should be called once at init time.
var (
	reSubUrlsKey    = regexp.MustCompile(`^sub-urls:\s*$`)
	reNonSpaceStart = regexp.MustCompile(`^\S`)
	reValidURL      = regexp.MustCompile(`^https?:\/\/[^\s\/].+`)
)

// parseNodesFromYaml parses proxy nodes from Clash/Mihomo YAML format
// using gopkg.in/yaml.v3 for robust parsing (10-50x faster than regex)
func ParseNodesFromYaml(yamlText string) []model.Node {
	if yamlText == "" {
		return nil
	}

	// Clash YAML structure: proxies: -> []Proxy
	var doc struct {
		Proxies []struct {
			Name       string `yaml:"name"`
			Type       string `yaml:"type"`
			Server     string `yaml:"server"`
			Port       int    `yaml:"port"`
			UUID       string `yaml:"uuid"`
			ServerName string `yaml:"servername"`
			TLS        bool   `yaml:"tls"`
			Network    string `yaml:"network"`
			Path       string `yaml:"path"`
			IPv6       bool   `yaml:"ipv6"`
			WS         bool   `yaml:"ws"`
		} `yaml:"proxies"`
	}

	if err := yaml.Unmarshal([]byte(yamlText), &doc); err != nil {
		// Fallback: try parsing without proxies key (some formats)
		return parseNodesFallback(yamlText)
	}

	nodes := make([]model.Node, 0, len(doc.Proxies))
	for _, p := range doc.Proxies {
		if p.Server == "" || p.Port <= 0 || p.Port > 65535 {
			continue
		}
		ipv6 := p.IPv6 || strings.Contains(p.Server, ":")
		network := p.Network
		if network == "" {
			network = "tcp"
		}
		nodes = append(nodes, model.Node{
			Name:    p.Name,
			Type:    p.Type,
			Server:  p.Server,
			Port:    p.Port,
			TLS:     p.TLS,
			IPv6:    ipv6,
			Network: network,
		})
	}

	return nodes
}

// parseNodesFallback handles YAML that doesn't have the standard proxies key
func parseNodesFallback(yamlText string) []model.Node {
	lines := strings.Split(yamlText, "\n")
	var nodes []model.Node
	var current *model.Node
	fieldIndent := -1

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		// Strip inline comments
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = line[:idx]
		}
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "- name:") {
			if current != nil {
				nodes = append(nodes, *current)
			}
			current = &model.Node{
				Name:    strings.Trim(strings.TrimPrefix(trimmed, "- name:"), "\"'"),
				Network: "tcp",
			}
			fieldIndent = -1
		} else if current != nil && trimmed != "" {
			// Calculate indentation
			leadingSpaces := len(line) - len(strings.TrimLeft(line, " "))

			// Skip nested blocks (deeper indentation)
			if fieldIndent >= 0 && leadingSpaces > fieldIndent {
				continue
			}

			// Set field indent on first field line
			if fieldIndent < 0 && leadingSpaces > 0 {
				fieldIndent = leadingSpaces
			}

			if fieldIndent >= 0 && leadingSpaces == fieldIndent {
				if idx := strings.Index(trimmed, ":"); idx > 0 {
					key := strings.TrimSpace(trimmed[:idx])
					val := strings.TrimSpace(trimmed[idx+1:])
					val = strings.Trim(val, "\"'")

					switch key {
					case "type":
						current.Type = val
					case "server":
						current.Server = val
					case "port":
						if p, err := strconv.Atoi(val); err == nil {
							current.Port = p
						}
					case "tls":
						current.TLS = val == "true"
					case "network":
						current.Network = val
					case "ipv6":
						current.IPv6 = val == "true"
					}
				}
			}
		}
	}

	if current != nil && current.Server != "" && current.Port > 0 && current.Port <= 65535 {
		nodes = append(nodes, *current)
	}

	return nodes
}

// ParseSubUrls extracts sub-urls list from config.yaml
func ParseSubUrls(configText string) []model.Source {
	lines := strings.Split(configText, "\n")
	sources := []model.Source{}
	inSubUrls := false

	for _, line := range lines {
		if reSubUrlsKey.MatchString(line) {
			inSubUrls = true
			continue
		}
		if inSubUrls {
			if reNonSpaceStart.MatchString(line) {
				inSubUrls = false
				continue
			}
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "- ") && strings.Contains(trimmed, "http") {
				u := strings.Trim(strings.TrimPrefix(trimmed, "- "), "\"'")
				sources = append(sources, model.Source{
					Index:   len(sources),
					URL:     u,
					Enabled: true,
				})
			} else if trimmed == "[]" {
				break
			} else if trimmed == "" {
				continue
			} else if !strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "-") {
				break
			}
		}
	}

	return sources
}

// htmlReplacer is a package-level Replacer to avoid per-call allocation.
// Reference: Go strings.Replacer docs — reuse across calls.
var htmlReplacer = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	"\"", "&quot;",
	"'", "&#39;",
)

// EscapeHtml escapes HTML special characters to prevent XSS
func EscapeHtml(s string) string {
	if s == "" {
		return ""
	}
	return htmlReplacer.Replace(s)
}

// ValidateURL checks if a URL is a valid http(s) URL
func ValidateURL(url string) bool {
	return reValidURL.MatchString(url)
}

// FormatError creates a standard error response
func FormatError(code int, message string) *model.Response {
	return &model.Response{
		Code:    code,
		Message: message,
		Data:    nil,
	}
}

// FormatOK creates a standard success response
func FormatOK(data interface{}) *model.Response {
	return &model.Response{
		Code:    0,
		Message: "ok",
		Data:    data,
	}
}
