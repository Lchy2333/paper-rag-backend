package httpapi

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

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
