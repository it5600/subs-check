package proxies

import "strings"

// validClientFingerprints mirrors the fingerprint table of mihomo's
// component/tls package (utls.go). Upstream subscriptions frequently carry
// values such as "unsafe" that mihomo does not know; every such node makes
// mihomo log "wrong clientFingerprint:..." while the adapter is built, which
// flooded the application log and left an invalid client-fingerprint in the
// exported YAML.
var validClientFingerprints = map[string]bool{
    "chrome":                     true,
    "firefox":                    true,
    "safari":                     true,
    "ios":                        true,
    "android":                    true,
    "edge":                       true,
    "360":                        true,
    "qq":                         true,
    "random":                     true,
    "chrome120":                  true,
    "firefox120":                 true,
    "safari16":                   true,
    "chrome_psk":                 true,
    "chrome_psk_shuffle":         true,
    "chrome_padding_psk_shuffle": true,
    "chrome_pq":                  true,
    "chrome_pq_psk":              true,
    "randomized":                 true,
}

// sanitizeProxy drops the fields mihomo would reject.
//
// Only "client-fingerprint" is normalized today: an unknown value means
// "no fingerprint" for mihomo (GetFingerprint returns false for an empty
// value), so removing the key keeps the connection semantics and removes
// both the log warning and the invalid field from the exported subscription.
func sanitizeProxy(proxy map[string]any) {
    value, ok := proxy["client-fingerprint"].(string)
    if !ok {
        return
    }
    normalized := strings.ToLower(strings.TrimSpace(value))
    if normalized == "" || normalized == "none" {
        delete(proxy, "client-fingerprint")
        return
    }
    if !validClientFingerprints[normalized] {
        delete(proxy, "client-fingerprint")
    }
}
