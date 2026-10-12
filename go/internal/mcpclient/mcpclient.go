// Package mcpclient MCP 客户端最小实现：JSON-RPC over HTTP/HTTPS
// （对应 Python 版 inspect_runner.py 中的 mcp_request / client_ssl_context）。
package mcpclient

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

type Client struct {
	url   string
	token string
	http  *http.Client
}

// New 构造客户端。http 地址无需 TLS 配置；https 时按 tlsVerify 校验证书，
// 自签证书把 tlsVerify 设为 false，公网 CA 证书可用 cafile 指定。
func New(serverURL, token string, tlsVerify bool, cafile string) (*Client, error) {
	client := &Client{url: strings.TrimRight(serverURL, "/"), token: token,
		http: &http.Client{Timeout: 120 * time.Second}}
	if !strings.HasPrefix(client.url, "https://") {
		return client, nil
	}
	tlsCfg := &tls.Config{InsecureSkipVerify: !tlsVerify} //nolint:gosec // 由配置显式控制
	if tlsVerify && cafile != "" {
		pem, err := os.ReadFile(cafile)
		if err != nil {
			return nil, fmt.Errorf("读取 CA 证书失败: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA 证书无法解析: %s", cafile)
		}
		tlsCfg.RootCAs = pool
	}
	client.http.Transport = &http.Transport{TLSClientConfig: tlsCfg}
	return client, nil
}

// Call 发起一次 JSON-RPC 调用并返回 result；method 如 tools/list、tools/call。
func (c *Client) Call(method string, params any) (any, error) {
	if params == nil {
		params = map[string]any{}
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var payload struct {
		Result any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("响应解析失败: %w", err)
	}
	if payload.Error != nil {
		return nil, fmt.Errorf("%s", payload.Error.Message)
	}
	return payload.Result, nil
}

// CallTool 调 tools/call 并取出 content[0].json；调用方负责处理降级。
func (c *Client) CallTool(name string, arguments map[string]any) (map[string]any, error) {
	result, err := c.Call("tools/call", map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		return nil, err
	}
	resultMap, ok := result.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tools/call 响应格式错误")
	}
	content, ok := resultMap["content"].([]any)
	if !ok || len(content) == 0 {
		return nil, fmt.Errorf("tools/call 响应缺少 content")
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tools/call content 格式错误")
	}
	payload, ok := first["json"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tools/call content 缺少 json")
	}
	return payload, nil
}

// ListTools 返回 tools/list 的工具规格列表。
func (c *Client) ListTools() ([]map[string]any, error) {
	result, err := c.Call("tools/list", nil)
	if err != nil {
		return nil, err
	}
	resultMap, ok := result.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tools/list 响应格式错误")
	}
	tools := []map[string]any{}
	for _, item := range toSlice(resultMap["tools"]) {
		if m, ok := item.(map[string]any); ok {
			tools = append(tools, m)
		}
	}
	return tools, nil
}

func toSlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}
