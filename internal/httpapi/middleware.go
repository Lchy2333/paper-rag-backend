package httpapi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// newRequestID 生成 12 位 hex rid（与 Agent 侧 uuid4().hex[:12] 同长度，便于跨服务 grep）。
func newRequestID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败极罕见（系统熵源故障）；退化为纳秒时间戳，保证不空、基本唯一
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// RequestID 链路追踪中间件：透传上游 X-Request-ID，否则自生成。
//
// 信任链：Agent access_log_middleware 生成/透传 rid → HttpTool/RagProxy 注入请求头
// → 本中间件 → c.GetString("rid")（[ACCESS] 与业务日志同 id，跨服务 grep 同一个 rid）。
// 直连调用（curl / 压测脚本）没有上游，就地生成——RAG 独立入口时也能自查。
// run_id：压测批次标记（X-Load-Test 头），生产流量没有、记 "-"；仅作事后统计切片，不影响运行时行为。
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := strings.TrimSpace(c.GetHeader("X-Request-ID"))
		if rid == "" {
			rid = newRequestID()
		}
		runID := strings.TrimSpace(c.GetHeader("X-Load-Test"))
		if runID == "" {
			runID = "-"
		}
		c.Set("rid", rid)
		c.Set("run_id", runID)
		c.Writer.Header().Set("X-Request-ID", rid) // 回显：调用方报障时直接引用
		c.Next()
	}
}

// APIKeyAuth 服务间鉴权中间件：校验请求头 X-API-Key。
//
// requiredKey 为空表示未启用鉴权（本地开发模式，所有请求放行）；
// 生产环境通过环境变量注入（config.yaml: api_key: "${RAG_API_KEY}"）。
// /health 不挂本中间件（见 router.go），供 docker-compose healthcheck 探活使用。
func APIKeyAuth(requiredKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if requiredKey == "" {
			c.Next()
			return
		}
		key := c.GetHeader("X-API-Key")
		// 常量时间比较：无论对错耗时一致，防止通过响应时间逐字节猜测 key（时序攻击）
		if subtle.ConstantTimeCompare([]byte(key), []byte(requiredKey)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "invalid or missing API key",
			})
			return
		}
		c.Next()
	}
}

// UserContext 用户身份中间件：从 X-User-ID 头取归属者注入 context。
//
// 信任链：Agent 认证（JWT）→ 注入 X-User-ID 头 → 本中间件 → c.GetString("user_id")。
// 多用户后所有文档都必须有归属，因此缺失 X-User-ID 直接 401（不做 "local" 兜底）。
func UserContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		uid := strings.TrimSpace(c.GetHeader("X-User-ID"))
		if uid == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "missing X-User-ID header",
			})
			return
		}
		c.Set("user_id", uid)
		c.Next()
	}
}
