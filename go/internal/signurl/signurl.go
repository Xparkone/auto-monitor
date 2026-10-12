// Package signurl 报告下载链接的 HMAC 签名与 Bearer token 校验
// （对应 Python 版 mcp_inspect/server.py 中的对应函数，拆出以便 runner 与 server 共用）。
package signurl

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DownloadTTLSeconds 签名链接有效期：默认 7 天。
const DownloadTTLSeconds = 7 * 24 * 3600

// DownloadURLFor 生成浏览器能直接打开的报告地址。配了 token 时附上过期时间和签名，
// 不把 token 明文放进链接。
func DownloadURLFor(baseURL, filename, token string) string {
	u := fmt.Sprintf("%s/reports/%s", strings.TrimRight(baseURL, "/"), url.PathEscape(filename))
	if token == "" {
		return u
	}
	exp := time.Now().Unix() + DownloadTTLSeconds
	sig := sign(filename, token, exp)
	return fmt.Sprintf("%s?exp=%d&sig=%s", u, exp, sig)
}

// DownloadSigOK 校验签名与过期时间。
func DownloadSigOK(filename, token, expStr, sig string, now time.Time) bool {
	if token == "" || expStr == "" || sig == "" {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || exp < now.Unix() {
		return false
	}
	expected := sign(filename, token, exp)
	return hmac.Equal([]byte(expected), []byte(sig))
}

func sign(filename, token string, exp int64) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(fmt.Sprintf("%s\n%d", filename, exp)))
	return hex.EncodeToString(mac.Sum(nil))
}

// BearerToken 从 `Authorization: Bearer <token>` 取出 token；格式不对返回空串。
func BearerToken(headerValue string) string {
	scheme, value, _ := strings.Cut(headerValue, " ")
	if !strings.EqualFold(scheme, "bearer") || strings.TrimSpace(value) == "" {
		return ""
	}
	return strings.TrimSpace(value)
}

// TokenOK 校验 token。expected 为空表示未启用校验。比较用 hmac，避免按字符提前返回。
func TokenOK(presented, expected string) bool {
	if expected == "" {
		return true
	}
	if presented == "" {
		return false
	}
	return hmac.Equal([]byte(presented), []byte(expected))
}
