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
	"gopkg.in/yaml.v3"
	"strings"
	"sync"
	"syscall"
	"time"
)

const CONFIG_FILE = "orproxy-config.yaml"
const CREDS_FILE = "orproxy-creds.yaml"
const OR_COOLDOWN_SEC = 10
const MAX_ROUNDS = 3

type Config struct {
	Port     int    `yaml:"port"`
	LogLevel string `yaml:"log_level"`
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
	statsMu      sync.Mutex
	poolAttempts = map[string]int{}
	poolSuccess  = map[string]int{}
	poolFail     = map[string]int{}
	statusAll    = map[string]int{}

	orKeyLocks = sync.Map{} // env -> chan struct{}
)

func getOrKeyLock(env string) chan struct{} {
	v, _ := orKeyLocks.LoadOrStore(env, make(chan struct{}, 1))
	return v.(chan struct{})
}

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
	ts := now.Format("2006-01-02 15:04:05")
	msg := fmt.Sprintf("[%s] [%s] ", ts, level) + fmt.Sprintf(format, args...)
	fmt.Fprintln(os.Stderr, msg)
	if err := os.MkdirAll("./log", 0755); err == nil {
		hourKey := now.Format("20060102-15")
		logFile := fmt.Sprintf("./log/%s-%s.log", strings.ToLower(level), hourKey)
		if f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
			f.WriteString(msg + "\n")
			f.Close()
		}
	}
}

func loadConfig() {
	defaultCfg := Config{Port: 18887, LogLevel: "INFO"}
	data, err := os.ReadFile(CONFIG_FILE)
	if err != nil {
		// 文件不存在，生成默认配置
		log("⚙️ 配置文件 %s 不存在，使用默认配置: port=%d, log_level=%s", CONFIG_FILE, defaultCfg.Port, defaultCfg.LogLevel)
		writeDefaultConfig(defaultCfg)
		config = &defaultCfg
		logLevel = levelToNum(defaultCfg.LogLevel)
		return
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		logWarn("⚙️ 配置文件解析失败，使用默认配置: %v", err)
		cfg = defaultCfg
	}
	if cfg.Port == 0 {
		cfg.Port = 18887
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "INFO"
	}
	config = &cfg
	logLevel = levelToNum(cfg.LogLevel)
	log("⚙️ 配置加载成功: port=%d, log_level=%s", cfg.Port, cfg.LogLevel)
}

func writeDefaultConfig(cfg Config) {
	data, _ := yaml.Marshal(cfg)
	os.WriteFile(CONFIG_FILE, data, 0644)
}

func loadCreds() {
	data, err := os.ReadFile(CREDS_FILE)
	if err != nil {
		log("读取凭据失败: %v", err)
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
		logError("[HTTP] 构造请求失败 %s %s: %v", method, hostname+path, err)
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	logDebug("[HTTP] → %s %s%s body=%dB timeout=%s", method, hostname, path, len(body), timeout)
	resp, err := client.Do(req)
	if err != nil {
		logError("[HTTP] ✗ %s %s 异常: %v", method, hostname+path, err)
		return nil, err
	}
	logDebug("[HTTP] ← %s %s status=%d", method, hostname+path, resp.StatusCode)
	return resp, nil
}

func httpRequestLocal(hostname string, port int, path, method string, body []byte, headers map[string]string, timeout time.Duration) (*http.Response, error) {
	client := &http.Client{Timeout: timeout}
	url := fmt.Sprintf("http://%s:%d%s", hostname, port, path)
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		logError("[HTTP] 构造本地请求失败 %s %s: %v", method, url, err)
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	logDebug("[HTTP] → %s %s body=%dB timeout=%s", method, url, len(body), timeout)
	resp, err := client.Do(req)
	if err != nil {
		logDebug("[HTTP] ✗ %s %s 异常: %v", method, url, err)
		return nil, err
	}
	logDebug("[HTTP] ← %s %s status=%d", method, url, resp.StatusCode)
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

// ---- 2) OpenRouter Keys (轮转 + 串行锁 + 冷却) ----
func pickCandidates() []int {
	now := time.Now().Unix()
	start := 0
	lastGoodMu.Lock()
	if lastGoodIdx >= 0 && lastGoodIdx < len(orKeys)-1 {
		start = lastGoodIdx + 1
	}
	lastGoodMu.Unlock()

	var candidates []int
	for i := 0; i < len(orKeys); i++ {
		idx := (start + i) % len(orKeys)
		env := orKeys[idx].Env
		if cd, ok := cooldownMap.Load(env); ok && cd.(int64) > now {
			logDebug("[OR-Key] 跳过 %s (冷却至 %s)", maskKey(orKeys[idx].Key), time.Unix(cd.(int64), 0).Format("15:04:05"))
			continue
		}
		candidates = append(candidates, idx)
	}
	logDebug("[OR-Key] 候选 %d 个 (总 %d, 起点 %d): %v", len(candidates), len(orKeys), start, candidates)
	return candidates
}

func tryOpenrouterKey(data []byte, idx int) ([]byte, int, bool) {
	// 串行锁：同一 Key 同时只能一个请求
	lock := getOrKeyLock(orKeys[idx].Env)
	lock <- struct{}{}        // 获取锁（满则等待）
	defer func() { <-lock }() // 释放锁

	env := orKeys[idx].Env
	key := orKeys[idx].Key
	now := time.Now()

	// 10s 冷却检查
	if lastTs, ok := lastCallTs.Load(env); ok {
		remaining := OR_COOLDOWN_SEC - int(now.Unix()-lastTs.(int64))
		if remaining > 0 {
			log("⏭️ %s 还在 %ds 冷却，跳过", maskKey(key), remaining)
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
		log("🔁 %s 429 → 冷却 10s", maskKey(key))
	case 401:
		cooldownMap.Store(env, time.Now().Unix()+86400)
		log("⛔ %s 401 → 冷却 24h", maskKey(key))
	case 403:
		cooldownMap.Store(env, time.Now().Unix()+3600)
		log("⛔ %s 403 → 冷却 1h", maskKey(key))
	case 402:
		log("⛔ %s 402 需付费，跳过", maskKey(key))
	}
	return respBody, resp.StatusCode, false
}

// ---- 3) Zhipu ----
func tryZhipu(data []byte, glmModel string) ([]byte, int, bool) {
	if zhipuKey == "" {
		logDebug("[Zhipu] 跳过：zhipuKey 为空")
		return nil, 0, false
	}
	zhipuMu.Lock()
	defer zhipuMu.Unlock()

	if time.Since(zhipuLastTs) < time.Second {
		logDebug("[Zhipu] 1RPS 限流跳过 model=%s since=%s", glmModel, time.Since(zhipuLastTs))
		log("⏭️ Zhipu 1RPS 限流，跳过")
		return nil, 0, false
	}
	zhipuLastTs = time.Now()

	var reqData map[string]interface{}
	json.Unmarshal(data, &reqData)
	reqData["model"] = glmModel
	body, _ := json.Marshal(reqData)
	logDebug("[Zhipu] 尝试 model=%s", glmModel)

	resp, err := httpRequest("open.bigmodel.cn", "/api/paas/v4/chat/completions", "POST", body, map[string]string{
		"Authorization": "Bearer " + zhipuKey,
	}, 20*time.Second)
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

// ---- 主处理逻辑 ----
func handleChatCompletions(w http.ResponseWriter, r *http.Request, body []byte) {
	reqID := extractRequestID(r)
	if reqID == "" {
		reqID = fmt.Sprintf("orproxy-%d", time.Now().UnixNano())
	}
	w.Header().Set("X-ORProxy-Request-Id", reqID)
	logDebug("[Chat] request_id=%s remote=%s", reqID, r.RemoteAddr)

	origLen := len(body)
	// 跳过 UTF-8 BOM (0xEF 0xBB 0xBF)
	if len(body) >= 3 && body[0] == 0xEF && body[1] == 0xBB && body[2] == 0xBF {
		body = body[3:]
		logDebug("[Chat] request_id=%s 跳过 UTF-8 BOM，剩余 %dB", reqID, len(body))
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

	// 只用 map[string]interface{} 解析以提取轻量字段，不限制 messages 类型
	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		preview := truncate(body, 200)
		logWarn("[Chat] request_id=%s JSON 解析失败 body=%dB (orig=%dB): %v | body=%s", reqID, len(body), origLen, err, preview)
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
	// messages 数组原样透传给上游，不解析其内部结构
	msgs, _ := data["messages"].([]interface{})
	logDebug("[Chat] request_id=%s model=%s stream=%v max_tokens=%d messages=%d", reqID, model, stream, maxTokens, len(msgs))

	// 每日计数
	today := time.Now().Format("2006-01-02")
	dailyMu.Lock()
	if dailyDate != today {
		if dailyCount > 0 {
			log("📊 跨日重置：前日 %d 次", dailyCount)
		}
		dailyDate = today
		dailyCount = 0
	}
	dailyCount++
	count := dailyCount
	dailyMu.Unlock()
	if count%10 == 0 {
		log("📊 今日请求: %d", count)
	}

	useFreePool := r.Header.Get("X-ORProxy-Free") == "1"
	logDebug("[Chat] request_id=%s useFreePool=%v", reqID, useFreePool)

	// 等待全 Key 冷却（最多 3s）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(pickCandidates()) > 0 {
			break
		}
		log("⏳ 全 Key 冷却中，等待解冻...")
		time.Sleep(500 * time.Millisecond)
	}

	var allErrors []string

	for round := 1; round <= MAX_ROUNDS; round++ {
		if round > 1 {
			interval := GRADIENT_INTERVALS[round-2]
			if round-2 >= len(GRADIENT_INTERVALS) {
				interval = GRADIENT_INTERVALS[len(GRADIENT_INTERVALS)-1]
			}
			log("⏳ 第 %d/%d 轮（等 %ds）", round, MAX_ROUNDS, interval)
			time.Sleep(time.Duration(interval) * time.Second)
		}

		var roundErrors []string

		// 1) OpenRouter free
		if useFreePool {
			if result, status, ok := tryOpenrouterFree(body); ok {
				log("✅ OpenRouter free 成功 [R%d] request_id=%s", round, reqID)
				w.Header().Set("Content-Type", "application/json")
				w.Write(result)
				return
			} else {
				roundErrors = append(roundErrors, fmt.Sprintf("free:%d", status))
				logDebug("[R%d] OR-Free 失败 status=%d", round, status)
			}
		} else {
			logDebug("[R%d] 跳过 OR-Free (useFreePool=false)", round)
		}

		// 3) OpenRouter Keys
		cands := pickCandidates()
		if len(cands) == 0 {
			roundErrors = append(roundErrors, "OR:all-cooling")
			log("⏳ OpenRouter 全 Key 冷却中 [R%d]", round)
		} else {
			for _, idx := range cands {
				result, status, ok := tryOpenrouterKey(body, idx)
				if ok {
					log("✅ %s 成功 [R%d] request_id=%s", maskKey(orKeys[idx].Key), round, reqID)
					w.Header().Set("Content-Type", "application/json")
					w.Write(result)
					return
				}
				roundErrors = append(roundErrors, fmt.Sprintf("key%d:%d", idx, status))
			}
		}

		// 4) Zhipu
		if zhipuKey != "" {
			for _, glm := range []string{"glm-4-flash", "glm-5-turbo"} {
				result, status, ok := tryZhipu(body, glm)
				if ok {
					log("✅ Zhipu %s 成功 [R%d] request_id=%s", glm, round, reqID)
					w.Header().Set("Content-Type", "application/json")
					w.Write(result)
					return
				}
				roundErrors = append(roundErrors, fmt.Sprintf("zhipu:%s:%d", glm, status))
			}
		}

		allErrors = append(allErrors, fmt.Sprintf("R%d:%s", round, strings.Join(roundErrors, ";")))
		log("❌ 第 %d/%d 轮全失败: %s", round, MAX_ROUNDS, strings.Join(roundErrors, "; "))
	}

	msg := "3 轮全失败: " + strings.Join(allErrors, " | ")
	log("💀 request_id=%s %s", reqID, msg)
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

	// CORS 中间件
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-ORProxy-Free")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
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
			"status": "ok", "upstream": "openrouter.ai", "keys": keyInfo,
			"daily_total": dailyCount, "daily_date": dailyDate,
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
		Addr:    fmt.Sprintf("127.0.0.1:%d", config.Port),
		Handler: handler,
	}

	// 优雅退出
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		log("收到退出信号，关闭服务器")
		server.Close()
	}()

	log("🚀 服务启动 http://127.0.0.1:%d/v1 · %d 个 Key · log_level=%s", config.Port, len(orKeys), config.LogLevel)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log("服务器启动失败: %v", err)
	}
}