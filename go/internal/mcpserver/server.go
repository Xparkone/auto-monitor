// Package mcpserver 巡检 MCP 服务：tools/list、tools/call（JSON-RPC）+ 报告 HTTP 接口
// （对应 Python 版 mcp_inspect/server.py）。
//
//   - POST /         JSON-RPC：tools/list、tools/call
//   - POST /inspect  触发一次巡检（进程内执行），返回报告路径与下载链接
//   - GET  /reports           报告文件列表（JSON）
//   - GET  /reports/<filename> 以接口形式下载 Markdown 报告
//   - GET  /healthz           存活探针（不校验 token，不返回业务数据）
//
// 配置了 token（环境变量 INSPECTION_TOKEN，或 [server].token）后，其余接口必须带
// `Authorization: Bearer <token>`，否则返回 401。报告下载是浏览器直接打开的链接，带不上这个请求头，
// 因此飞书按钮使用由 token 签出的 exp 与 sig（只对这一份文件有效，默认 7 天）。访问日志不记录 query。
package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"auto-monitor/internal/config"
	"auto-monitor/internal/probes"
	"auto-monitor/internal/registry"
	"auto-monitor/internal/runner"
	"auto-monitor/internal/signurl"

	_ "auto-monitor/internal/tools" // 注册内置工具：tools/list、tools/call 由注册表驱动
)

type server struct {
	configPath string
	token      string
	log        *fileLog
}

type fileLog struct {
	mu sync.Mutex
	f  *os.File
}

func newFileLog(logDir string) (*fileLog, string, error) {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, "", err
	}
	path := filepath.Join(logDir, "server.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, "", err
	}
	return &fileLog{f: f}, path, nil
}

func (l *fileLog) printf(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	ts := now.Format("2006-01-02 15:04:05") + fmt.Sprintf(",%03d", now.Nanosecond()/1e6)
	fmt.Fprintf(l.f, "%s %s %s\n", ts, level, fmt.Sprintf(format, args...))
	// 服务端日志同步到控制台，便于直接观察
	fmt.Fprintf(os.Stderr, "%s %s %s\n", ts, level, fmt.Sprintf(format, args...))
}

func (l *fileLog) infof(format string, args ...any)  { l.printf("INFO", format, args...) }
func (l *fileLog) warnf(format string, args ...any)  { l.printf("WARNING", format, args...) }
func (l *fileLog) errorf(format string, args ...any) { l.printf("ERROR", format, args...) }

// Run 启动 MCP 服务（阻塞）。host/port 为监听地址。
func Run(configPath, host string, port int) error {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Load(abs, probes.Valid)
	if err != nil {
		return err
	}
	log, logFile, err := newFileLog(cfg.Output.LogDir)
	if err != nil {
		return err
	}
	s := &server{configPath: abs, token: cfg.Token, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.route)

	srv := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", host, port),
		Handler: http.HandlerFunc(s.withAccessLog(mux.ServeHTTP)),
	}

	scheme := "http"
	if cfg.Token != "" {
		log.infof("token 校验已启用（除 /healthz 外需要 Authorization: Bearer）")
	} else {
		log.warnf("未配置 token，接口不校验身份。设置 INSPECTION_TOKEN 或 [server].token 后才会拒绝未授权访问")
	}
	if cfg.Certfile != "" && cfg.Keyfile != "" {
		scheme = "https"
	}
	log.infof("巡检 MCP 服务监听于 %s://%s:%d（config: %s，日志: %s）",
		scheme, host, port, configPath, logFile)
	if scheme == "https" {
		return srv.ListenAndServeTLS(cfg.Certfile, cfg.Keyfile)
	}
	return srv.ListenAndServe()
}

// withAccessLog 只记路径，不记 query，避免 token 出现在访问日志里。
func (s *server) withAccessLog(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next(rec, r)
		s.log.infof("%s %s %s -> %d", remoteIP(r), r.Method, r.URL.Path, rec.status)
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func remoteIP(r *http.Request) string {
	host, _, err := splitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func splitHostPort(addr string) (string, string, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return addr, "", fmt.Errorf("no port")
	}
	return addr[:i], addr[i+1:], nil
}

func (s *server) route(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.doPost(w, r)
	case http.MethodGet:
		s.doGet(w, r)
	default:
		s.sendJSON(w, map[string]any{"error": "method not allowed"}, http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------- 工具方法

func (s *server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if signurl.TokenOK(signurl.BearerToken(r.Header.Get("Authorization")), s.token) {
		return true
	}
	s.sendJSON(w, map[string]any{"error": "unauthorized"}, http.StatusUnauthorized)
	return false
}

func (s *server) sendJSON(w http.ResponseWriter, payload any, status int) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprint(b.Len()))
	w.WriteHeader(status)
	_, _ = w.Write(b.Bytes())
}

func (s *server) reportDir() (string, error) {
	cfg, err := config.Load(s.configPath, probes.Valid)
	if err != nil {
		return "", err
	}
	return cfg.Output.ReportDir, nil
}

func readBody(r *http.Request) (map[string]any, error) {
	if r.ContentLength <= 0 {
		if r.ContentLength == 0 {
			return map[string]any{}, nil
		}
	}
	defer r.Body.Close()
	var body map[string]any
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil {
		if err.Error() == "EOF" {
			return map[string]any{}, nil
		}
		return nil, err
	}
	return body, nil
}

// ---------------------------------------------------------------- POST

func (s *server) doPost(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	switch r.URL.Path {
	case "/", "":
		s.handleJSONRPC(w, r)
	case "/inspect":
		s.handleInspect(w, r)
	default:
		s.sendJSON(w, map[string]any{"error": fmt.Sprintf("unknown path: %s", r.URL.Path)}, http.StatusNotFound)
	}
}

func (s *server) handleJSONRPC(w http.ResponseWriter, r *http.Request) {
	request, err := readBody(r)
	if err != nil {
		s.sendJSON(w, map[string]any{"jsonrpc": "2.0", "id": nil,
			"error": map[string]any{"message": err.Error()}}, http.StatusBadRequest)
		return
	}
	method, _ := request["method"].(string)
	var result any
	switch method {
	case "tools/list":
		result = map[string]any{"tools": registry.ToolSpecs()}
	case "tools/call":
		result, err = s.callTool(request["params"])
		if err != nil {
			s.sendJSON(w, map[string]any{"jsonrpc": "2.0", "id": request["id"],
				"error": map[string]any{"message": err.Error()}}, http.StatusBadRequest)
			return
		}
	default:
		s.sendJSON(w, map[string]any{"jsonrpc": "2.0", "id": request["id"],
			"error": map[string]any{"message": fmt.Sprintf("unknown method: %s", method)}}, http.StatusBadRequest)
		return
	}
	s.sendJSON(w, map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": result}, http.StatusOK)
}

func (s *server) callTool(paramsAny any) (map[string]any, error) {
	params, _ := paramsAny.(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	name, _ := params["name"].(string)
	arguments, _ := params["arguments"].(map[string]any)
	start := time.Now()
	result, err := registry.Call(name, arguments)
	if err != nil {
		s.log.warnf("tools/call %s 拒绝: %s", name, err)
		return nil, err
	}
	dur := time.Since(start).Milliseconds()
	errText := "无"
	if errs, ok := result["errors"]; ok && errs != nil {
		if list, isSlice := toStringSlice(errs); isSlice && len(list) > 0 {
			errText = jsonForLog(list)
		}
	}
	s.log.infof("tools/call %-22s %5dms errors=%s", name, dur, errText)
	return map[string]any{"content": []any{map[string]any{"type": "json", "json": result}}}, nil
}

func toStringSlice(v any) ([]string, bool) {
	items, ok := v.([]any)
	if !ok {
		if strs, isStrs := v.([]string); isStrs {
			return strs, true
		}
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, fmt.Sprint(item))
	}
	return out, true
}

// toStringSliceOr 容忍单字符串或字符串数组；字符串按逗号拆分（同 Python CLI）。
func toStringSliceOr(v any, def []string) []string {
	if s, ok := v.(string); ok {
		var parts []string
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				parts = append(parts, p)
			}
		}
		if len(parts) == 0 {
			return def
		}
		return parts
	}
	if list, ok := toStringSlice(v); ok {
		return list
	}
	return def
}

func jsonForLog(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimRight(b.String(), "\n")
}

func (s *server) handleInspect(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		s.sendJSON(w, map[string]any{"jsonrpc": "2.0", "id": nil,
			"error": map[string]any{"message": err.Error()}}, http.StatusBadRequest)
		return
	}
	s.log.infof("/inspect 触发: %s", jsonForLog(body))

	configPath := s.configPath
	if v, ok := body["config"].(string); ok && v != "" {
		configPath = v
	}
	notifyOn := true
	if v, ok := body["notify"].(bool); ok {
		notifyOn = v
	}
	opts := runner.Options{
		ServersSel:    toStringSliceOr(body["servers"], nil),
		NamespacesSel: toStringSliceOr(body["namespaces"], nil),
		ServicesSel:   toStringSliceOr(body["services"], nil),
		SectionsSel:   toStringSliceOr(body["sections"], nil),
		NotifyOn:      notifyOn,
	}

	type runResult struct {
		summary map[string]any
		err     error
	}
	done := make(chan runResult, 1)
	go func() {
		cfg, err := config.Load(configPath, probes.Valid)
		if err != nil {
			done <- runResult{nil, err}
			return
		}
		summary, err := runner.RunInspection(cfg, opts)
		done <- runResult{summary, err}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			s.log.errorf("/inspect 执行失败: %s", res.err)
			s.sendJSON(w, map[string]any{"error": res.err.Error()}, http.StatusInternalServerError)
			return
		}
		summary := res.summary
		s.log.infof("/inspect 完成: %v %v 报告=%v",
			summary["overall_emoji"], summary["counts"], summary["report_name"])
		s.sendJSON(w, summary, http.StatusOK)
	case <-time.After(600 * time.Second):
		s.log.errorf("/inspect 执行超时(600s): %s", jsonForLog(body))
		s.sendJSON(w, map[string]any{"error": "巡检执行超时(600s)"}, http.StatusInternalServerError)
	}
}

// ---------------------------------------------------------------- GET

func (s *server) doGet(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		s.sendJSON(w, map[string]any{"status": "ok"}, http.StatusOK)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/reports/") && r.URL.Path != "/reports/" {
		s.downloadReport(w, r, strings.TrimPrefix(r.URL.Path, "/reports/"))
		return
	}
	if !s.authorized(w, r) {
		return
	}
	if r.URL.Path == "/reports" {
		s.listReports(w)
		return
	}
	s.sendJSON(w, map[string]any{"error": fmt.Sprintf("unknown path: %s", r.URL.Path)}, http.StatusNotFound)
}

func (s *server) listReports(w http.ResponseWriter) {
	dir, err := s.reportDir()
	if err != nil {
		s.sendJSON(w, map[string]any{"error": err.Error()}, http.StatusInternalServerError)
		return
	}
	type fileInfo struct {
		name  string
		size  int64
		mtime time.Time
	}
	var infos []fileInfo
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		infos = append(infos, fileInfo{entry.Name(), info.Size(), info.ModTime()})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].mtime.After(infos[j].mtime) })
	files := []map[string]any{}
	for _, info := range infos {
		files = append(files, map[string]any{
			"name":         info.name,
			"size":         info.size,
			"download_url": "/reports/" + url.PathEscape(info.name),
		})
	}
	s.sendJSON(w, map[string]any{"reports": files}, http.StatusOK)
}

// downloadAllowed 请求头里的 Bearer，或链接上的签名，二者任一通过即可。未配置 token 则放行。
func (s *server) downloadAllowed(w http.ResponseWriter, r *http.Request, filename string) bool {
	if signurl.TokenOK(signurl.BearerToken(r.Header.Get("Authorization")), s.token) {
		return true
	}
	exp := r.URL.Query().Get("exp")
	sig := r.URL.Query().Get("sig")
	if signurl.DownloadSigOK(filename, s.token, exp, sig, time.Now()) {
		return true
	}
	s.sendJSON(w, map[string]any{"error": "unauthorized"}, http.StatusUnauthorized)
	return false
}

func (s *server) downloadReport(w http.ResponseWriter, r *http.Request, rawName string) {
	decoded, err := url.PathUnescape(rawName)
	if err != nil {
		s.sendJSON(w, map[string]any{"error": "文件名无法解码"}, http.StatusBadRequest)
		return
	}
	name := path.Base(decoded)
	if !s.downloadAllowed(w, r, name) {
		return
	}
	if !strings.HasSuffix(name, ".md") {
		s.sendJSON(w, map[string]any{"error": "仅支持下载 .md 报告"}, http.StatusBadRequest)
		return
	}
	dir, err := s.reportDir()
	if err != nil {
		s.sendJSON(w, map[string]any{"error": err.Error()}, http.StatusInternalServerError)
		return
	}
	target := filepath.Join(dir, name)
	body, err := os.ReadFile(target)
	if err != nil {
		s.sendJSON(w, map[string]any{"error": fmt.Sprintf("报告不存在: %s", name)}, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename*=UTF-8''%s", url.PathEscape(name)))
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
