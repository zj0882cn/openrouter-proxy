// orproxy v1.3.0 - OpenRouter 本地多账号代理（Go 版本）
// 改进：6阶段日志 + 毫秒时间戳 + 客户端ID跟踪 + Zhipu 60s + 性能优化
// 交叉编译：GOOS=windows GOARCH=amd64 go build -o orproxy.exe orproxy_v130.go
//           GOOS=linux   GOARCH=amd64 go build -o orproxy orproxy_v130.go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const CONFIG_FILE = "orproxy-config.yaml"
const CREDS_FILE = "orproxy-creds.yaml"
const OR_COOLDOWN_SEC = 10
const MAX_ROUNDS = 3
const ZHIPU_TIMEOUT_SEC = 60 // v1.3.0: Zhipu timeout 20s → 60s
const ZHIPU_COOLDOWN_SEC = 6 // v1.3.0: Zhipu 1RPS → 6s
const LARGE_BODY_THRESHOLD = 200 * 1024 // v1.3.0: 大 body 阈值 200KB
const ALMOST_READY_THRESHOLD_SEC = 2    // v1.3.0: 冷却 < 2s 也参与

type Config struct {
	Port      int    `yaml:"port"`
	Bind      string `yaml:"bind"`
	LogLevel  string `yaml:"log_level"`
	AuthToken string `yaml:"auth_token"`
}

var (
	config   *Config
	logLevel = 1 // 0=DEBUG, 1=INFO, 2=WARN, 3=ERROR
)

var GRADIENT_INTERVALS = []int{2, 4, 8, 16, 32, 64, 128, 256}

type ORKey struct {
	Env string
	Key string
}

var (
	orKeys       []ORKey
	lastGoodIdx  = -1
	lastGoodMu   sync.Mutex
	cooldownMap  = sync.Map{}
	lastCallTs   = sync.Map{}
	zhipuKey     string
	zhipuLastTs  time.Time
	zhipuMu      sync.Mutex
	dailyCount   int
	dailyDate    string
	dailyMu      sync.Mutex
	dailySuccess int // v1.3.0: 成功计数
	dailyFail    int // v1.3.0: 失败计数
	dailyRetry   int // v1.3.0: 重试计数
	statsMu      sync.Mutex
	poolAttempts = map[string]int{}
	poolSuccess  = map[string]int{}
	poolFail     = map[string]int{}
	statusAll    = map[string]int{}

	orKeyLocks = sync.Map{} // env -> chan struct{}
)

// v1.3.0: 客户端 ID 首次访问跟踪
var (
	firstVisitMap  = make(map[string]bool) // 短ID -> 是否首次
	firstVisitMu   sync.RWMutex
)

func getOrKeyLock(env string) chan struct{} {
	v, _ := orKeyLocks.LoadOrStore(env, make(chan struct{}, 1))
	return v.(chan struct{})
}

// v1.3.0: 6阶段日志上下文
type RequestContext struct {
	CliID       string // 完整客户端ID
	CliIDShort  string // 短客户端ID (8字符)
	ReqID       string // 完整请求ID
	ReqIDShort  string // 短请求ID (4字符)
	IsFirst     bool   // 是否首次访问
	StartTime   time.Time
	Model       string
	BodySize    int
	CliType     string
	ClientIP    string
	Messages    int
	LargeBody   bool // v1.3.0: 大 body 标记
}

func newRequestContext(cliID, reqID, cliType, clientIP string, bodySize int) *RequestContext {
	return &RequestContext{
		CliID:      cliID,
		CliIDShort: shortID(cliID, 8),
		ReqID:      reqID,
		ReqIDShort: shortID(reqID, 4),
		StartTime:  time.Now(),
		BodySize:   bodySize,
		CliType:    cliType,
		ClientIP:   clientIP,
		LargeBody:  bodySize > LARGE_BODY_THRESHOLD,
	}
}

// v1.3.0: 检查是否首次访问
func checkFirstVisit(cliID string) bool {
	shortID := shortID(cliID, 8)
	firstVisitMu.RLock()
	isFirst := !firstVisitMap[shortID]
	firstVisitMu.RUnlock()
	if isFirst {
		firstVisitMu.Lock()
		firstVisitMap[shortID] = true
		firstVisitMu.Unlock()
	}
	return isFirst
}

// v1.3.0: 生成短 ID
func shortID(id string, length int) string {
	if len(id) <= length {
		return id
	}
	return id[:length]
}

// v1.3.0: 计算耗时 (毫秒)
func elapsedMs(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}

// v1.3.0: 6阶段日志格式
// 📥 起始: 收到请求
// 🎯 选路: 选模型
// ➡️ 尝试: 尝试 provider
// ✅❌⏭️🔁 结果: 成功/失败/跳过/429
// 📤 写回: 写回客户端
// ✔️ 结束: 完成

func logReq(ctx *RequestContext, emoji string, format string, args ...interface{}) {
	ts := time.Now().Format("15:04:05.000") // v1.3.0: 毫秒级时间戳
	prefix := fmt.Sprintf("[%s] [INFO] [c=%s] [r=%s] %s", ts, ctx.CliIDShort, ctx.ReqIDShort, emoji)
	
	// v1.3.0: 首次请求时在 📥 段显示完整信息
	if emoji == "📥" && ctx.IsFirst {
		bodySize := formatBytes(ctx.BodySize)
		fullLine := fmt.Sprintf("%s POST /v1/chat | cliid=%s reqid=%s cli=%s ip=%s model=%s body=%s/msgs=%d",
			prefix, ctx.CliID, ctx.ReqID, ctx.CliType, ctx.ClientIP, ctx.Model, bodySize, ctx.Messages)
		if ctx.LargeBody {
			fullLine += " LARGE_BODY"
		}
		fmt.Fprintln(os.Stderr, fullLine)
		writeLogToFile(fullLine)
		return
	}
	
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("%s %s", prefix, msg)
	fmt.Fprintln(os.Stderr, line)
	writeLogToFile(line)
}

func logReqWarn(ctx *RequestContext, emoji string, format string, args ...interface{}) {
	ts := time.Now().Format("15:04:05.000")
	prefix := fmt.Sprintf("[%s] [WARN] [c=%s] [r=%s] %s", ts, ctx.CliIDShort, ctx.ReqIDShort, emoji)
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("%s %s", prefix, msg)
	fmt.Fprintln(os.Stderr, line)
	writeLogToFile(line)
}

func logReqError(ctx *RequestContext, emoji string, format string, args ...interface{}) {
	ts := time.Now().Format("15:04:05.000")
	prefix := fmt.Sprintf("[%s] [ERROR] [c=%s] [r=%s] %s", ts, ctx.CliIDShort, ctx.ReqIDShort, emoji)
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("%s %s", prefix, msg)
	fmt.Fprintln(os.Stderr, line)
	writeLogToFile(line)
}

func logReqDebug(ctx *RequestContext, format string, args ...interface{}) {
	if logLevel > 1 { // DEBUG only if log_level allows
		return
	}
	ts := time.Now().Format("15:04:05.000")
	prefix := fmt.Sprintf("[%s] [DEBUG] [c=%s] [r=%s]", ts, ctx.CliIDShort, ctx.ReqIDShort)
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("%s %s", prefix, msg)
	fmt.Fprintln(os.Stderr, line)
	writeLogToFile(line)
}

func formatBytes(n int) string {
	if n >= 1024*1024 {
		return fmt.Sprintf("%.1fMB", float64(n)/1024/1024)
	} else if n >= 1024 {
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	}
	return fmt.Sprintf("%dB", n)
}

func writeLogToFile(msg string) {
	if err := os.MkdirAll("./log", 0755); err == nil {
		now := time.Now()
		hourKey := now.Format("20060102-15")
		logFile := fmt.Sprintf("./log/%s-info.log", hourKey)
		if f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
			f.WriteString(msg + "\n")
			f.Close()
		}
	}
}

// 保留旧版日志函数用于兼容
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(+" + strconv.Itoa(len(b)-n) + "B)"
}

func levelToNum(level string) int {
	switch level {
	case "DEBUG": return 0
	case "INFO":  return 1
	case "WARN":  return 2
	case "ERROR": return 3
	default:      return 1
	}
}

func shouldLog(level string) bool {
	return levelToNum(level) >= logLevel
}

func log(format string, args ...interface{})  { writeLog("INFO", format, args...) }
func logDebug(format string, args ...interface{}) { writeLog("DEBUG", format, args...) }
func logWarn(format string, args ...interface{}) { writeLog("WARN", format, args...) }
func logError(format string, args ...interface{}) { writeLog("ERROR", format, args...) }

func writeLog(level, format string, args ...interface{}) {
	if !shouldLog(level) {
		return
	}
	now := time.Now()
	ts := now.Format("15:04:05.000") // v1.3.0: 毫秒级时间戳
	msg := fmt.Sprintf("[%s] [%s] ", ts, level) + fmt.Sprintf(format, args...)
	fmt.Fprintln(os.Stderr, msg)
	writeLogToFile(msg)
}

func loadConfig() {
	defaultCfg := Config{Port: 18887, Bind: "127.0.0.1", LogLevel: "INFO", AuthToken: ""}
	data, err := os.ReadFile(CONFIG_FILE)
	if err != nil {
		log("配置加载失败: %s 不存在，使用默认配置: port=%d, bind=%s, log_level=%s", CONFIG_FILE, defaultCfg.Port, defaultCfg.Bind, defaultCfg.LogLevel)
		writeDefaultConfig(defaultCfg)
		config = &defaultCfg
		logLevel = levelToNum(defaultCfg.LogLevel)
		return
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		logWarn("配置解析失败: %v", err)
		cfg = defaultCfg
	}
	if cfg.Port == 0 {
		cfg.Port = 18887
	}
	if cfg.Bind == "" {
		cfg.Bind = "127.0.0.1"
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "INFO"
	}
	config = &cfg
	logLevel = levelToNum(cfg.LogLevel)
	log("配置加载成功: port=%d, bind=%s, log_level=%s", cfg.Port, cfg.Bind, cfg.LogLevel)
}

func writeDefaultConfig(cfg Config) {
	defaultContent := fmt.Sprintf(`# ORProxy v1.3.0 配置文件
# 启动时若不存在则自动创建此文件

port: %d
bind: "%s"
log_level: "%s"
auth_token: "%s"
`, cfg.Port, cfg.Bind, cfg.LogLevel, cfg.AuthToken)
	os.WriteFile(CONFIG_FILE, []byte(defaultContent), 0644)
}

func loadCreds() {
	data, err := os.ReadFile(CREDS_FILE)
	if err != nil {
		log("读取凭证失败: %v", err)
		return
	}
	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	orKeys = nil
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		env := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if strings.HasPrefix(env, "OPENROUTER") && strings.HasSuffix(env, "_API_KEY") {
			orKeys = append(orKeys, ORKey{Env: env, Key: val})
		}
		if env == "ZHIPU_API_KEY" {
			zhipuKey = val
		}
	}
	sort.Slice(orKeys, func(i, j int) bool {
		ni, nj := 0, 0
		fmt.Sscanf(orKeys[i].Env, "OPENROUTER%d_API_KEY", &ni)
		fmt.Sscanf(orKeys[j].Env, "OPENROUTER%d_API_KEY", &nj)
		return ni < nj
	})
	log("已加载 %d 个 OR Key, Zhipu: %v", len(orKeys), zhipuKey != "")
}

func maskKey(key string) string {
	if len(key) < 6 {
		return key
	}
	return "..." + key[len(key)-6:]
}

func recordSuccess(pool string) {
	statsMu.Lock()
	poolAttempts[pool]++
	poolSuccess[pool]++
	statsMu.Unlock()
}

func recordError(pool string, status string) {
	statsMu.Lock()
	poolAttempts[pool]++
	poolFail[pool]++
	statusAll[status]++
	statsMu.Unlock()
}

func httpRequest(hostname, path, method string, body []byte, headers map[string]string, timeout time.Duration) (*http.Response, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(method, "https://"+hostname+path, bytes.NewReader(body))
	if err != nil {
		logError("HTTP创建请求失败 %s %s: %v", method, hostname+path, err)
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	logDebug("→ %s %s%s body=%dB timeout=%s", method, hostname, path, len(body), timeout)
	resp, err := client.Do(req)
	if err != nil {
		logError("✗ %s %s 异常: %v", method, hostname+path, err)
		return nil, err
	}
	logDebug("← %s %s status=%d", method, hostname+path, resp.StatusCode)
	return resp, nil
}

func httpRequestLocal(hostname string, port int, path, method string, body []byte, headers map[string]string, timeout time.Duration) (*http.Response, error) {
	client := &http.Client{Timeout: timeout}
	url := fmt.Sprintf("http://%s:%d%s", hostname, port, path)
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		logError("HTTP创建本地请求失败 %s %s: %v", method, url, err)
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	logDebug("→ %s %s body=%dB timeout=%s", method, url, len(body), timeout)
	resp, err := client.Do(req)
	if err != nil {
		logDebug("✗ %s %s 异常: %v", method, url, err)
		return nil, err
	}
	logDebug("← %s %s status=%d", method, url, resp.StatusCode)
	return resp, nil
}

// ---- 1) OpenRouter free ----
func tryOpenrouterFree(data []byte) ([]byte, int, bool) {
	var reqData map[string]interface{}
	json.Unmarshal(data, &reqData)
	reqData["model"] = "openrouter/free"
	body, _ := json.Marshal(reqData)
	logDebug("[OR-Free] 尝试 model=openrouter/free")
	resp, err := httpRequest("openrouter.ai", "/api/v1/chat/completions", "POST", body, nil, 180*time.Second)
	if err != nil {
		logDebug("[OR-Free] 请求异常: %v", err)
		recordError("openrouter_free", "exception")
		return nil, 0, false
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 200 {
		logDebug("[OR-Free] 成功 body=%dB", len(respBody))
		recordSuccess("openrouter_free")
		return respBody, resp.StatusCode, true
	}
	logDebug("[OR-Free] 失败 status=%d body=%s", resp.StatusCode, truncate(respBody, 200))
	recordError("openrouter_free", strconv.Itoa(resp.StatusCode))
	return respBody, resp.StatusCode, false
}

// ---- 2) OpenRouter Keys (轮转 + 互斥锁 + 冷却) ----
// v1.3.0: pickCandidates 优化 - 冷却剩余 < 2s 的 key 也参与尝试
func pickCandidates() []int {
	now := time.Now()
	start := 0
	lastGoodMu.Lock()
	if lastGoodIdx >= 0 && lastGoodIdx < len(orKeys)-1 {
		start = lastGoodIdx + 1
	}
	lastGoodMu.Unlock()

	var candidates []int
	var almostReady []int // v1.3.0: 快要好的 key

	for i := 0; i < len(orKeys); i++ {
		idx := (start + i) % len(orKeys)
		env := orKeys[idx].Env
		if cd, ok := cooldownMap.Load(env); ok {
			remaining := time.Until(time.Unix(cd.(int64), 0))
			if remaining > 0 {
				// v1.3.0: 冷却剩余 < 2s 的 key 加入 almostReady
				if remaining < ALMOST_READY_THRESHOLD_SEC*time.Second {
					logDebug("[OR-Key] 几乎就绪 %s (剩余 %v)", maskKey(orKeys[idx].Key), remaining)
					almostReady = append(almostReady, idx)
				} else {
					logDebug("[OR-Key] 跳过 %s (冷却至 %s)", maskKey(orKeys[idx].Key), time.Unix(cd.(int64), 0).Format("15:04:05"))
					continue
				}
			}
		}
		candidates = append(candidates, idx)
	}

	// v1.3.0: 如果没有 ready 的，尝试 almostReady
	if len(candidates) == 0 && len(almostReady) > 0 {
		log("⚡ 没有 ready 的 key，尝试 %d 个 almostReady", len(almostReady))
		candidates = almostReady
	}

	logDebug("[OR-Key] 候选 %d 个 (总%d, 起点 %d): %v", len(candidates), len(orKeys), start, candidates)
	return candidates
}

func tryOpenrouterKey(data []byte, idx int) ([]byte, int, bool) {
	// 互斥锁：同一 Key 同时只能一个请求
	lock := getOrKeyLock(orKeys[idx].Env)
	lock <- struct{}{}        // 获取锁（阻塞则等待）
	defer func() { <-lock }() // 释放锁

	env := orKeys[idx].Env
	key := orKeys[idx].Key
	now := time.Now()

	// 10s 冷却检查
	if lastTs, ok := lastCallTs.Load(env); ok {
		remaining := OR_COOLDOWN_SEC - int(now.Unix()-lastTs.(int64))
		if remaining > 0 {
			log("⏳ %s 还在 %ds 冷却，跳过", maskKey(key), remaining)
			return nil, 0, false
		}
	}
	lastCallTs.Store(env, now.Unix())

	logDebug("[OR-Key] 尝试 idx=%d env=%s", idx, env)
	resp, err := httpRequest("openrouter.ai", "/api/v1/chat/completions", "POST", data, map[string]string{
		"Authorization": "Bearer " + key,
	}, 180*time.Second)
	if err != nil {
		logDebug("[OR-Key] 请求异常 %s: %v", maskKey(key), err)
		recordError("openrouter_key:"+env, "exception")
		lastCallTs.Store(env, int64(0))
		return nil, 0, false
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == 200 {
		logDebug("[OR-Key] 成功 %s body=%dB", maskKey(key), len(respBody))
		recordSuccess("openrouter_key:" + env)
		lastGoodMu.Lock()
		lastGoodIdx = idx
		lastGoodMu.Unlock()
		return respBody, 200, true
	}

	logDebug("[OR-Key] 失败 %s status=%d body=%s", maskKey(key), resp.StatusCode, truncate(respBody, 200))
	recordError("openrouter_key:"+env, strconv.Itoa(resp.StatusCode))
	lastCallTs.Store(env, int64(0))

	switch resp.StatusCode {
	case 429:
		cooldownMap.Store(env, time.Now().Unix()+10)
		log("🚫 %s 429 → 冷却 10s", maskKey(key))
	case 401:
		cooldownMap.Store(env, time.Now().Unix()+86400)
		log("🔒 %s 401 → 冷却 24h", maskKey(key))
	case 403:
		cooldownMap.Store(env, time.Now().Unix()+3600)
		log("🔒 %s 403 → 冷却 1h", maskKey(key))
	case 402:
		log("🔒 %s 402 需要付费，跳过", maskKey(key))
	}
	return respBody, resp.StatusCode, false
}

// ---- 3) Zhipu ----
// v1.3.0: timeout 20s → 60s, 1RPS → 6s
func tryZhipu(data []byte, glmModel string) ([]byte, int, bool) {
	if zhipuKey == "" {
		logDebug("[Zhipu] 跳过，zhipuKey 为空")
		return nil, 0, false
	}
	zhipuMu.Lock()
	defer zhipuMu.Unlock()

	// v1.3.0: 6s 冷却（原 1s）
	if time.Since(zhipuLastTs) < ZHIPU_COOLDOWN_SEC*time.Second {
		logDebug("[Zhipu] %ds 限流跳过 model=%s since=%s", ZHIPU_COOLDOWN_SEC, glmModel, time.Since(zhipuLastTs))
		log("🐌 Zhipu %ds 限流，跳过", ZHIPU_COOLDOWN_SEC)
		return nil, 0, false
	}
	zhipuLastTs = time.Now()

	var reqData map[string]interface{}
	json.Unmarshal(data, &reqData)
	reqData["model"] = glmModel
	body, _ := json.Marshal(reqData)
	logDebug("[Zhipu] 尝试 model=%s", glmModel)

	// v1.3.0: timeout 60s（原 20s）
	timeout := time.Duration(ZHIPU_TIMEOUT_SEC) * time.Second
	
	resp, err := httpRequest("open.bigmodel.cn", "/api/paas/v4/chat/completions", "POST", body, map[string]string{
		"Authorization": "Bearer " + zhipuKey,
	}, timeout)
	if err != nil {
		logDebug("[Zhipu] 请求异常 %s: %v", glmModel, err)
		recordError("zhipu:"+glmModel, "exception")
		return nil, 0, false
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == 200 {
		logDebug("[Zhipu] 成功 %s body=%dB", glmModel, len(respBody))
		recordSuccess("zhipu:" + glmModel)
		return respBody, 200, true
	}
	logDebug("[Zhipu] 失败 %s status=%d body=%s", glmModel, resp.StatusCode, truncate(respBody, 200))
	recordError("zhipu:"+glmModel, strconv.Itoa(resp.StatusCode))
	return respBody, resp.StatusCode, false
}

// ---- Request ID 提取 ----
func extractRequestID(r *http.Request) string {
	// 记录所有可能的 Request ID header
	logDebug("[Chat] 检查 Request ID header: X-Request-Id=%q, X-Client-Request-Id=%q, Copilot-Request-Id=%q, X-Correlation-Id=%q, Request-Id=%q",
		r.Header.Get("X-Request-Id"),
		r.Header.Get("X-Client-Request-Id"),
		r.Header.Get("Copilot-Request-Id"),
		r.Header.Get("X-Correlation-Id"),
		r.Header.Get("Request-Id"))
	
	for _, h := range []string{"X-Request-Id", "X-Client-Request-Id", "Copilot-Request-Id", "X-Correlation-Id", "Request-Id"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	return ""
}

// ---- v1.3.0: 客户端 ID 管理 ----
func getOrCreateClientID(r *http.Request) string {
	// 优先级: Cookie > X-Client-Id header > 生成新 ID
	if cookie, err := r.Cookie("orproxy_cli"); err == nil && cookie.Value != "" {
		return cookie.Value
	}
	if cliID := r.Header.Get("X-Client-Id"); cliID != "" {
		return cliID
	}
	// 生成新 ID（由调用方通过 SetCookie 下发）
	return fmt.Sprintf("orproxy-%d", time.Now().UnixNano())
}

// ---- 主处理逻辑 ----
func handleChatCompletions(w http.ResponseWriter, r *http.Request, body []byte) {
	reqStartTime := time.Now()
	
	// v1.3.0: 客户端 ID 和请求 ID
	cliID := getOrCreateClientID(r)
	reqID := extractRequestID(r)
	if reqID == "" {
		reqID = fmt.Sprintf("orproxy-%d", time.Now().UnixNano())
	}
	w.Header().Set("X-ORProxy-Request-Id", reqID)
	
	// v1.3.0: 检查是否首次访问
	isFirst := checkFirstVisit(cliID)
	
	// v1.3.0: 获取客户端信息
	clientIP := r.RemoteAddr
	if idx := strings.LastIndex(clientIP, ":"); idx > 0 {
		clientIP = clientIP[:idx]
	}
	cliType := r.Header.Get("User-Agent")
	if cliType == "" {
		cliType = "unknown"
	}

	origLen := len(body)
	// 跳过 UTF-8 BOM (0xEF 0xBB 0xBF)
	if len(body) >= 3 && body[0] == 0xEF && body[1] == 0xBB && body[2] == 0xBF {
		body = body[3:]
		logDebug("[Chat] request_id=%s 跳过 UTF-8 BOM，节省 %dB", reqID, len(body))
	}
	// 跳过 UTF-16 BOM
	if len(body) >= 2 && ((body[0] == 0xFF && body[1] == 0xFE) || (body[0] == 0xFE && body[1] == 0xFF)) {
		logWarn("[Chat] request_id=%s 检测到 UTF-16 BOM，拒绝处理", reqID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":      map[string]string{"message": "invalid JSON (UTF-16 encoding not supported)"},
			"request_id": reqID,
		})
		return
	}
	// 跳过前导空白
	body = bytes.TrimLeft(body, " \t\r\n")
	if len(body) < origLen {
		logDebug("[Chat] request_id=%s 跳过 %d 字节前导空白", reqID, origLen-len(body))
	}

	// 仅用 map[string]interface{} 解析以提取简短字段，不限制 messages 类型
	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		// v1.3.0: JSON 解析失败时打印 body 原文和原因
		preview := truncate(body, 500)
		logWarn("[Chat] request_id=%s JSON 解析失败 body=%dB (orig=%dB): %v", reqID, len(body), origLen, err)
		logWarn("[Chat] request_id=%s body原文: %s", reqID, preview)
		// 提示可能原因
		if strings.Contains(err.Error(), "invalid character") && bytes.Contains(body, []byte("\\")) {
			logWarn("[Chat] request_id=%s 原因: body 含反斜杠转义 → 可能是被多次 JSON 编码", reqID)
		}
		logDebug("[Chat] request_id=%s body hex=%x", reqID, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":      map[string]string{"message": "invalid JSON"},
			"request_id": reqID,
		})
		return
	}
	model, _ := data["model"].(string)
	stream, _ := data["stream"].(bool)
	maxTokens := 512
	if mt, ok := data["max_tokens"].(float64); ok && mt > 0 {
		maxTokens = int(mt)
	}
	// messages 数组原样传给上游，不解析其内部结构
	msgs, _ := data["messages"].([]interface{})
	logDebug("[Chat] request_id=%s model=%s stream=%v max_tokens=%d messages=%d", reqID, model, stream, maxTokens, len(msgs))

	// v1.3.0: 创建请求上下文（用于 6 阶段日志）
	ctx := &RequestContext{
		CliID:      cliID,
		CliIDShort: shortID(cliID, 8),
		ReqID:      reqID,
		ReqIDShort: shortID(reqID, 4),
		IsFirst:    isFirst,
		StartTime:  reqStartTime,
		Model:      model,
		BodySize:   len(body),
		CliType:    cliType,
		ClientIP:   clientIP,
		Messages:   len(msgs),
		LargeBody:  len(body) > LARGE_BODY_THRESHOLD,
	}

	// v1.3.0: 📥 阶段 - 收到请求（首次显示完整信息）
	logReq(ctx, "📥", "POST /v1/chat")

	// v1.3.0: 🎯 阶段 - 选模型
	logReq(ctx, "🎯", "选模型: %s", model)

	// 每日计数
	today := time.Now().Format("2006-01-02")
	dailyMu.Lock()
	if dailyDate != today {
		if dailyCount > 0 {
			log("📊 跨日重置：前日 %d 成功/%d 失败", dailySuccess, dailyFail)
		}
		dailyDate = today
		dailyCount = 0
		dailySuccess = 0
		dailyFail = 0
		dailyRetry = 0
	}
	dailyCount++
	count := dailyCount
	dailyMu.Unlock()
	if count%10 == 0 {
		// v1.3.0: 每日统计加成功/失败分类
		log("📊 今日请求: %d (成功:%d 失败:%d 重试:%d)", dailyCount, dailySuccess, dailyFail, dailyRetry)
	}

	useFreePool := r.Header.Get("X-ORProxy-Free") == "1"
	logDebug("[Chat] request_id=%s useFreePool=%v", reqID, useFreePool)

	// 等待所有 Key 冷却（最大 3s）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(pickCandidates()) > 0 {
			break
		}
		log("⏳ 所有 Key 冷却中，等待解除...")
		time.Sleep(500 * time.Millisecond)
	}

	var allErrors []string
	success := false

	for round := 1; round <= MAX_ROUNDS; round++ {
		if round > 1 {
			dailyRetry++ // v1.3.0: 记录重试
			interval := GRADIENT_INTERVALS[round-2]
			if round-2 >= len(GRADIENT_INTERVALS) {
				interval = GRADIENT_INTERVALS[len(GRADIENT_INTERVALS)-1]
			}
			log("⏳ 第 %d/%d 轮（间隔 %ds）", round, MAX_ROUNDS, interval)
			time.Sleep(time.Duration(interval) * time.Second)
		}

		var roundErrors []string

		// 1) OpenRouter free
		if useFreePool {
			logReq(ctx, "➡️", "尝试 openrouter/free")
			if result, status, ok := tryOpenrouterFree(body); ok {
				logReq(ctx, "✅", "openrouter/free 成功 %dms", elapsedMs(reqStartTime))
				w.Header().Set("Content-Type", "application/json")
				w.Write(result)
				success = true
				return
			} else {
				roundErrors = append(roundErrors, fmt.Sprintf("free:%d", status))
				logReq(ctx, "❌", "openrouter/free 失败 status=%d", status)
			}
		} else {
			logDebug("[R%d] 跳过 OR-Free (useFreePool=false)", round)
		}

		// 2) OpenRouter Keys
		cands := pickCandidates()
		if len(cands) == 0 {
			roundErrors = append(roundErrors, "OR:all-cooling")
			log("⚠️ OpenRouter 所有 Key 冷却中 [R%d]", round)
		} else {
			for _, idx := range cands {
				env := orKeys[idx].Env
				logReq(ctx, "➡️", "尝试 %s", env)
				result, status, ok := tryOpenrouterKey(body, idx)
				if ok {
					logReq(ctx, "✅", "%s 成功 %dms", env, elapsedMs(reqStartTime))
					w.Header().Set("Content-Type", "application/json")
					w.Write(result)
					success = true
					return
				}
				roundErrors = append(roundErrors, fmt.Sprintf("key%d:%d", idx, status))
				// v1.3.0: 429 时显示特殊 emoji
				if status == 429 {
					logReq(ctx, "⏭️", "%s 429 (冷却中)", env)
				} else {
					logReq(ctx, "❌", "%s 失败 status=%d", env, status)
				}
			}
		}

		// 3) Zhipu
		if zhipuKey != "" {
			for _, glm := range []string{"glm-4-flash", "glm-5-turbo"} {
				zhipuTimeout := ZHIPU_TIMEOUT_SEC
				if ctx.LargeBody {
					// v1.3.0: 大 body 时 Zhipu timeout 保持 60s
					logReq(ctx, "➡️", "尝试 zhipu:%s (large body %s)", glm, formatBytes(ctx.BodySize))
				} else {
					logReq(ctx, "➡️", "尝试 zhipu:%s (timeout=%ds)", glm, zhipuTimeout)
				}
				zhipuStart := time.Now()
				result, status, ok := tryZhipu(body, glm)
				elapsed := time.Since(zhipuStart).Milliseconds()
				if ok {
					logReq(ctx, "✅", "zhipu:%s 成功 %dms", glm, elapsed)
					w.Header().Set("Content-Type", "application/json")
					w.Write(result)
					success = true
					return
				}
				roundErrors = append(roundErrors, fmt.Sprintf("zhipu:%s:%d", glm, status))
				// v1.3.0: Zhipu 超时时显示详细上下文
				if status == 0 || status == 599 { // timeout 通常返回 0 或自定义状态码
					logReqError(ctx, "❌", "zhipu:%s 异常: %s 耗时=%dms", glm, "timeout", elapsed)
					logReqError(ctx, "⚠️", "失败上下文: model=%s timeout=%ds body=%s", glm, zhipuTimeout, formatBytes(ctx.BodySize))
				} else {
					logReq(ctx, "❌", "zhipu:%s 失败 status=%d", glm, status)
				}
			}
		}

		allErrors = append(allErrors, fmt.Sprintf("R%d:%s", round, strings.Join(roundErrors, ";")))
		log("❌ 第 %d/%d 轮全失败: %s", round, MAX_ROUNDS, strings.Join(roundErrors, "; "))
	}

	// v1.3.0: 更新每日统计
	dailyMu.Lock()
	if success {
		dailySuccess++
	} else {
		dailyFail++
	}
	dailyMu.Unlock()

	msg := "3 轮全失败: " + strings.Join(allErrors, " | ")
	logReqError(ctx, "❌", "全轮失败: %s", msg)
	logReqError(ctx, "❌", "完成(失败) 总耗时=%dms", elapsedMs(reqStartTime))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(429)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error":      map[string]string{"message": msg},
		"status":     "all_pools_exhausted",
		"request_id": reqID,
	})
}

// ---- HTTP 服务器 ----
func main() {
	loadConfig()
	loadCreds()

	mux := http.NewServeMux()

	// CORS + 认证中间件
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-ORProxy-Free, X-Auth-Token, X-Client-Id")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		// v1.3.0: 认证检查
		if config.AuthToken != "" {
			host := r.Host
			if !(strings.HasPrefix(host, "127.") || host == "localhost" || host == "[::1]") {
				token := r.Header.Get("X-Auth-Token")
				if token == "" {
					token = r.URL.Query().Get("token")
				}
				if token != config.AuthToken {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					json.NewEncoder(w).Encode(map[string]interface{}{
						"error": map[string]string{"message": "unauthorized"},
					})
					return
				}
			}
		}
		mux.ServeHTTP(w, r)
	})

	// /health
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		var keyInfo []map[string]string
		for _, k := range orKeys {
			info := map[string]string{"key": maskKey(k.Key)}
			if cd, ok := cooldownMap.Load(k.Env); ok && cd.(int64) > now.Unix() {
				info["cooling_until"] = time.Unix(cd.(int64), 0).Format("15:04:05")
			} else {
				info["cooling_until"] = "-"
			}
			keyInfo = append(keyInfo, info)
		}
		statsMu.Lock()
		psr := map[string]map[string]interface{}{}
		for pool, attempts := range poolAttempts {
			s := poolSuccess[pool]
			f := poolFail[pool]
			rate := 0.0
			if attempts > 0 {
				rate = float64(s) / float64(attempts) * 100
			}
			psr[pool] = map[string]interface{}{
				"attempts": attempts, "success": s, "fail": f, "success_rate": rate,
			}
		}
		statsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "ok", 
			"upstream": "openrouter.ai", 
			"version": "v1.3.0",
			"keys": keyInfo,
			"daily_total": dailyCount, 
			"daily_success": dailySuccess, // v1.3.0
			"daily_fail": dailyFail,       // v1.3.0
			"daily_retry": dailyRetry,     // v1.3.0
			"daily_date": dailyDate,
			"error_stats": map[string]interface{}{
				"all_time":          map[string]interface{}{"by_status": statusAll},
				"pool_success_rate": psr,
			},
		})
	})

	// /v1/models
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "method not allowed", 405)
			return
		}
		resp, err := httpRequest("openrouter.ai", "/api/v1/models", "GET", nil, nil, 10*time.Second)
		if err != nil {
			http.Error(w, `{"error":{"message":"`+err.Error()+`"}}`, 502)
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	// /v1/chat/completions
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", 405)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"error":{"message":"read body failed"}}`, 400)
			return
		}
		handleChatCompletions(w, r, body)
	})

	server := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", config.Bind, config.Port),
		Handler: handler,
	}

	// 优雅退出
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		log("收到退出信号，关闭服务器...")
		server.Close()
	}()

	log("🚀 服务启动 http://%s:%d/v1 | %d 个 Key | v1.3.0 | log_level=%s", config.Bind, config.Port, len(orKeys), config.LogLevel)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log("服务器启动失败: %v", err)
	}
}
