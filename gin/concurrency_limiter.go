package middlewares

import "github.com/gin-gonic/gin"

// ConcurrencyLimiter limits simultaneous requests
func ConcurrencyLimiter(max int64) gin.HandlerFunc {
	if max <= 0 {
		return func(c *gin.Context) {
			c.Next()
		}
	}

	lc := make(chan struct{}, max)
	return func(c *gin.Context) {
		lc <- struct{}{}
		defer func() { <-lc }()

		c.Next()
	}
}
