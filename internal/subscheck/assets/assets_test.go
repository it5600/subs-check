package assets

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestAssetsFingerprint_Stable 指纹必须稳定，否则每次启动都会重新解压
func TestAssetsFingerprint_Stable(t *testing.T) {
	if assetsFingerprint() != assetsFingerprint() {
		t.Fatalf("资源指纹不稳定")
	}
}

// TestFingerprintAsset_Changes 资源内容或长度变化时指纹必须变化
func TestFingerprintAsset_Changes(t *testing.T) {
	calc := func(b []byte) string {
		h := sha256.New()
		fingerprintAsset(h, b)
		return hex.EncodeToString(h.Sum(nil))
	}
	base := make([]byte, 4*fingerprintChunk)
	for i := range base {
		base[i] = byte(i)
	}
	if calc(base) != calc(append([]byte(nil), base...)) {
		t.Fatalf("相同内容应得到相同指纹")
	}
	changed := append([]byte(nil), base...)
	changed[len(changed)-1]++
	if calc(base) == calc(changed) {
		t.Fatalf("尾部变化应改变指纹")
	}
	if calc(base) == calc(base[:len(base)-1]) {
		t.Fatalf("长度变化应改变指纹")
	}
}
