// Package audit 实现网关请求报文的全量归档：每个客户端请求采集
// 客户端请求、实际上游请求、上游响应、客户端响应四份 Header/Body，
// 以 zstd 整体压缩写入本地目录，并提供按需流式读取的管理接口。
//
// 该包刻意不依赖 service / repository / middleware 等业务包，
// 业务侧只通过少量钩子（Gin 中间件、RoundTripper、WS Tap、OnUsage）接入，
// 以保证可作为独立补丁在基线上应用。
package audit

import (
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config 控制归档行为。所有字段都可由 config.yaml 的 `audit:` 段或
// AUDIT_* 环境变量（viper AutomaticEnv，`.` 映射为 `_`）配置。
type Config struct {
	// Enabled 为 false 时所有钩子直接透传，不产生任何文件。
	Enabled bool
	// RootDir 归档根目录，默认 ./data/audit。
	RootDir string
	// SessionHeaders 按优先级查找会话 ID 的请求头。
	SessionHeaders []string
	// RedactHeaders 落盘前需要脱敏的鉴权类 Header（大小写不敏感）。
	RedactHeaders []string
	// RedactQueryParams 落盘前需要脱敏的 URL 查询参数（例如 Gemini 的 ?key=）。
	RedactQueryParams []string
	// SessionCacheSize 会话→日期目录缓存的最大条目数（有界 LRU）。
	SessionCacheSize int
	// MemoryBufferBytes 单个文件在内存中累积到该大小前不启用流式压缩，
	// 结束时一次性压缩；超过后切换为后台流式压缩，避免大正文占用内存。
	MemoryBufferBytes int
	// EncoderLevel zstd 压缩级别：fastest / default / better / best。
	EncoderLevel string
	// DisplayBodyLimit 管理接口单段正文的展示上限（字节），超出部分只能下载；
	// 仅影响展示，不影响存储。
	DisplayBodyLimit int
	// FinalizeTimeout 关闭归档（停机）时等待未完成写入的最长时间。
	FinalizeTimeout time.Duration
	// FlushWorkers 后台落盘并发上限（创建文件 + 压缩写入），限制同时打开的文件数。
	FlushWorkers int
}

// DefaultSessionHeaders 覆盖常见客户端的会话头：
// Claude Code（X-Claude-Code-Session-Id）、Codex（session_id / conversation_id）、
// OpenRouter 及通用客户端（X-Session-Id / Session-Id）、OpenCode、Gemini CLI。
var DefaultSessionHeaders = []string{
	"X-Claude-Code-Session-Id",
	"session_id",
	"Session-Id",
	"X-Session-Id",
	"conversation_id",
	"X-OpenCode-Session",
	"X-Gemini-Session-Id",
	"X-Goog-Session-Id",
}

// DefaultRedactHeaders 仅包含鉴权类 Header。
var DefaultRedactHeaders = []string{
	"Authorization",
	"Proxy-Authorization",
	"X-Api-Key",
	"Api-Key",
	"X-Goog-Api-Key",
	"Cookie",
	"Set-Cookie",
	"X-Auth-Token",
}

// DefaultRedactQueryParams 仅包含鉴权类查询参数。
var DefaultRedactQueryParams = []string{"key", "api_key", "access_token"}

// DefaultConfig 返回默认配置（默认开启，根目录 ./data/audit）。
func DefaultConfig() Config {
	return Config{
		Enabled:           true,
		RootDir:           "./data/audit",
		SessionHeaders:    append([]string(nil), DefaultSessionHeaders...),
		RedactHeaders:     append([]string(nil), DefaultRedactHeaders...),
		RedactQueryParams: append([]string(nil), DefaultRedactQueryParams...),
		SessionCacheSize:  10000,
		MemoryBufferBytes: 512 << 10,
		EncoderLevel:      "fastest",
		DisplayBodyLimit:  4 << 20,
		FinalizeTimeout:   30 * time.Second,
		FlushWorkers:      max(4, 2*runtime.GOMAXPROCS(0)),
	}
}

// ConfigFromViper 从全局 viper 读取 `audit.*`，缺省项回落到 DefaultConfig。
// 主配置加载（config.Load）已对全局 viper 启用 AutomaticEnv，因此
// AUDIT_ENABLED / AUDIT_ROOT_DIR 等环境变量同样生效。
func ConfigFromViper() Config {
	cfg := DefaultConfig()
	if viper.IsSet("audit.enabled") {
		cfg.Enabled = viper.GetBool("audit.enabled")
	}
	if v := strings.TrimSpace(viper.GetString("audit.root_dir")); v != "" {
		cfg.RootDir = v
	}
	if v := viper.GetStringSlice("audit.session_headers"); len(v) > 0 {
		cfg.SessionHeaders = splitList(v)
	}
	if v := viper.GetStringSlice("audit.redact_headers"); len(v) > 0 {
		cfg.RedactHeaders = splitList(v)
	}
	if v := viper.GetStringSlice("audit.redact_query_params"); len(v) > 0 {
		cfg.RedactQueryParams = splitList(v)
	}
	if v := viper.GetInt("audit.session_cache_size"); v > 0 {
		cfg.SessionCacheSize = v
	}
	if v := viper.GetInt("audit.memory_buffer_bytes"); v > 0 {
		cfg.MemoryBufferBytes = v
	}
	if v := strings.TrimSpace(viper.GetString("audit.encoder_level")); v != "" {
		cfg.EncoderLevel = v
	}
	if v := viper.GetInt("audit.display_body_limit"); v > 0 {
		cfg.DisplayBodyLimit = v
	}
	if v := viper.GetDuration("audit.finalize_timeout"); v > 0 {
		cfg.FinalizeTimeout = v
	}
	if v := viper.GetInt("audit.flush_workers"); v > 0 {
		cfg.FlushWorkers = v
	}
	return cfg
}

// splitList 兼容环境变量中逗号分隔的写法。
func splitList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func canonicalSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		set[strings.ToLower(v)] = struct{}{}
		set[strings.ToLower(http.CanonicalHeaderKey(v))] = struct{}{}
	}
	return set
}
