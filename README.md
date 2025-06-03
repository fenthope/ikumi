# Ikumi

Touka(灯花)框架的速率限制中间件

此仓库名称来自 牧野郁美(Makino Ikumi), 固有为冻结

## 安装

```bash
go get github.com/fenthope/ikumi
```

## 使用

以下为方法演示, 单独函数的高级自定义请了解后使用

```go
func main() {
	r := touka.Default()

	// 将速率限制中间件及其所有配置直接嵌入到 r.Use() 中
	r.Use(ikumi.TokenRateLimit(ikumi.TokenRateLimiterOptions{
		Limit:            rate.Limit(5), //限制
		Burst:            10, // 桶大小
		TokensPerRequest: 1, // 每个请求消耗的令牌数量

		// KeyFunc: 示例：按User-Agent限流，回退到按IP限流
		KeyFunc: func(c *touka.Context) string {
			userAgent := c.Request.UserAgent()
			if userAgent != "" {
				return userAgent
			}
			return ikumi.DefaultTokenKeyFunc(c)
		},

		// OnLimited: 请求被拒绝时调用的回调函数
		OnLimited: func(c *touka.Context, info ikumi.RateLimitInfo) {
			log.Printf("Rate limit hit for key '%s'. Limit: %.2f req/s, Burst: %d. Retry after: %s",
				info.Key, info.LimitPerSecond, info.Burst, info.RetryAfter)
			c.SetHeader("X-Custom-RateLimit-Message", "You've sent too many requests, please slow down!")
			c.String(http.StatusTooManyRequests, "自定义：请求过于频繁，请稍后再试。")
			c.Abort()
		},

		// Skip: 示例：跳过来自 localhost 的请求
		Skip: func(c *touka.Context) bool {
			if c.ClientIP() == "127.0.0.1" || c.ClientIP() == "::1" {
				log.Printf("Skipping rate limit for localhost IP: %s", c.ClientIP())
				return true
			}
			if os.Getenv("ENV") == "development" {
				log.Println("Skipping rate limit in development environment.")
				return true
			}
			return false
		},

		// Store: 默认使用内存存储，带自动清理
		Store: ikumi.NewMemoryTokenLimiterStore(5*time.Minute, 15*time.Minute),
	}))
}
```

## 许可证

Apache 2.0 License

Copyright © Infinite-Iroha & WJQSERVER. All rights reserved.