// orproxy.go - OpenRouter 本地多账号代理（Go 版本）
// 交叉编译：GOOS=windows GOARCH=amd64 go build -o orproxy.exe orproxy.go
//           GOOS=linux   GOARCH=amd64 go build -o orproxy orproxy.go
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ============ Key 管理器 ============
type keyManager struct {
	mu         sync.RWMutex
	pausedKeys map[string]bool // key前12位 -> true
}

func newKeyManager() *keyManager {
	return &keyManager{pausedKeys: make(map[string]bool)}
}

func (km *keyManager) isPaused(key string) bool {
	km.mu.RLock()
	defer km.mu.RUnlock()
	return km.pausedKeys[key[:min(12, len(key))]]
}

func (km *keyManager) pause(key string) {
	km.mu.Lock()
	defer km.mu.Unlock()
	km.pausedKeys[key[:min(12, len(key))]] = true
}

func (km *keyManager) resume(key string) {
	km.mu.Lock()
	defer km.mu.Unlock()
	delete(km.pausedKeys, key[:min(12, len(key))])
}

func (km *keyManager) getPausedCount() int {
	km.mu.RLock()
	defer km.mu.RUnlock()
	return len(km.pausedKeys)
}

//go:embed orproxy-dashboard.html
var dashboardHTML embed.FS

// ============ 配置 ============
const (
	defaultPort     = "8787"
	defaultBind     = "127.0.0.1" // 远程访问时设为 0.0.0.0
	defaultUpstream = "openrouter.ai"
	defaultRefreshMin = 15         // 免费模型刷新间隔（分钟）
	defaultAutoExit   = 0          // 0=不自动退出
	defaultConfigFile = "orproxy.yaml"       // 主配置
	defaultCredsFile  = "orproxy-creds.yaml" // 凭据
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ============ 日志缓冲（支持 SSE 推送）============
const maxLogLines = 500

type logLine struct {
	Ts   string `json:"ts"`
	Msg  string `json:"msg"`
	Kind string `json:"kind"` // "ok","warn","err","info","req"
}

type logBuffer struct {
	mu     sync.RWMutex
	lines  []logLine
	sse    map[chan string]struct{}
	sseMu  sync.Mutex
}

var globalLog = &logBuffer{
	lines: make([]logLine, 0, maxLogLines),
	sse:   make(map[chan string]struct{}),
}

var (
	logger   *log.Logger
	logMutex sync.Mutex
)

func init() {
	logger = log.New(os.Stdout, "", 0)
}

func nowTs() string {
	return time.Now().In(time.FixedZone("CST", 8*3600)).Format("15:04:05")
}

// classify 解析日志消息判断类型（用于前端染色）
func classify(msg string) string {
	switch {
	case strings.HasPrefix(msg, "❌"), strings.HasPrefix(msg, "⛔"):
		return "err"
	case strings.HasPrefix(msg, "⚠️"), strings.HasPrefix(msg, "⚠ "):
		return "warn"
	case strings.HasPrefix(msg, "✅"), strings.HasPrefix(msg, "✅ "):
		return "ok"
	case strings.HasPrefix(msg, "💬"), strings.HasPrefix(msg, "🎁"), strings.HasPrefix(msg, "📄"), strings.HasPrefix(msg, "🚀"), strings.HasPrefix(msg, "🩺"), strings.HasPrefix(msg, "📊"), strings.HasPrefix(msg, "🔐"):
		return "info"
	default:
		return ""
	}
}

func logf(format string, v ...interface{}) {
	msg := fmt.Sprintf(format, v...)
	line := fmt.Sprintf("[%s] %s", nowTs(), msg)
	ts := nowTs()
	kind := classify(msg)

	logLine := logLine{Ts: ts, Msg: msg, Kind: kind}

	logMutex.Lock()
	logger.Print(line)
	logMutex.Unlock()

	// 写缓冲
	globalLog.mu.Lock()
	globalLog.lines = append(globalLog.lines, logLine)
	if len(globalLog.lines) > maxLogLines {
		globalLog.lines = globalLog.lines[len(globalLog.lines)-maxLogLines:]
	}
	// 推 SSE
	payload := fmt.Sprintf("event: log\ndata: {\"ts\":\"%s\",\"msg\":%q,\"kind\":\"%s\"}\n\n", ts, msg, kind)
	globalLog.sseMu.Lock()
	for ch := range globalLog.sse {
		select {
		case ch <- payload:
		default: // 客户端来不及读则跳过
		}
	}
	globalLog.sseMu.Unlock()
	globalLog.mu.Unlock()
}

func (lb *logBuffer) getAll() []logLine {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	res := make([]logLine, len(lb.lines))
	copy(res, lb.lines)
	return res
}

func (lb *logBuffer) addSSE(ch chan string) {
	lb.sseMu.Lock()
	lb.sse[ch] = struct{}{}
	lb.sseMu.Unlock()
}

func (lb *logBuffer) removeSSE(ch chan string) {
	lb.sseMu.Lock()
	delete(lb.sse, ch)
	close(ch)
	lb.sseMu.Unlock()
}

// ============ 凭据解析 ============
type keyEntry struct {
	envName string
	key     string
	slot    string
}

func loadKeys(credPath string) ([]keyEntry, error) {
	data, err := os.ReadFile(credPath)
	if err != nil {
		return nil, fmt.Errorf("读凭据文件失败: %w", err)
	}

	var keys []keyEntry
	re := regexp.MustCompile(`(?m)^\s*(OPENROUTER(\d*)_API_KEY)\s*:\s*(\S+)`)
	matches := re.FindAllSubmatch(data, -1)
	for _, m := range matches {
		envName := string(m[1])
		slot := string(m[2])
		key := string(m[3])
		keys = append(keys, keyEntry{envName: envName, key: key, slot: slot})
	}

	sort.Slice(keys, func(i, j int) bool {
		if keys[i].envName == "OPENROUTER_API_KEY" {
			return true
		}
		if keys[j].envName == "OPENROUTER_API_KEY" {
			return false
		}
		return keys[i].slot < keys[j].slot
	})

	return keys, nil
}

func maskKey(k string) string {
	if len(k) < 4 {
		return "***"
	}
	return k[len(k)-4:]
}

// ============ 冷却管理 ============
type cooldownMgr struct {
	mu       sync.RWMutex
	coolings map[string]time.Time
}

func newCooldownMgr() *cooldownMgr {
	return &cooldownMgr{coolings: make(map[string]time.Time)}
}

func (c *cooldownMgr) isCooling(envName string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if t, ok := c.coolings[envName]; ok {
		return time.Now().Before(t)
	}
	return false
}

func (c *cooldownMgr) setCooling(envName string, until time.Time) {
	c.mu.Lock()
	c.coolings[envName] = until
	c.mu.Unlock()
}

func (c *cooldownMgr) readyKeys(keys []keyEntry) ([]keyEntry, string) {
	var ready []keyEntry
	for _, k := range keys {
		if !c.isCooling(k.envName) {
			ready = append(ready, k)
		}
	}
	if len(ready) > 0 {
		return ready, ""
	}
	soonest := keys[0]
	minTs := time.Now().Add(100 * time.Hour)
	for _, k := range keys {
		c.mu.RLock()
		if t, ok := c.coolings[k.envName]; ok && t.Before(minTs) {
			minTs = t
			soonest = k
		}
		c.mu.RUnlock()
	}
	return []keyEntry{soonest}, "all-cooling"
}

func parseCooldown(resp *http.Response, body []byte) time.Duration {
	if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
		if ts, err := strconv.ParseFloat(reset, 64); err == nil {
			if ts > 1e12 {
				ts /= 1000
			}
			t := time.Unix(int64(ts), 0)
			if t.After(time.Now()) {
				return time.Until(t) + 30*time.Second
			}
		}
	}
	var j struct {
		Error struct {
			Metadata struct {
				Headers struct {
					XRateLimitReset string `json:"X-RateLimit-Reset"`
				} `json:"headers"`
			} `json:"metadata"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &j) == nil && j.Error.Metadata.Headers.XRateLimitReset != "" {
		if ts, err := strconv.ParseFloat(j.Error.Metadata.Headers.XRateLimitReset, 64); err == nil {
			if ts > 1e12 {
				ts /= 1000
			}
			t := time.Unix(int64(ts), 0)
			if t.After(time.Now()) {
				return time.Until(t) + 30*time.Second
			}
		}
	}
	return 90 * time.Second
}

// ============ 状态追踪 ============
type state struct {
	mu       sync.RWMutex
	lastGood string
}

func newState() *state { return &state{} }

func (s *state) setLastGood(envName string) {
	s.mu.Lock()
	s.lastGood = envName
	s.mu.Unlock()
}

func (s *state) rotateStart(keys []keyEntry) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lastGood == "" {
		return 0
	}
	for i, k := range keys {
		if k.envName == s.lastGood {
			return (i + 1) % len(keys)
		}
	}
	return 0
}

// ============ 免费模型缓存 ============
type FreeModel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextLength int    `json:"context_length"`
}

func isFreeModel(m map[string]interface{}) bool {
	pricing, ok := m["pricing"].(map[string]interface{})
	if !ok {
		return false
	}
	promptStr := fmt.Sprintf("%v", pricing["prompt"])
	compStr := fmt.Sprintf("%v", pricing["completion"])
	return promptStr == "0" && compStr == "0"
}

type freeModelsCache struct {
	mu         sync.RWMutex
	models     []FreeModel
	lastUpdate time.Time
	fetchErr   error
}

func newFreeModelsCache() *freeModelsCache {
	return &freeModelsCache{}
}

func (c *freeModelsCache) fetch(key string) {
	upstreamURL := "https://openrouter.ai/api/v1/models"
	req, err := http.NewRequest(http.MethodGet, upstreamURL, nil)
	if err != nil {
		c.mu.Lock()
		c.fetchErr = err
		c.mu.Unlock()
		return
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", "OpenRouter-Go-Proxy/1.0")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		c.mu.Lock()
		c.fetchErr = fmt.Errorf("network error: %w", err)
		c.mu.Unlock()
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.mu.Lock()
		c.fetchErr = fmt.Errorf("read error: %w", err)
		c.mu.Unlock()
		return
	}

	var j struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		c.mu.Lock()
		c.fetchErr = fmt.Errorf("parse error: %w", err)
		c.mu.Unlock()
		return
	}

	var free []FreeModel
	for _, m := range j.Data {
		if !isFreeModel(m) {
			continue
		}
		name, _ := m["name"].(string)
		id, _ := m["id"].(string)
		ctxLen := 0
		if cl, ok := m["context_length"].(float64); ok {
			ctxLen = int(cl)
		}
		free = append(free, FreeModel{
			ID:            id,
			Name:          name,
			ContextLength: ctxLen,
		})
	}

	c.mu.Lock()
	c.models = free
	c.lastUpdate = time.Now()
	c.fetchErr = nil
	c.mu.Unlock()

	logf("🎁 免费模型已刷新: %d 个", len(free))
}

func (c *freeModelsCache) get() ([]FreeModel, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.models, c.fetchErr == nil
}

func (c *freeModelsCache) startBackgroundRefresh(key string, interval time.Duration) {
	go func() {
		c.fetch(key)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			c.fetch(key)
		}
	}()
}

// ============ 心跳检测：页面关闭自动退出 ============
type heartbeatMgr struct {
	mu          sync.RWMutex
	lastBeat    time.Time
	pageOpen    bool
	autoExitSec int // 0=不自动退出，正数=超时秒数
}

func newHeartbeatMgr(autoExitSec int) *heartbeatMgr {
	return &heartbeatMgr{pageOpen: true, autoExitSec: autoExitSec, lastBeat: time.Now()}
}

func (h *heartbeatMgr) beat() {
	h.mu.Lock()
	h.lastBeat = time.Now()
	h.pageOpen = true
	h.mu.Unlock()
}

func (h *heartbeatMgr) pageClosed() {
	h.mu.Lock()
	h.pageOpen = false
	h.mu.Unlock()
}

func (h *heartbeatMgr) shouldExit() bool {
	if h.autoExitSec <= 0 {
		return false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if !h.pageOpen {
		return true // 收到 page-closed 信号
	}
	return time.Since(h.lastBeat) > time.Duration(h.autoExitSec)*time.Second
}

func (h *heartbeatMgr) startMonitor() {
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if h.shouldExit() {
				logf("⏹ 检测到 Dashboard 已关闭，自动退出")
				os.Exit(0)
			}
		}
	}()
}

// ============ Handler ============
type handler struct {
	upstream    string
	credPath    string
	bind        string
	authToken   string
	cooldown    *cooldownMgr
	state       *state
	freeModels  *freeModelsCache
	keysMu      sync.Mutex
	keyManager  *keyManager
	heartbeat   *heartbeatMgr
}

func (h *handler) loadKeys() ([]keyEntry, error) {
	h.keysMu.Lock()
	defer h.keysMu.Unlock()
	return loadKeys(h.credPath)
}

func (h *handler) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	if h.authToken == "" {
		return true
	}
	host := r.Host
	if strings.HasPrefix(host, "127.") || host == "localhost" || host == "[::1]" {
		return true
	}
	token := r.Header.Get("X-Auth-Token")
	if token == h.authToken {
		return true
	}
	if r.URL.Query().Get("token") == h.authToken {
		return true
	}
	http.Error(w, `{"error":{"message":"unauthorized"}}`, http.StatusUnauthorized)
	return false
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.checkAuth(w, r) {
		return
	}

	switch r.URL.Path {
	case "/":
		h.serveDashboard(w, r)
	case "/health":
		h.serveHealth(w, r)
	case "/v1/free-models":
		h.serveFreeModels(w, r)
	case "/v1/models":
		h.serveModels(w, r)
	case "/v1/chat/completions":
		h.proxyChat(w, r)
	case "/keys":
		h.manageKeys(w, r)
	case "/keys/add":
		h.addKey(w, r)
	case "/keys/remove":
		h.removeKey(w, r)
	case "/keys/pause":
		h.pauseKey(w, r)
	case "/keys/resume":
		h.resumeKey(w, r)
	case "/shutdown":
		h.shutdown(w, r)
	case "/heartbeat":
		h.heartbeatHandler(w, r)
	case "/page-closed":
		h.pageClosedHandler(w, r)
	case "/logs":
		h.serveLogs(w, r)
	default:
		http.Error(w, `{"error":{"message":"Not found"}}`, http.StatusNotFound)
	}
}

func (h *handler) serveDashboard(w http.ResponseWriter, r *http.Request) {
	data, err := dashboardHTML.ReadFile("orproxy-dashboard.html")
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(200)
		w.Write([]byte(`<!DOCTYPE html><html><head><meta charset="utf-8"></head>
<body><h1>proxy-dashboard.html not found</h1>
<p>Ensure the HTML file is in the same directory as the proxy binary.</p></body></html>`))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(200)
	w.Write(data)
}

func (h *handler) serveHealth(w http.ResponseWriter, r *http.Request) {
	keys, err := h.loadKeys()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	type keyStatus struct {
		Key          string `json:"key"`
		CoolingUntil string `json:"cooling_until"`
	}
	status := struct {
		Status   string      `json:"status"`
		Upstream string      `json:"upstream"`
		Keys     []keyStatus `json:"keys"`
	}{
		Status:   "ok",
		Upstream: h.upstream,
		Keys:     []keyStatus{},
	}
	for _, k := range keys {
		ks := keyStatus{Key: fmt.Sprintf("Key%s(%s)", k.slot, maskKey(k.key))}
		if h.cooldown.isCooling(k.envName) {
			h.cooldown.mu.RLock()
			if t, ok := h.cooldown.coolings[k.envName]; ok {
				ks.CoolingUntil = t.In(time.FixedZone("CST", 8*3600)).Format("15:04:05")
			}
			h.cooldown.mu.RUnlock()
		}
		status.Keys = append(status.Keys, ks)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func (h *handler) serveFreeModels(w http.ResponseWriter, r *http.Request) {
	models, ok := h.freeModels.get()
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(struct {
			Models []FreeModel `json:"models"`
			Stale  bool        `json:"stale"`
		}{Models: models, Stale: true})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	json.NewEncoder(w).Encode(struct {
		Models []FreeModel `json:"models"`
		Stale  bool        `json:"stale"`
	}{Models: models, Stale: false})
}

// ============ Key 管理 API ============
// GET  /keys          - 列出所有 Key 状态
// POST /keys/add       - 添加新 Key（写入 credentials.yaml）
// POST /keys/remove   - 删除 Key（写入 credentials.yaml）
// POST /keys/pause    - 暂停 Key（内存态，不写入文件）
// POST /keys/resume   - 恢复 Key（内存态）

func (h *handler) manageKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":{"message":"GET only"}}`, http.StatusMethodNotAllowed)
		return
	}
	keys, err := h.loadKeys()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%v"}}`, err.Error()), 500)
		return
	}
	type keyInfo struct {
		Name      string `json:"name"`
		Mask      string `json:"mask"`
		Paused    bool   `json:"paused"`
		Cooling   bool   `json:"cooling"`
		CoolUntil string `json:"cooling_until,omitempty"`
	}
	var list []keyInfo
	for _, k := range keys {
		ki := keyInfo{
			Name:    k.envName,
			Mask:    maskKey(k.key),
			Paused:  h.keyManager.isPaused(k.key),
			Cooling: h.cooldown.isCooling(k.envName),
		}
		if ki.Cooling {
			h.cooldown.mu.RLock()
			if t, ok := h.cooldown.coolings[k.envName]; ok {
				ki.CoolUntil = t.In(time.FixedZone("CST", 8*3600)).Format("15:04:05")
			}
			h.cooldown.mu.RUnlock()
		}
		list = append(list, ki)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"keys": list, "count": len(list)})
}

func (h *handler) addKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":{"message":"POST only"}}`, http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		EnvName string `json:"env_name"` // 可选；若不提供则自动分配
		Key     string `json:"key"`      // sk-or-v1-xxx
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, `{"error":{"message":"invalid json"}}`, 400)
		return
	}
	if req.Key == "" {
		http.Error(w, `{"error":{"message":"key required"}}`, 400)
		return
	}
	if !strings.HasPrefix(req.Key, "sk-or-v1-") {
		http.Error(w, `{"error":{"message":"invalid key format, must start with sk-or-v1-"}}`, 400)
		return
	}

	h.keysMu.Lock()
	defer h.keysMu.Unlock()

	// 若未指定 envName，自动分配：OPENROUTER_API_KEY (空) / OPENROUTER2/3/4...
	envName := req.EnvName
	if envName == "" {
		existing, _ := loadKeys(h.credPath)
		used := make(map[string]bool)
		maxSlot := 0
		hasPrimary := false
		for _, k := range existing {
			used[k.envName] = true
			if k.envName == "OPENROUTER_API_KEY" {
				hasPrimary = true
				continue
			}
			if n, err := strconv.Atoi(k.slot); err == nil && n > maxSlot {
				maxSlot = n
			}
		}
		if !hasPrimary {
			envName = "OPENROUTER_API_KEY"
		} else {
			envName = fmt.Sprintf("OPENROUTER%d_API_KEY", maxSlot+1)
		}
	}

	data, err := os.ReadFile(h.credPath)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"cannot read creds: %v"}}`, err), 500)
		return
	}

	if strings.Contains(string(data), envName) {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s already exists"}}`, envName), 409)
		return
	}

	newLine := fmt.Sprintf("\n%s: %s", envName, req.Key)
	if err := os.WriteFile(h.credPath, append(data, []byte(newLine)...), 0644); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"write failed: %v"}}`, err), 500)
		return
	}

	logf("➕ 添加 Key: %s", envName)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":   "ok",
		"message":  "Key added",
		"env_name": envName,
	})
}

func (h *handler) removeKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":{"message":"POST only"}}`, http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		EnvName string `json:"env_name"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, `{"error":{"message":"invalid json"}}`, 400)
		return
	}
	if req.EnvName == "" {
		http.Error(w, `{"error":{"message":"env_name required"}}`, 400)
		return
	}

	h.keysMu.Lock()
	defer h.keysMu.Unlock()

	data, err := os.ReadFile(h.credPath)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"cannot read creds: %v"}}`, err), 500)
		return
	}

	pattern := fmt.Sprintf(`(?m)^\s*%s\s*:\s*\S+.*$`, regexp.QuoteMeta(req.EnvName))
	re := regexp.MustCompile(pattern)
	newData := re.ReplaceAll(data, nil)
	if string(newData) == string(data) {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s not found"}}`, req.EnvName), 404)
		return
	}

	if err := os.WriteFile(h.credPath, newData, 0644); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"write failed: %v"}}`, err), 500)
		return
	}

	logf("🗑️ 删除 Key: %s", req.EnvName)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": "Key removed"})
}

func (h *handler) pauseKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":{"message":"POST only"}}`, http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Key string `json:"key"` // 完整 key / mask / envName
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, `{"error":{"message":"invalid json"}}`, 400)
		return
	}
	if req.Key == "" {
		http.Error(w, `{"error":{"message":"key required"}}`, 400)
		return
	}

	keys, err := h.loadKeys()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%v"}}`, err), 500)
		return
	}

	var found string
	for _, k := range keys {
		if strings.Contains(k.key, req.Key) || maskKey(k.key) == req.Key || k.envName == req.Key {
			h.keyManager.pause(k.key)
			found = k.envName
			logf("⏸️ 暂停 Key: %s", k.envName)
			break
		}
	}

	if found == "" {
		http.Error(w, `{"error":{"message":"key not found"}}`, 404)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": "Key paused", "env_name": found})
}

func (h *handler) resumeKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":{"message":"POST only"}}`, http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, `{"error":{"message":"invalid json"}}`, 400)
		return
	}
	if req.Key == "" {
		http.Error(w, `{"error":{"message":"key required"}}`, 400)
		return
	}

	keys, err := h.loadKeys()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%v"}}`, err), 500)
		return
	}

	var found string
	for _, k := range keys {
		if strings.Contains(k.key, req.Key) || maskKey(k.key) == req.Key || k.envName == req.Key {
			h.keyManager.resume(k.key)
			found = k.envName
			logf("▶️ 恢复 Key: %s", k.envName)
			break
		}
	}

	if found == "" {
		http.Error(w, `{"error":{"message":"key not found"}}`, 404)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": "Key resumed", "env_name": found})
}

// ============ 关闭程序 ============
func (h *handler) shutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":{"message":"POST only"}}`, http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	w.Write([]byte(`{"status":"shutting down"}`))

	logf("⏹ 收到关闭请求，3秒后退出…")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	go func() {
		time.Sleep(3 * time.Second)
		os.Exit(0)
	}()
}

func (h *handler) heartbeatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		// SSE: 持续推送心跳，保持连接
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(200)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		// 先发一次
		now := time.Now().In(time.FixedZone("CST", 8*3600)).Format("15:04:05")
		fmt.Fprintf(w, "data: %s\n\n", now)
		flusher.Flush()
		for range ticker.C {
			now := time.Now().In(time.FixedZone("CST", 8*3600)).Format("15:04:05")
			fmt.Fprintf(w, "data: %s\n\n", now)
			flusher.Flush()
		}
		return
	}
	// POST: 接收心跳
	h.heartbeat.beat()
	w.WriteHeader(200)
	w.Write([]byte(`{}`))
}

func (h *handler) pageClosedHandler(w http.ResponseWriter, r *http.Request) {
	h.heartbeat.pageClosed()
	w.WriteHeader(200)
	w.Write([]byte(`{}`))
}

func (h *handler) serveModels(w http.ResponseWriter, r *http.Request) {
	resp, err := h.doUpstream("", http.MethodGet, "/v1/models", nil)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	body, _ := io.ReadAll(resp.Body)
	h.copyResponse(w, resp, body)
}

func (h *handler) proxyChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":{"message":"failed to read body"}}`, 400)
		return
	}

	modelName := extractModel(body)

	keys, err := h.loadKeys()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%v"}}`, err.Error()), 500)
		return
	}

	start := h.state.rotateStart(keys)
	rotated := append(keys[start:], keys[:start]...)
	ready, note := h.cooldown.readyKeys(rotated)

	for i, k := range ready {
		if h.keyManager.isPaused(k.key) {
			logf("⏸️ [%s] %s 已暂停，跳过", modelName, maskKey(k.key))
			continue
		}
		if i > 0 || note == "all-cooling" {
			logf("⚠️ [%s] %s", modelName, note)
		}

		reqStart := time.Now()
		resp, err := h.doUpstream(k.key, http.MethodPost, "/v1/chat/completions", body)
		elapsed := time.Since(reqStart)

		if err != nil {
			logf("❌ [%s] %s 网络错误: %v", modelName, maskKey(k.key), err)
			continue
		}

		respBody, _ := io.ReadAll(resp.Body)
		statusCode := resp.StatusCode

		if statusCode == 200 {
			h.state.setLastGood(k.envName)
			tokens := extractTokens(respBody)
			logf("✅ [%s] %s %s [%.1fs, %s]",
				modelName, maskKey(k.key), tokens, elapsed.Seconds(), k.envName)
			h.copyResponse(w, resp, respBody)
			return
		}

		if statusCode == 429 {
			coolDur := parseCooldown(resp, respBody)
			until := time.Now().Add(coolDur)
			h.cooldown.setCooling(k.envName, until)
			logf("🔁 [%s] %s 429 → 冷却%.0f分钟，换下一条 [%s]",
				modelName, maskKey(k.key), coolDur.Minutes(), k.envName)
			continue
		}

		if statusCode == 402 || statusCode == 403 {
			logf("⛔ [%s] %s %d 付费额度不足，跳过 [%s]",
				modelName, maskKey(k.key), statusCode, k.envName)
			continue
		}

		detail := string(respBody)
		if len(detail) > 100 {
			detail = detail[:100] + "..."
		}
		logf("❌ [%s] %s HTTP %d: %s [%s]",
			modelName, maskKey(k.key), statusCode, detail, k.envName)
		if i == len(ready)-1 {
			h.copyResponse(w, resp, respBody)
			return
		}
	}

	http.Error(w, `{"error":{"message":"all keys exhausted"}}`, 502)
}

// extractModel 从请求体中提取模型名称
func extractModel(body []byte) string {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "?"
	}
	if m, ok := parsed["model"].(string); ok && m != "" {
		return m
	}
	return "?"
}

// extractTokens 从响应体中提取 token 使用量
func extractTokens(body []byte) string {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "?"
	}
	if usage, ok := parsed["usage"].(map[string]interface{}); ok {
		p := usage["prompt_tokens"]
		c := usage["completion_tokens"]
		t := usage["total_tokens"]
		return fmt.Sprintf("📥%v 📤%v 📐%v",
			intOr(p, 0), intOr(c, 0), intOr(t, 0))
	}
	return ""
}

func intOr(v interface{}, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return def
	}
}

func (h *handler) doUpstream(key, method, path string, body []byte) (*http.Response, error) {
	upstreamPath := path
	if strings.HasPrefix(path, "/v1") {
		upstreamPath = "/api" + path
	}

	upstreamURL := fmt.Sprintf("https://%s%s", h.upstream, upstreamPath)
	req, err := http.NewRequest(method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "OpenRouter-Go-Proxy/1.0")
	}

	client := &http.Client{Timeout: 180 * time.Second}
	return client.Do(req)
}

func (h *handler) copyResponse(w http.ResponseWriter, resp *http.Response, body []byte) {
	w.WriteHeader(resp.StatusCode)
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Write(body)
}

// ============ 配置加载 ============
// AppConfig 定义了 orproxy.yaml 的所有可配置项
type AppConfig struct {
	Port             string `yaml:"port"`
	Bind             string `yaml:"bind"`
	Upstream         string `yaml:"upstream"`
	RefreshMinutes   int    `yaml:"refresh_minutes"`
	AutoExitSec      int    `yaml:"auto_exit_sec"`
	AuthToken        string `yaml:"auth_token"`
	CredsPath        string `yaml:"creds_path"`
}

var defaultConfigYAML = `# ORProxy 配置文件
# 启动时若不存在则自动创建此文件
# 所有配置项均可通过同名环境变量覆盖（环境变量优先）

# 监听端口
port: "8787"

# 监听地址（127.0.0.1=仅本机，0.0.0.0=允许远程访问）
bind: "127.0.0.1"

# 上游服务器（代理目标域名或 IP）
upstream: "openrouter.ai"

# 免费模型列表刷新间隔（分钟）
refresh_minutes: 15

# 自动退出：Dashboard 关闭后多少秒退出（0=不自动退出）
auto_exit_sec: 0

# 远程认证 Token（留空则不认证，支持 header: X-Auth-Token 或 query: ?token=xxx）
auth_token: ""

# 凭据文件路径（Key 列表，与配置文件分离）
creds_path: "orproxy-creds.yaml"
`

func resolvePath(rel string) string {
	if filepath.IsAbs(rel) {
		return rel
	}
	execPath, _ := os.Executable()
	if execPath != "" {
		return filepath.Join(filepath.Dir(execPath), rel)
	}
	return rel
}

func loadConfig() AppConfig {
	cfg := AppConfig{
		Port:           env("PROXY_PORT", defaultPort),
		Bind:           env("BIND_ADDR", defaultBind),
		Upstream:       env("UPSTREAM_HOST", defaultUpstream),
		RefreshMinutes: defaultRefreshMin,
		AutoExitSec:    defaultAutoExit,
		AuthToken:      env("AUTH_TOKEN", ""),
		CredsPath:      env("CREDS_PATH", defaultCredsFile),
	}

	cfgPath := resolvePath(env("CONFIG_PATH", defaultConfigFile))
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if err := os.WriteFile(cfgPath, []byte(defaultConfigYAML), 0644); err != nil {
			logf("⚠️ 配置文件不存在，且自动创建失败: %v", err)
		} else {
			logf("📄 配置文件不存在，已自动创建: %s\n   ➜ 可编辑此文件调整配置，重启后生效", cfgPath)
		}
	} else {
		data, err := os.ReadFile(cfgPath)
		if err == nil {
			var fileCfg AppConfig
			if err := yaml.Unmarshal(data, &fileCfg); err == nil {
				// 环境变量已在 cfg 初始化时设置，此处仅在 env 未设置时才用 yaml
				if os.Getenv("PROXY_PORT") == "" && fileCfg.Port != "" {
					cfg.Port = fileCfg.Port
				}
				if os.Getenv("BIND_ADDR") == "" && fileCfg.Bind != "" {
					cfg.Bind = fileCfg.Bind
				}
				if os.Getenv("UPSTREAM_HOST") == "" && fileCfg.Upstream != "" {
					cfg.Upstream = fileCfg.Upstream
				}
				if fileCfg.RefreshMinutes > 0 {
					cfg.RefreshMinutes = fileCfg.RefreshMinutes
				}
				if fileCfg.AutoExitSec > 0 {
					cfg.AutoExitSec = fileCfg.AutoExitSec
				}
				if os.Getenv("AUTH_TOKEN") == "" && fileCfg.AuthToken != "" {
					cfg.AuthToken = fileCfg.AuthToken
				}
				if os.Getenv("CREDS_PATH") == "" && fileCfg.CredsPath != "" {
					cfg.CredsPath = fileCfg.CredsPath
				}
			}
		}
	}

	// 环境变量始终优先
	if v := os.Getenv("AUTO_EXIT_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.AutoExitSec = n
		}
	}

	return cfg
}

// ============ SSE 日志端点 ============
func (h *handler) serveLogs(w http.ResponseWriter, r *http.Request) {
	if h.authToken != "" && !h.checkAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "sse not supported", 500)
		return
	}

	// 先发送所有历史日志
	hist := globalLog.getAll()
	for _, l := range hist {
		fmt.Fprintf(w, "event: log\ndata: {\"ts\":\"%s\",\"msg\":%q,\"kind\":\"%s\"}\n\n", l.Ts, l.Msg, l.Kind)
	}
	flusher.Flush()

	ch := make(chan string, 64)
	globalLog.addSSE(ch)
	defer globalLog.removeSSE(ch)

	// 保持连接，每 25s 发一次空 comment 保活
	for {
		select {
		case payload, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprint(w, payload)
			flusher.Flush()
		case <-r.Context().Done():
			return
		case <-time.After(25 * time.Second):
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// ============ 入口 ============
func main() {
	cfg := loadConfig()
	credPath := resolvePath(cfg.CredsPath)

	if _, err := os.Stat(credPath); os.IsNotExist(err) {
		defaultCreds := `# OpenRouter API Key 代理配置文件
# 格式：OPENROUTER_API_KEY 或 OPENROUTER{N}_API_KEY : your-key
# OPENROUTER_API_KEY（无数字）优先级最高，会优先使用
# OPENROUTER2_API_KEY、OPENROUTER3_API_KEY … 按数字顺序轮询
# Key 格式应为 sk-or-v1-xxxx，Dashboard 的 Key 管理页面可查看

OPENROUTER_API_KEY: sk-or-v1-YOUR_KEY_HERE
`
		if err := os.WriteFile(credPath, []byte(defaultCreds), 0644); err != nil {
			logf("⚠️ 凭据文件不存在，且自动创建失败: %v", err)
		} else {
			logf("📄 凭据文件不存在，已自动创建: %s\n   ➜ 请编辑文件填入你的 API Key 后重启程序", credPath)
		}
	}

	refreshInterval := time.Duration(cfg.RefreshMinutes) * time.Minute

	h := &handler{
		upstream:   cfg.Upstream,
		credPath:   credPath,
		bind:       cfg.Bind,
		authToken:  cfg.AuthToken,
		cooldown:   newCooldownMgr(),
		state:      newState(),
		freeModels: newFreeModelsCache(),
		keyManager: newKeyManager(),
		heartbeat:  newHeartbeatMgr(cfg.AutoExitSec),
	}

	keys, err := loadKeys(credPath)
	if err == nil && len(keys) > 0 {
		h.freeModels.startBackgroundRefresh(keys[0].key, refreshInterval)
	} else {
		logf("⚠️ 无可用 Key，免费模型刷新跳过")
	}

	if cfg.AutoExitSec > 0 {
		h.heartbeat.startMonitor()
		logf("💓 心跳检测已开启，Dashboard 关闭后 %d 秒自动退出", cfg.AutoExitSec)
	}

	addr := fmt.Sprintf("%s:%s", cfg.Bind, cfg.Port)
	scheme := "http"
	if cfg.Bind != "127.0.0.1" && cfg.Bind != "localhost" && cfg.Bind != "[::1]" {
		scheme = "🔓 http(远程开放)"
	}
	logf("🚀 启动: %s://%s → https://%s", scheme, addr, cfg.Upstream)
	logf("📄 凭据: %s", credPath)
	logf("🩺 健康: http://%s/health", addr)
	logf("📊 面板: http://%s/", addr)
	if cfg.AuthToken != "" {
		logf("🔐 远程认证: X-Auth-Token header 或 ?token= query")
	}

	server := &http.Server{
		Addr:    addr,
		Handler: h,
	}
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("服务器错误: %v", err)
	}
}
