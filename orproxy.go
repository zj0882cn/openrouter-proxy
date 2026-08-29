// orproxy.go - OpenRouter 本地多账号代理（Go 版本）
// 交叉编译：GOOS=windows GOARCH=amd64 go build -o orproxy.exe orproxy.go
//           GOOS=linux   GOARCH=amd64 go build -o orproxy orproxy.go
package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
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

func fingerprintKey(k string) string {
	// 短指纹：SHA256 截断后 4 位，不泄漏明文但可识别同一 key
	h := sha256.Sum256([]byte(k))
	fp := fmt.Sprintf("%x", h)
	return fp[len(fp)-4:]
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

func (h *handler) parseCooldown(resp *http.Response, body []byte) time.Duration {
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
	return time.Duration(h.retry.Cooldown429Sec) * time.Second
}

// waitForKey 等待直到至少有一个 key 脱离冷却,或超过 maxSec
// 模仿 DSH proxy.py 的 wait_for_key():轮询 readyKeys(),全部冷却时 sleep WaitRetrySec
func (h *handler) waitForKey(keys []keyEntry, maxSec int) ([]keyEntry, string) {
	if maxSec <= 0 {
		maxSec = h.retry.WaitMaxSec
	}
	deadline := time.Now().Add(time.Duration(maxSec) * time.Second)
	for {
		ready, note := h.cooldown.readyKeys(keys)
		if note != "all-cooling" && len(ready) > 0 {
			return ready, note
		}
		if time.Now().After(deadline) {
			return ready, note // 返回 all-cooling 状态,由调用方决定是否重试
		}
		time.Sleep(time.Duration(h.retry.WaitRetrySec) * time.Second)
	}
}

// perKeyDelay 控制同一 key 的调用间隔(per-key rate limit)
// DSH 策略:每把 key 调用后强制 sleep 2s,避免触发 OpenRouter 5s/req 单 key 限制
func (h *handler) perKeyDelay(envName string) {
	if h.retry.PerKeyDelaySec <= 0 {
		return
	}
	h.lastCallMu.Lock()
	last := h.lastCall[envName]
	need := time.Duration(h.retry.PerKeyDelaySec) * time.Second
	if !last.IsZero() {
		elapsed := time.Since(last)
		if elapsed < need {
			h.lastCallMu.Unlock()
			time.Sleep(need - elapsed)
			h.lastCallMu.Lock()
		}
	}
	h.lastCall[envName] = time.Now()
	h.lastCallMu.Unlock()
}

// recordCall 记录一次调用到历史，保留最近 60s
func (h *handler) recordCall(envName string) {
	now := time.Now().UnixMilli()
	h.statsMu.Lock()
	defer h.statsMu.Unlock()
	h.callHistory[envName] = append(h.callHistory[envName], float64(now))
	cutoff := float64(now) - 60000
	h.callHistory[envName] = h.trimHistory(h.callHistory[envName], cutoff)
}

// record429 记录一次 429，打印频率统计
func (h *handler) record429(envName string) {
	now := time.Now().UnixMilli()
	h.statsMu.Lock()
	defer h.statsMu.Unlock()
	h.hist429[envName] = append(h.hist429[envName], float64(now))
	cutoff := float64(now) - 60000
	h.hist429[envName] = h.trimHistory(h.hist429[envName], cutoff)
	n429 := len(h.hist429[envName])
	ncalls := len(h.callHistory[envName])
	logf("📊 %s 429 频率: 近60s %d次429/%d次请求 [%s]", fingerprintKey(""), n429, ncalls, envName)
}

func (h *handler) trimHistory(arr []float64, cutoff float64) []float64 {
	i := 0
	for i < len(arr) && arr[i] < cutoff {
		i++
	}
	if i > 0 {
		return arr[i:]
	}
	return arr
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
	FreeRouter    bool   `json:"free_router"` // true = openrouter/free 官方路由（不消耗 key 额度）
}

func isFreeModel(m map[string]interface{}) bool {
	// openrouter/free 是 OpenRouter 官方免费 router：不消耗 key 自己的额度
	if id, _ := m["id"].(string); id == "openrouter/free" {
		return true
	}
	pricing, ok := m["pricing"].(map[string]interface{})
	if !ok {
		return false
	}
	promptStr := fmt.Sprintf("%v", pricing["prompt"])
	compStr := fmt.Sprintf("%v", pricing["completion"])
	return promptStr == "0" && compStr == "0"
}

func isFreeRouter(m map[string]interface{}) bool {
	id, _ := m["id"].(string)
	return id == "openrouter/free"
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
			FreeRouter:    isFreeRouter(m),
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
type RetryConfig struct {
	Cooldown429Sec   int
	Cooldown401Sec   int
	Cooldown403Sec   int
	PerKeyDelaySec   int
	WaitRetrySec     int
	WaitMaxSec       int
	FallbackRetrySec int
	FallbackMaxSec   int
	BetweenRoundsSec int
}

func defaultRetryConfig() RetryConfig {
	return RetryConfig{
		Cooldown429Sec:   10,
		Cooldown401Sec:   86400,
		Cooldown403Sec:   3600,
		PerKeyDelaySec:   6, // DSH 策略:同 key 调用间隔 6s(429 冻结 10s)
		WaitRetrySec:     3,
		WaitMaxSec:       30,
		FallbackRetrySec: 3,
		FallbackMaxSec:   60,
		BetweenRoundsSec: 10,
	}
}

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
	configPath  string
	configMu    sync.Mutex // 保护 bind/authToken/upstream 等运行时热更新的字段
	retry       RetryConfig
	lastCallMu  sync.Mutex
	lastCall    map[string]time.Time // per-key 上次调用时刻（限速用）
	// 频率统计: 60s 滑动窗口，用于观察 429 阈值
	statsMu     sync.Mutex
	callHistory map[string][]float64 // env -> [ts, ts, ...] 近 60s 每次调用
	hist429     map[string][]float64 // env -> [ts, ts, ...] 近 60s 每次 429
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
	// 兼容 OpenAI 风格的 Authorization: Bearer xxx
	if auth := r.Header.Get("Authorization"); auth != "" {
		if strings.HasPrefix(auth, "Bearer ") && auth[7:] == h.authToken {
			return true
		}
		// 也接受直接传 token (不强制 Bearer)
		if auth == h.authToken {
			return true
		}
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
	case "/config":
		h.serveConfig(w, r)
	case "/config/save":
		h.saveConfig(w, r)
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
		ks := keyStatus{Key: fmt.Sprintf("Key%s(%s)", k.slot, fingerprintKey(k.key))}
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

// ============ 配置管理 API ============
// GET  /config         - 读取配置文件原始内容（YAML 文本）
// POST /config/save    - 保存配置文件内容（YAML 文本）
//                         字段热更新: bind / auth_token / upstream / port / refresh_minutes / auto_exit_sec / creds_path
//                         （port/refresh_minutes/auto_exit_sec/creds_path 等需重启才生效的项仅日志提示）

func (h *handler) serveConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":{"message":"GET only"}}`, http.StatusMethodNotAllowed)
		return
	}
	data, err := os.ReadFile(h.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// 配置文件不存在就用默认内容
			data = []byte(defaultConfigYAML)
		} else {
			http.Error(w, fmt.Sprintf(`{"error":{"message":"read failed: %v"}}`, err), 500)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}{Path: h.configPath, Content: string(data)})
}

func (h *handler) saveConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":{"message":"POST only"}}`, http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"invalid body: %v"}}`, err), 400)
		return
	}
	if body.Content == "" {
		http.Error(w, `{"error":{"message":"content empty"}}`, 400)
		return
	}

	// 先备份原文件到 .bak（首次保存时不存在就跳过）
	orig, _ := os.ReadFile(h.configPath)
	if len(orig) > 0 {
		_ = os.WriteFile(h.configPath+".bak", orig, 0644)
	}

	// 写到 .tmp，再原子重命名，避免保存中途崩溃破坏文件
	tmp := h.configPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(body.Content), 0644); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"write tmp failed: %v"}}`, err), 500)
		return
	}
	if err := os.Rename(tmp, h.configPath); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"rename failed: %v"}}`, err), 500)
		return
	}

	// 热更新: 重新解析 yaml，更新可热加载的字段
	var newCfg AppConfig
	if err := yaml.Unmarshal([]byte(body.Content), &newCfg); err != nil {
		logf("⚠️ 配置保存成功但解析失败（需检查 yaml 语法）: %v", err)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(struct {
			OK      bool   `json:"ok"`
			Message string `json:"message"`
		}{OK: true, Message: "saved (yaml parse failed, no live update): " + err.Error()})
		return
	}

	h.configMu.Lock()
	hotChanges := []string{}
	if newCfg.Upstream != "" && newCfg.Upstream != h.upstream {
		old := h.upstream
		h.upstream = newCfg.Upstream
		hotChanges = append(hotChanges, fmt.Sprintf("upstream: %s → %s", old, newCfg.Upstream))
	}
	if newCfg.Bind != "" && newCfg.Bind != h.bind {
		old := h.bind
		h.bind = newCfg.Bind
		hotChanges = append(hotChanges, fmt.Sprintf("bind: %s → %s", old, newCfg.Bind))
	}
	if newCfg.AuthToken != h.authToken {
		h.authToken = newCfg.AuthToken
		hotChanges = append(hotChanges, "auth_token: updated")
	}
	h.configMu.Unlock()

	// 需要重启的项日志提示
	restartNeeded := []string{}
	if newCfg.Port != "" && newCfg.Port != defaultPort {
		restartNeeded = append(restartNeeded, "port")
	}
	if newCfg.RefreshMinutes > 0 {
		restartNeeded = append(restartNeeded, "refresh_minutes")
	}
	if newCfg.AutoExitSec != 0 {
		restartNeeded = append(restartNeeded, "auto_exit_sec")
	}
	if newCfg.CredsPath != "" {
		restartNeeded = append(restartNeeded, "creds_path")
	}

	msg := "已保存: " + h.configPath
	if len(hotChanges) > 0 {
		msg += "（已热更新: " + strings.Join(hotChanges, ", ") + "）"
	}
	if len(restartNeeded) > 0 {
		msg += "（下次重启生效: " + strings.Join(restartNeeded, ", ") + "）"
	}
	logf("📝 " + msg)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		OK      bool     `json:"ok"`
		Message string   `json:"message"`
		Hot     []string `json:"hot_updated"`
		Restart []string `json:"restart_required"`
	}{OK: true, Message: msg, Hot: hotChanges, Restart: restartNeeded})
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
		Name        string `json:"name"`
		Fingerprint string `json:"fingerprint"`
		Paused      bool   `json:"paused"`
		Cooling     bool   `json:"cooling"`
		CoolUntil   string `json:"cooling_until,omitempty"`
	}
	var list []keyInfo
	for _, k := range keys {
		ki := keyInfo{
			Name:        k.envName,
			Fingerprint: fingerprintKey(k.key),
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
	envName := req.EnvName
	if envName == "" {
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

	// 用 loadKeys 已解析的 envName 集合判断重复，避免误中注释行（如 "# OPENROUTER2_API_KEY..."）
	if used[envName] {
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
		if strings.Contains(k.key, req.Key) || fingerprintKey(k.key) == req.Key || k.envName == req.Key {
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
		if strings.Contains(k.key, req.Key) || fingerprintKey(k.key) == req.Key || k.envName == req.Key {
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

	// 客户端 X-ORProxy-Free: 1 → 强制走 openrouter/free（OpenRouter 官方免费 router，不消耗 key 自己的额度）
	if strings.EqualFold(r.Header.Get("X-ORProxy-Free"), "1") ||
		strings.EqualFold(r.Header.Get("X-ORProxy-Free-Router"), "1") {
		body = rewriteModel(body, "openrouter/free")
		modelName = "openrouter/free"
		logf("🌐 走免费 router（不消耗 key 额度）: %s", modelName)
	}

	keys, err := h.loadKeys()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%v"}}`, err.Error()), 500)
		return
	}

	start := h.state.rotateStart(keys)
	rotated := append(keys[start:], keys[:start]...)
	ready, note := h.cooldown.readyKeys(rotated)

	// 记录原始请求体（免费路由全部失败时 fallback 用）
	originalBody := body
	origModelName := extractModel(body)

	// 单次请求内免费路由失败标记，触发后不再重复 fallback
	fallbackDone := false

	tryProxy := func(body []byte, modelName string, isFreeRouter bool) bool {
		// DSH 策略: 免费路由第 0 步先试 upstream_free（无认证，不耗 Key 配额）
		if isFreeRouter {
			reqStart := time.Now()
			resp, err := h.upstreamFree(http.MethodPost, "/v1/chat/completions", body)
			elapsed := time.Since(reqStart)
			if err == nil {
				respBody, _ := io.ReadAll(resp.Body)
				statusCode := resp.StatusCode
				if statusCode == 200 {
					logf("✅ [%s] 🌐 无认证 free 成功 [%.1fs]",
						modelName, elapsed.Seconds())
					h.copyResponse(w, resp, respBody)
					return true
				}
				// 400/500/502/503 直返不 fallback（DSH 策略）
				if statusCode == 400 || statusCode >= 500 {
					logf("❌ [%s] 🌐 无认证 HTTP %d（直返不 fallback）", modelName, statusCode)
					h.copyResponse(w, resp, respBody)
					return true
				}
				logf("⚠️ [%s] 🌐 无认证 free 失败: HTTP %d（将走 key 轮询）", modelName, statusCode)
			} else {
				logf("⚠️ [%s] 🌐 无认证 free 异常: %v（将走 key 轮询）", modelName, err)
			}
		}

		// all-cooling 时等待 key 恢复(DSH wait_for_key 策略)
		if note == "all-cooling" {
			logf("⚠️ [%s] 所有 key 均冷却中，等待恢复 (wait_max=%ds)...", modelName, h.retry.WaitMaxSec)
			var waitNote string
			ready, waitNote = h.waitForKey(rotated, h.retry.WaitMaxSec)
			if len(ready) == 0 {
				logf("⚠️ [%s] 等待超时(all-cooling)，继续尝试最后一把 key", modelName)
			} else {
				logf("✅ [%s] %s", modelName, waitNote)
			}
		}

		for i, k := range ready {
			if h.keyManager.isPaused(k.key) {
				logf("⏸️ [%s] %s 已暂停，跳过", modelName, fingerprintKey(k.key))
				continue
			}
			if i > 0 || note == "all-cooling" {
				logf("⚠️ [%s] %s", modelName, note)
			}

			// DSH: per-key rate limit — 调用前强制 sleep 间隔
			h.perKeyDelay(k.envName)

			reqStart := time.Now()
			resp, err := h.doUpstream(k.key, http.MethodPost, "/v1/chat/completions", body)
			elapsed := time.Since(reqStart)
			h.recordCall(k.envName)

			if err != nil {
				logf("❌ [%s] %s 网络错误: %v", modelName, fingerprintKey(k.key), err)
				continue
			}

			respBody, _ := io.ReadAll(resp.Body)
			statusCode := resp.StatusCode

			if statusCode == 200 {
				h.state.setLastGood(k.envName)
				tokens := extractTokens(respBody)
				if isFreeRouter {
					logf("✅ [%s] 🌐 免费路由成功 %s %s [%.1fs, %s]",
						modelName, fingerprintKey(k.key), tokens, elapsed.Seconds(), k.envName)
				} else {
					logf("✅ [%s] %s %s [%.1fs, %s]",
						modelName, fingerprintKey(k.key), tokens, elapsed.Seconds(), k.envName)
				}
				h.copyResponse(w, resp, respBody)
				return true
			}

			if statusCode == 429 {
				h.record429(k.envName)
				coolDur := h.parseCooldown(resp, respBody)
				until := time.Now().Add(coolDur)
				h.cooldown.setCooling(k.envName, until)
				logf("🔁 [%s] %s 429 → 冷却%.0f分钟，换下一条 [%s]",
					modelName, fingerprintKey(k.key), coolDur.Minutes(), k.envName)
				continue
			}

			if statusCode == 401 {
				// 401: key 无效/被删除，24h 冷却
				coolDur := time.Duration(h.retry.Cooldown401Sec) * time.Second
				h.cooldown.setCooling(k.envName, time.Now().Add(coolDur))
				logf("⛔ [%s] %s 401 → 冷却%.0f小时 [%s]",
					modelName, fingerprintKey(k.key), coolDur.Hours(), k.envName)
				continue
			}

			if statusCode == 402 {
				// 402: 模型需付费额度，直接跳过（不冷却，DSH 策略）
				logf("⛔ [%s] %s 402 模型需付费额度，跳过 [%s]",
					modelName, fingerprintKey(k.key), k.envName)
				continue
			}

			if statusCode == 403 {
				// 403: 付费额度不足，1h 冷却
				coolDur := time.Duration(h.retry.Cooldown403Sec) * time.Second
				h.cooldown.setCooling(k.envName, time.Now().Add(coolDur))
				logf("⛔ [%s] %s 403 付费额度不足，冷却%.0f分钟 [%s]",
					modelName, fingerprintKey(k.key), coolDur.Minutes(), k.envName)
				continue
			}

			// DSH 策略: 400/500/502/503 直返，不 fallback
			if statusCode == 400 || statusCode >= 500 {
				h.copyResponse(w, resp, respBody)
				return true
			}

			// 其他错误：直返（不 fallback）
			h.copyResponse(w, resp, respBody)
			return true
		}
		return false
	}

	// 第一轮：走免费路由（body 已在前面被 rewriteModel 改写）
	if modelName == "openrouter/free" {
		ok := tryProxy(body, modelName, true)
		if ok {
			return
		}
		// 免费路由全部失败 → fallback 原始模型（带重试循环）
		if !fallbackDone {
			fallbackDone = true
			logf("🔄 免费路由全部失败，fallback 到原始模型: %s (max=%ds, retry=%ds)",
				origModelName, h.retry.FallbackMaxSec, h.retry.FallbackRetrySec)
			// 轮间 sleep：让前面免费路由的 429 冷却有时间恢复
			if h.retry.BetweenRoundsSec > 0 {
				logf("⏳ 轮间 sleep %ds...", h.retry.BetweenRoundsSec)
				time.Sleep(time.Duration(h.retry.BetweenRoundsSec) * time.Second)
			}
			deadline := time.Now().Add(time.Duration(h.retry.FallbackMaxSec) * time.Second)
			for round := 1; ; round++ {
				// 重新 rotate + 取 ready keys（冷却状态已变化）
				start2 := h.state.rotateStart(keys)
				rotated := append(keys[start2:], keys[:start2]...)
				ready, note = h.cooldown.readyKeys(rotated)
				if note == "all-cooling" {
					logf("⏳ [fallback round %d] %s，等待 key 恢复...", round, note)
				}
				if tryProxy(originalBody, origModelName, false) {
					return
				}
				if time.Now().After(deadline) {
					logf("⏱️ [fallback] 已达最大重试 %ds，放弃", h.retry.FallbackMaxSec)
					break
				}
				logf("⏳ [fallback round %d] 全部失败，%ds 后重试...", round, h.retry.FallbackRetrySec)
				time.Sleep(time.Duration(h.retry.FallbackRetrySec) * time.Second)
			}
		}
	} else {
		// 非免费路由请求，直接走
		tryProxy(body, modelName, false)
		return
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

// rewriteModel 把请求体中的 model 字段重写为新值；解析失败时返回原 body
func rewriteModel(body []byte, newModel string) []byte {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}
	parsed["model"] = newModel
	out, err := json.Marshal(parsed)
	if err != nil {
		return body
	}
	return out
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

// upstreamFree 无认证请求，用于 openrouter/free 兜底（绕过付费 Key 的 429 传染）
// DSH 策略: free 路由第一轮第 0 步先试这个，省一次 Key 调用
func (h *handler) upstreamFree(method, path string, body []byte) (*http.Response, error) {
	upstreamPath := path
	if strings.HasPrefix(path, "/v1") {
		upstreamPath = "/api" + path
	}
	upstreamURL := fmt.Sprintf("https://%s%s", h.upstream, upstreamPath)
	req, err := http.NewRequest(method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
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
	// reasoning 模型返回 content="" 但 reasoning 有内容时,填充 content 避免客户端报错
	if resp.StatusCode == 200 {
		if filled := tryFillEmptyContent(body); len(filled) > 0 {
			body = filled
		}
	}
	w.WriteHeader(resp.StatusCode)
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Write(body)
}

// 当 message.content 为空但 message.reasoning 有内容时,填充 content
func tryFillEmptyContent(body []byte) []byte {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	choices, ok := parsed["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return nil
	}
	changed := false
	for _, c := range choices {
		choice, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		msg, ok := choice["message"].(map[string]interface{})
		if !ok {
			continue
		}
		content, hasContent := msg["content"].(string)
		reasoning, hasReasoning := msg["reasoning"].(string)
		if hasContent && content == "" && hasReasoning && reasoning != "" {
			msg["content"] = reasoning
			changed = true
		}
	}
	if changed {
		out, _ := json.Marshal(parsed)
		return out
	}
	return nil
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
	// 轮询 / 冷却 / 重试延时（秒）
	Cooldown429Sec   int `yaml:"cooldown_429_sec"`    // 429 冷却秒数（默认 10）
	Cooldown401Sec   int `yaml:"cooldown_401_sec"`    // 401 冷却秒数（默认 86400）
	Cooldown403Sec   int `yaml:"cooldown_403_sec"`    // 403 冷却秒数（默认 3600）
	PerKeyDelaySec   int `yaml:"per_key_delay_sec"`   // per-key 调用间隔秒数（默认 2）
	WaitRetrySec     int `yaml:"wait_retry_sec"`      // 全 Key 冷却时重试间隔秒数（默认 3）
	WaitMaxSec       int `yaml:"wait_max_sec"`        // GET 路径最大等待秒数（默认 30）
	FallbackRetrySec int `yaml:"fallback_retry_sec"`  // fallback 轮询间重试间隔秒数（默认 3）
	FallbackMaxSec   int `yaml:"fallback_max_sec"`    // fallback 最大超时秒数（默认 60）
	BetweenRoundsSec int `yaml:"between_rounds_sec"`  // 两轮之间 sleep 秒数（默认 10）
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

# ===== 重试/限速参数（DSH 策略，全部可调） =====
# 429 冷却（秒），OpenRouter 触发的限流通常 5~15s 恢复
cooldown_429_sec: 10
# 401 冷却（秒），key 无效/被删除 → 24h
cooldown_401_sec: 86400
# 403/402 冷却（秒），付费额度不足 → 1h
cooldown_403_sec: 3600
# 同一 key 最小调用间隔（秒），DSH 策略：成功也冻结 6s，429 冻结 10s
per_key_delay_sec: 6
# 全部冷却时重试间隔（秒）
wait_retry_sec: 3
# 全部冷却时最大等待（秒）
wait_max_sec: 30
# fallback 轮询间隔（秒）
fallback_retry_sec: 3
# fallback 最大总超时（秒），超过后 502
fallback_max_sec: 60
# 两轮重试间 sleep（秒），让 429 冷却恢复
between_rounds_sec: 10
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
				// 9 个延时字段:yaml 优先级 > default (无需 env 覆盖,这些参数测试期不需 env)
				if fileCfg.Cooldown429Sec > 0 {
					cfg.Cooldown429Sec = fileCfg.Cooldown429Sec
				}
				if fileCfg.Cooldown401Sec > 0 {
					cfg.Cooldown401Sec = fileCfg.Cooldown401Sec
				}
				if fileCfg.Cooldown403Sec > 0 {
					cfg.Cooldown403Sec = fileCfg.Cooldown403Sec
				}
				if fileCfg.PerKeyDelaySec > 0 {
					cfg.PerKeyDelaySec = fileCfg.PerKeyDelaySec
				}
				if fileCfg.WaitRetrySec > 0 {
					cfg.WaitRetrySec = fileCfg.WaitRetrySec
				}
				if fileCfg.WaitMaxSec > 0 {
					cfg.WaitMaxSec = fileCfg.WaitMaxSec
				}
				if fileCfg.FallbackRetrySec > 0 {
					cfg.FallbackRetrySec = fileCfg.FallbackRetrySec
				}
				if fileCfg.FallbackMaxSec > 0 {
					cfg.FallbackMaxSec = fileCfg.FallbackMaxSec
				}
				if fileCfg.BetweenRoundsSec > 0 {
					cfg.BetweenRoundsSec = fileCfg.BetweenRoundsSec
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
		configPath: resolvePath(env("CONFIG_PATH", defaultConfigFile)),
		retry: func() RetryConfig {
			rc := defaultRetryConfig()
			if cfg.Cooldown429Sec > 0   { rc.Cooldown429Sec = cfg.Cooldown429Sec }
			if cfg.Cooldown401Sec > 0   { rc.Cooldown401Sec = cfg.Cooldown401Sec }
			if cfg.Cooldown403Sec > 0   { rc.Cooldown403Sec = cfg.Cooldown403Sec }
			if cfg.PerKeyDelaySec > 0   { rc.PerKeyDelaySec = cfg.PerKeyDelaySec }
			if cfg.WaitRetrySec > 0     { rc.WaitRetrySec = cfg.WaitRetrySec }
			if cfg.WaitMaxSec > 0       { rc.WaitMaxSec = cfg.WaitMaxSec }
			if cfg.FallbackRetrySec > 0 { rc.FallbackRetrySec = cfg.FallbackRetrySec }
			if cfg.FallbackMaxSec > 0   { rc.FallbackMaxSec = cfg.FallbackMaxSec }
			if cfg.BetweenRoundsSec > 0 { rc.BetweenRoundsSec = cfg.BetweenRoundsSec }
			return rc
		}(),
		lastCall:    make(map[string]time.Time),
		callHistory: make(map[string][]float64),
		hist429:     make(map[string][]float64),
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

	// 显式 TCP4 监听（避免 Go 默认把 0.0.0.0 转 IPv6 wildcard，导致 IPv4 包被丢弃）
	listener, err := net.Listen("tcp4", addr)
	if err != nil {
		log.Fatalf("监听失败 %s: %v", addr, err)
	}
	server := &http.Server{
		Handler: h,
	}
	if err := server.Serve(listener); err != nil {
		log.Fatalf("服务器错误: %v", err)
	}
}
