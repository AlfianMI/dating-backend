package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

type CacheMiddleware struct {
	redis *redis.Client
}

func NewCacheMiddleware(r *redis.Client) *CacheMiddleware {
	return &CacheMiddleware{redis: r}
}

func (m *CacheMiddleware) Cache(ttl time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := fmt.Sprintf("cache:%s:%s", c.Request.Method, c.Request.URL.String())

		// Try to get from cache
		val, err := m.redis.Get(context.Background(), key).Result()
		if err == nil {
			var cachedResponse interface{}

			if unmarshalErr := json.Unmarshal([]byte(val), &cachedResponse); unmarshalErr == nil {
				c.JSON(http.StatusOK, cachedResponse)
				c.Abort()
				return
			}
		}

		// Custom writer to capture response
		w := &responseWriter{body: "", ResponseWriter: c.Writer}
		c.Writer = w

		c.Next()

		// Cache if successful
		if c.Writer.Status() == http.StatusOK {
			m.redis.Set(context.Background(), key, w.body, ttl)
		}
	}
}

type responseWriter struct {
	gin.ResponseWriter
	body string
}

func (w *responseWriter) Write(b []byte) (int, error) {
	w.body += string(b)
	return w.ResponseWriter.Write(b)
}
