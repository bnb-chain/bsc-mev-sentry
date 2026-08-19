package middlewares

import "github.com/gin-gonic/gin"

// NewConcurrencySem builds a semaphore for capping in-flight requests. It is
// shared between the gin middleware and the gRPC interceptor so the two
// listeners bound TOTAL process concurrency instead of 2x when limited apart.
// Returns nil when max <= 0 (unlimited).
func NewConcurrencySem(max int64) chan struct{} {
	if max <= 0 {
		return nil
	}
	return make(chan struct{}, max)
}

// ConcurrencyLimiter limits simultaneous requests
func ConcurrencyLimiter(max int64) gin.HandlerFunc {
	return ConcurrencyLimiterWith(NewConcurrencySem(max))
}

// ConcurrencyLimiterWith limits simultaneous requests using a caller-provided
// semaphore; nil disables limiting.
func ConcurrencyLimiterWith(sem chan struct{}) gin.HandlerFunc {
	if sem == nil {
		return func(c *gin.Context) {
			c.Next()
		}
	}

	return func(c *gin.Context) {
		sem <- struct{}{}
		defer func() { <-sem }()

		c.Next()
	}
}
