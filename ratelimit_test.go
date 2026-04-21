package ikumi

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/infinite-iroha/touka"
	"golang.org/x/time/rate"
)

func TestNewMemoryTokenLimiterStore(t *testing.T) {
	store := NewMemoryTokenLimiterStore(5*time.Minute, 15*time.Minute)
	if store == nil {
		t.Fatal("NewMemoryTokenLimiterStore returned nil")
	}
}

func TestMemoryTokenLimiterStore_GetLimiter(t *testing.T) {
	store := NewMemoryTokenLimiterStore(0, 0) // 禁用自动清理
	defer store.StopCleanup()

	// 测试创建新的 limiter
	limiter1 := store.GetLimiter("key1", rate.Limit(10), 20)
	if limiter1 == nil {
		t.Fatal("GetLimiter returned nil")
	}

	// 测试获取已存在的 limiter
	limiter2 := store.GetLimiter("key1", rate.Limit(10), 20)
	if limiter1 != limiter2 {
		t.Error("GetLimiter should return the same limiter for the same key")
	}

	// 测试不同 key 返回不同的 limiter
	limiter3 := store.GetLimiter("key2", rate.Limit(5), 10)
	if limiter1 == limiter3 {
		t.Error("GetLimiter should return different limiters for different keys")
	}
}

func TestMemoryTokenLimiterStore_GetLimiter_Concurrent(t *testing.T) {
	store := NewMemoryTokenLimiterStore(0, 0)
	defer store.StopCleanup()

	var wg sync.WaitGroup
	concurrency := 100
	key := "concurrent-key"

	// 并发获取同一个 key 的 limiter
	limiters := make([]*rate.Limiter, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			limiters[idx] = store.GetLimiter(key, rate.Limit(10), 20)
		}(i)
	}
	wg.Wait()

	// 验证所有 goroutine 获取到的是同一个 limiter
	first := limiters[0]
	for i := 1; i < concurrency; i++ {
		if limiters[i] != first {
			t.Errorf("limiter at index %d is different from the first", i)
		}
	}
}

func TestMemoryTokenLimiterStore_Cleanup(t *testing.T) {
	store := &memoryTokenLimiterStore{
		stopCleanupChan: make(chan struct{}),
	}

	// 手动添加一些 entries
	now := time.Now()
	store.limiters.Store("active", &limiterEntry{
		limiter:      rate.NewLimiter(10, 20),
		lastAccessed: now,
	})
	store.limiters.Store("inactive", &limiterEntry{
		limiter:      rate.NewLimiter(10, 20),
		lastAccessed: now.Add(-20 * time.Minute), // 20 分钟前访问
	})

	// 执行清理，移除超过 15 分钟不活动的 entries
	store.Cleanup(15 * time.Minute)

	// 验证 active entry 仍然存在
	if _, ok := store.limiters.Load("active"); !ok {
		t.Error("active entry should not be cleaned up")
	}

	// 验证 inactive entry 已被移除
	if _, ok := store.limiters.Load("inactive"); ok {
		t.Error("inactive entry should be cleaned up")
	}
}

func TestMemoryTokenLimiterStore_StopCleanup(t *testing.T) {
	store := NewMemoryTokenLimiterStore(100*time.Millisecond, 50*time.Millisecond)

	// 调用 StopCleanup 多次不应该 panic
	store.StopCleanup()
	store.StopCleanup()
	store.StopCleanup()
}

func TestTokenRateLimit_Basic(t *testing.T) {
	r := touka.New()

	// 配置速率限制：每秒 10 个请求，突发容量 10
	r.Use(TokenRateLimit(TokenRateLimiterOptions{
		Limit: rate.Limit(10),
		Burst: 10,
	}))

	r.GET("/test", func(c *touka.Context) {
		c.String(http.StatusOK, "OK")
	})

	// 发送请求
	for i := 0; i < 15; i++ {
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if i < 10 {
			// 前 10 个请求应该成功
			if w.Code != http.StatusOK {
				t.Errorf("request %d: expected status 200, got %d", i, w.Code)
			}
		} else {
			// 超过突发容量的请求应该被限制
			if w.Code != http.StatusTooManyRequests {
				t.Errorf("request %d: expected status 429, got %d", i, w.Code)
			}
		}
	}
}

func TestTokenRateLimit_CustomKeyFunc(t *testing.T) {
	r := touka.New()

	r.Use(TokenRateLimit(TokenRateLimiterOptions{
		Limit: rate.Limit(5),
		Burst: 5,
		KeyFunc: func(c *touka.Context) string {
			// 使用自定义 key 函数
			return "custom-key"
		},
	}))

	r.GET("/test", func(c *touka.Context) {
		c.String(http.StatusOK, "OK")
	})

	// 所有请求使用同一个 key，应该共享限制
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest("GET", "/test", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if i < 5 {
			if w.Code != http.StatusOK {
				t.Errorf("request %d: expected status 200, got %d", i, w.Code)
			}
		} else {
			if w.Code != http.StatusTooManyRequests {
				t.Errorf("request %d: expected status 429, got %d", i, w.Code)
			}
		}
	}
}

func TestTokenRateLimit_Skip(t *testing.T) {
	r := touka.New()

	r.Use(TokenRateLimit(TokenRateLimiterOptions{
		Limit: rate.Limit(5),
		Burst: 5,
		Skip: func(c *touka.Context) bool {
			// 跳过白名单 IP
			return c.ClientIP() == "192.168.1.1"
		},
	}))

	r.GET("/test", func(c *touka.Context) {
		c.String(http.StatusOK, "OK")
	})

	// 白名单 IP 的请求应该始终成功
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "192.168.1.1:12345"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("whitelisted IP request %d: expected status 200, got %d", i, w.Code)
		}
	}
}

func TestTokenRateLimit_OnLimited(t *testing.T) {
	r := touka.New()
	limitHit := false

	r.Use(TokenRateLimit(TokenRateLimiterOptions{
		Limit: rate.Limit(5),
		Burst: 5,
		OnLimited: func(c *touka.Context, info RateLimitInfo) {
			limitHit = true
			c.SetHeader("X-Custom-Rate-Limited", "true")
			c.String(http.StatusTooManyRequests, "Custom rate limit message")
			c.Abort()
		},
	}))

	r.GET("/test", func(c *touka.Context) {
		c.String(http.StatusOK, "OK")
	})

	// 发送超过限制的请求
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
	}

	if !limitHit {
		t.Error("OnLimited callback should have been called")
	}
}

func TestTokenRateLimit_ResponseHeaders(t *testing.T) {
	r := touka.New()

	r.Use(TokenRateLimit(TokenRateLimiterOptions{
		Limit: rate.Limit(10),
		Burst: 20,
	}))

	r.GET("/test", func(c *touka.Context) {
		c.String(http.StatusOK, "OK")
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// 验证响应头
	limitHeader := w.Header().Get("X-RateLimit-Limit-Per-Second")
	if limitHeader != "10.00" {
		t.Errorf("expected X-RateLimit-Limit-Per-Second header to be '10.00', got '%s'", limitHeader)
	}

	burstHeader := w.Header().Get("X-RateLimit-Burst-Capacity")
	if burstHeader != "20" {
		t.Errorf("expected X-RateLimit-Burst-Capacity header to be '20', got '%s'", burstHeader)
	}
}

func TestTokenRateLimit_DefaultValues(t *testing.T) {
	r := touka.New()

	// 使用空配置，应该使用默认值
	r.Use(TokenRateLimit(TokenRateLimiterOptions{}))

	r.GET("/test", func(c *touka.Context) {
		c.String(http.StatusOK, "OK")
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// 默认 Limit 是 5，Burst 是 20
	limitHeader := w.Header().Get("X-RateLimit-Limit-Per-Second")
	if limitHeader != "5.00" {
		t.Errorf("expected default X-RateLimit-Limit-Per-Second header to be '5.00', got '%s'", limitHeader)
	}

	burstHeader := w.Header().Get("X-RateLimit-Burst-Capacity")
	if burstHeader != "20" {
		t.Errorf("expected default X-RateLimit-Burst-Capacity header to be '20', got '%s'", burstHeader)
	}
}

func TestTokenRateLimit_TokensPerRequest(t *testing.T) {
	r := touka.New()

	r.Use(TokenRateLimit(TokenRateLimiterOptions{
		Limit:            rate.Limit(10),
		Burst:            10,
		TokensPerRequest: 3, // 每个请求消耗 3 个令牌
	}))

	r.GET("/test", func(c *touka.Context) {
		c.String(http.StatusOK, "OK")
	})

	// 每个请求消耗 3 个令牌，Burst 是 10
	// 所以前 3 个请求应该成功 (3*3=9 <= 10)，第 4 个请求应该失败
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if i < 3 {
			if w.Code != http.StatusOK {
				t.Errorf("request %d: expected status 200, got %d", i, w.Code)
			}
		} else {
			if w.Code != http.StatusTooManyRequests {
				t.Errorf("request %d: expected status 429, got %d", i, w.Code)
			}
		}
	}
}

func TestTokenRateLimit_TokensPerRequestAdjustment(t *testing.T) {
	// TokensPerRequest 大于 Burst 时应该自动调整
	opts := TokenRateLimiterOptions{
		Limit:            rate.Limit(10),
		Burst:            5,
		TokensPerRequest: 10, // 大于 Burst
	}

	// 创建中间件会调整 TokensPerRequest
	handler := TokenRateLimit(opts)

	r := touka.New()
	r.Use(handler)
	r.GET("/test", func(c *touka.Context) {
		c.String(http.StatusOK, "OK")
	})

	// 应该能正常工作，因为 TokensPerRequest 已被调整为 Burst
	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200 after TokensPerRequest adjustment, got %d", w.Code)
	}
}

func TestTokenRateLimit_DifferentIPs(t *testing.T) {
	r := touka.New()

	r.Use(TokenRateLimit(TokenRateLimiterOptions{
		Limit: rate.Limit(5),
		Burst: 5,
	}))

	r.GET("/test", func(c *touka.Context) {
		c.String(http.StatusOK, "OK")
	})

	// 不同 IP 应该有独立的限制
	ips := []string{"192.168.1.1:12345", "192.168.1.2:12345", "192.168.1.3:12345"}

	for _, ip := range ips {
		// 每个 IP 发送 5 个请求，应该都成功
		for i := 0; i < 5; i++ {
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = ip
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("IP %s request %d: expected status 200, got %d", ip, i, w.Code)
			}
		}

		// 第 6 个请求应该被限制
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = ip
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusTooManyRequests {
			t.Errorf("IP %s request 6: expected status 429, got %d", ip, w.Code)
		}
	}
}

func BenchmarkGetLimiter(b *testing.B) {
	store := NewMemoryTokenLimiterStore(0, 0)
	defer store.StopCleanup()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			store.GetLimiter("benchmark-key", rate.Limit(10), 20)
		}
	})
}

func BenchmarkGetLimiter_DifferentKeys(b *testing.B) {
	store := NewMemoryTokenLimiterStore(0, 0)
	defer store.StopCleanup()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := "key-" + string(rune(i%100))
			store.GetLimiter(key, rate.Limit(10), 20)
			i++
		}
	})
}

func BenchmarkTokenRateLimit(b *testing.B) {
	r := touka.New()
	r.Use(TokenRateLimit(TokenRateLimiterOptions{
		Limit: rate.Limit(1000),
		Burst: 1000,
	}))
	r.GET("/test", func(c *touka.Context) {
		c.String(http.StatusOK, "OK")
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "127.0.0.1:12345"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
	}
}
