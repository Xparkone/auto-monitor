// Package config 加载并校验 TOML 配置（对应 Python 版 mcp_inspect/config.py）。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

type ServerCfg struct {
	Name   string
	Host   string
	Via    string // local | ssh
	Mounts []string
}

type ServiceCfg struct {
	Name      string
	Deploy    string // binary | container | k8s（或 probes 注册表扩展类型）
	Host      string
	Port      int
	Process   string
	Container string
	K8s       map[string]any
}

type OutputCfg struct {
	ReportDir string
	LogDir    string
	BaseURL   string
}

type Thresholds struct {
	CPUWarn         float64
	MemWarn         float64
	DiskWarn        float64
	DiskCrit        float64
	LoadPerCoreWarn float64
	PodRestartWarn  int
}

// WebhookChannel 一条通知渠道：名称 + URL 列表（保持配置书写顺序）。
type WebhookChannel struct {
	Name string
	URLs []string
}

type Config struct {
	Root        string // 项目根目录
	Path        string
	ServerURL   string
	Token       string // 非空时，HTTP 接口要求 Authorization: Bearer <token>
	Output      OutputCfg
	Thresholds  Thresholds
	Servers     []ServerCfg
	Namespaces  []string
	Services    []ServiceCfg
	Channels    []WebhookChannel // webhook 渠道（保序，与配置文件一致）
	DatabaseURL string
	Certfile    string // 非空且与 Keyfile 成对时，MCP 服务以 HTTPS 监听
	Keyfile     string
	TLSVerify   bool           // 工作流访问 https 时是否校验证书；自签证书可设为 false
	Raw         map[string]any // 配置原文，扩展插件可读取自定义配置段
}

func resolve(root, value string) string {
	if strings.HasPrefix(value, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			value = home + value[1:]
		}
	}
	if filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(root, value)
}

func require(mapping map[string]any, key, where string) (any, error) {
	v, ok := mapping[key]
	if !ok || v == nil || v == "" {
		return nil, fmt.Errorf("配置错误：%s 缺少必填字段 %s", where, key)
	}
	return v, nil
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asStringMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func toFloat(v any, def float64) float64 {
	switch n := v.(type) {
	case int64:
		return float64(n)
	case float64:
		return n
	default:
		return def
	}
}

func toInt(v any, def int) int {
	switch n := v.(type) {
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return def
	}
}

func toStringList(v any) []string {
	if s, ok := v.(string); ok {
		return []string{s}
	}
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Load 读取 path 指向的 TOML 配置并做逐项校验。validDeploy 用于校验 services[].deploy，
// 由调用方传入已注册的部署类型（通常在 probes 包的注册表）。
func Load(path string, validDeploy func(string) bool) (*Config, error) {
	configPath, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(configPath)
	if err != nil || info.IsDir() {
		return nil, fmt.Errorf("配置文件不存在: %s", configPath)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var data map[string]any
	if _, err := toml.Decode(string(raw), &data); err != nil {
		return nil, fmt.Errorf("配置解析失败: %w", err)
	}
	// 约定：配置文件位于 <root>/config/ 下；否则以配置文件所在目录为根。
	root := filepath.Dir(configPath)
	if filepath.Base(root) == "config" {
		root = filepath.Dir(root)
	}

	out := asStringMap(data["output"])
	th := asStringMap(data["thresholds"])
	wh := asStringMap(data["webhook"])

	servers := []ServerCfg{}
	for i, item := range toAnyList(data["servers"]) {
		raw := asStringMap(item)
		where := fmt.Sprintf("servers[%d]", i)
		via := asString(raw["via"])
		if via == "" {
			via = "ssh"
		}
		if via != "local" && via != "ssh" {
			return nil, fmt.Errorf("配置错误：%s.via 只能是 local 或 ssh，当前为 %s", where, via)
		}
		name, err := require(raw, "name", where)
		if err != nil {
			return nil, err
		}
		host, err := require(raw, "host", where)
		if err != nil {
			return nil, err
		}
		mounts := toStringList(raw["mounts"])
		if len(mounts) == 0 {
			mounts = []string{"/"}
		}
		servers = append(servers, ServerCfg{
			Name: asString(name), Host: asString(host), Via: via, Mounts: mounts,
		})
	}

	namespaces := []string{}
	for i, item := range toAnyList(data["namespaces"]) {
		raw := asStringMap(item)
		name, err := require(raw, "name", fmt.Sprintf("namespaces[%d]", i))
		if err != nil {
			return nil, err
		}
		namespaces = append(namespaces, asString(name))
	}

	services := []ServiceCfg{}
	for i, item := range toAnyList(data["services"]) {
		raw := asStringMap(item)
		where := fmt.Sprintf("services[%d]", i)
		deployV, err := require(raw, "deploy", where)
		if err != nil {
			return nil, err
		}
		deploy := asString(deployV)
		if validDeploy != nil && !validDeploy(deploy) {
			return nil, fmt.Errorf("配置错误：%s.deploy 未注册，当前为 %s", where, deploy)
		}
		name, err := require(raw, "name", where)
		if err != nil {
			return nil, err
		}
		portV, err := require(raw, "port", where)
		if err != nil {
			return nil, err
		}
		services = append(services, ServiceCfg{
			Name:      asString(name),
			Deploy:    deploy,
			Host:      asString(raw["host"]),
			Port:      toInt(portV, 0),
			Process:   asString(raw["process"]),
			Container: asString(raw["container"]),
			K8s:       asStringMap(raw["k8s"]),
		})
	}

	var channels []WebhookChannel
	hasChannel := func(name string) bool {
		for _, c := range channels {
			if c.Name == name {
				return true
			}
		}
		return false
	}
	for name, v := range asStringMap(wh["channels"]) {
		if urls := toStringList(v); len(urls) > 0 {
			channels = append(channels, WebhookChannel{Name: name, URLs: urls})
		}
	}
	// 兼容旧写法：feishu_url / extra_urls 折叠进 channels
	if v := asString(wh["feishu_url"]); v != "" && !hasChannel("feishu") {
		channels = append(channels, WebhookChannel{Name: "feishu", URLs: []string{v}})
	}
	if v := wh["extra_urls"]; v != nil && !hasChannel("generic") {
		if urls := toStringList(v); len(urls) > 0 {
			channels = append(channels, WebhookChannel{Name: "generic", URLs: urls})
		}
	}

	server := asStringMap(data["server"])
	// 环境变量优先，避免把口令写进可提交的配置文件
	token := strings.TrimSpace(os.Getenv("INSPECTION_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(asString(server["token"]))
	}
	databaseURL := strings.TrimSpace(os.Getenv("INSPECTION_DATABASE_URL"))
	if databaseURL == "" {
		databaseURL = strings.TrimSpace(asString(asStringMap(data["database"])["url"]))
	}
	certfile := strings.TrimSpace(asString(server["certfile"]))
	keyfile := strings.TrimSpace(asString(server["keyfile"]))
	if (certfile != "") != (keyfile != "") {
		return nil, fmt.Errorf("配置错误：[server] 的 certfile 和 keyfile 必须同时填写")
	}
	if certfile != "" {
		certfile = resolve(root, certfile)
		keyfile = resolve(root, keyfile)
		for label, p := range map[string]string{"certfile": certfile, "keyfile": keyfile} {
			if info, err := os.Stat(p); err != nil || info.IsDir() {
				return nil, fmt.Errorf("配置错误：[server].%s 文件不存在: %s", label, p)
			}
		}
	}

	serverURL := strings.TrimRight(asString(server["url"]), "/")
	if serverURL == "" {
		serverURL = "http://127.0.0.1:8765"
	}
	baseURL := strings.TrimRight(asString(out["base_url"]), "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8765"
	}
	tlsVerify := true
	if v, ok := server["tls_verify"].(bool); ok {
		tlsVerify = v
	}
	if v, ok := server["tls_verify"].(string); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			tlsVerify = b
		}
	}

	return &Config{
		Root:      root,
		Path:      configPath,
		ServerURL: serverURL,
		Token:     token,
		Output: OutputCfg{
			ReportDir: resolve(root, defaultString(asString(out["report_dir"]), "reports")),
			LogDir:    resolve(root, defaultString(asString(out["log_dir"]), "logs")),
			BaseURL:   baseURL,
		},
		Thresholds: Thresholds{
			CPUWarn:         toFloat(th["cpu_warn"], 80),
			MemWarn:         toFloat(th["mem_warn"], 80),
			DiskWarn:        toFloat(th["disk_warn"], 75),
			DiskCrit:        toFloat(th["disk_crit"], 90),
			LoadPerCoreWarn: toFloat(th["load_per_core_warn"], 2),
			PodRestartWarn:  toInt(th["pod_restart_warn"], 5),
		},
		Servers:     servers,
		Namespaces:  namespaces,
		Services:    services,
		Channels:    channels,
		DatabaseURL: databaseURL,
		Certfile:    certfile,
		Keyfile:     keyfile,
		TLSVerify:   tlsVerify,
		Raw:         data,
	}, nil
}

func toAnyList(v any) []any {
	if items, ok := v.([]any); ok {
		return items
	}
	if items, ok := v.([]map[string]any); ok {
		out := make([]any, 0, len(items))
		for _, m := range items {
			out = append(out, m)
		}
		return out
	}
	return nil
}

func defaultString(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
