package ikumi

import (
	"fmt"
	"net/http"
	"strconv"

	"math"

	"sync"
	"time"

	"github.com/infinite-iroha/touka"
	"golang.org/x/time/rate"
)

// TokenRateLimiterXStore 定义了存储和检索 golang.org/x/time/rate.Limiter 实例的接口。
// 这允许在不同存储后端（如内存、Redis）之间切换，以支持单机或分布式速率限制。
type TokenRateLimiterXStore interface {
	// GetLimiter 获取或创建一个与指定 key 关联的速率限制器。
	// limit 参数定义了每秒允许的平均事件数 (令牌生成速率)。
	// burst 参数定义了令牌桶的容量 (允许的最大突发事件数)。
	// 实现需要确保对同一个 key 返回的是同一个 *rate.Limiter 实例，以保证状态一致。
	GetLimiter(key string, limit rate.Limit, burst int) *rate.Limiter

	// Cleanup 执行存储后端的清理操作，例如移除长时间不活动的限制器实例。
	// inactiveDuration 定义了限制器被视为空闲并可被清理前的最大不活动时间。
	// 此方法主要用于内存存储等需要手动管理的后端。
	Cleanup(inactiveDuration time.Duration)

	// StopCleanup (可选) 用于优雅地停止任何在后台运行的清理任务。
	StopCleanup()
}

// memoryTokenLimiterStore 是 TokenRateLimiterXStore 接口的一个基于内存的实现。
// 它为每个唯一的 key 存储一个 *rate.Limiter 实例。
type memoryTokenLimiterStore struct {
	limiters map[string]*rate.Limiter // 存储 key 到 Limiter 的映射
	mu       sync.RWMutex             // 保护对 limiters map 的并发访问

	lastAccessed map[string]time.Time // 记录每个 Limiter 的最后访问时间，用于清理
	accessMu     sync.Mutex           // 保护对 lastAccessed map 的并发访问

	cleanupInterval time.Duration // 定期清理的间隔
	inactiveTimeout time.Duration // Limiter 被视为不活动前的超时时间
	cleanupTicker   *time.Ticker  // 用于定期触发清理的定时器
	stopCleanupChan chan struct{} // 用于信号通知清理 goroutine 停止
}

// NewMemoryTokenLimiterStore 创建一个新的 memoryTokenLimiterStore 实例。
// cleanupInterval: 定期执行清理操作的间隔。如果为0，则不启动自动清理。
// inactiveDurationForCleanup: Limiter 在被清理前可以保持不活动状态的最长时间。
func NewMemoryTokenLimiterStore(cleanupInterval time.Duration, inactiveDurationForCleanup time.Duration) TokenRateLimiterXStore {
	store := &memoryTokenLimiterStore{
		limiters:        make(map[string]*rate.Limiter),
		lastAccessed:    make(map[string]time.Time),
		cleanupInterval: cleanupInterval,
		inactiveTimeout: inactiveDurationForCleanup,
		stopCleanupChan: make(chan struct{}),
	}

	if store.cleanupInterval > 0 && store.inactiveTimeout > 0 {
		store.cleanupTicker = time.NewTicker(store.cleanupInterval)
		go store.runCleanupLoop() // 在后台 goroutine 中运行清理循环
	}
	return store
}

// GetLimiter 从内存存储中获取或创建一个新的 *rate.Limiter。
// 如果 key 对应的 Limiter 已存在，则返回现有实例并更新其最后访问时间。
// 如果不存在，则使用提供的 limit 和 burst 参数创建一个新的 Limiter，存储并返回。
// 此方法是并发安全的。
func (s *memoryTokenLimiterStore) GetLimiter(key string, limit rate.Limit, burst int) *rate.Limiter {
	now := time.Now()

	// 首先尝试读锁下快速路径获取
	s.mu.RLock()
	limiter, exists := s.limiters[key]
	s.mu.RUnlock()

	if exists {
		s.accessMu.Lock()
		s.lastAccessed[key] = now // 更新最后访问时间
		s.accessMu.Unlock()
		return limiter
	}

	// Limiter 不存在，需要获取写锁创建
	s.mu.Lock()
	defer s.mu.Unlock()

	// 双重检查，防止在获取写锁期间其他 goroutine 已创建
	if limiter, exists = s.limiters[key]; exists {
		s.accessMu.Lock()
		s.lastAccessed[key] = now
		s.accessMu.Unlock()
		return limiter
	}

	// 创建新的 Limiter
	limiter = rate.NewLimiter(limit, burst)
	s.limiters[key] = limiter
	s.accessMu.Lock()
	s.lastAccessed[key] = now // 记录创建（即首次访问）时间
	s.accessMu.Unlock()
	return limiter
}

// runCleanupLoop 是一个内部方法，在后台 goroutine 中定期调用 Cleanup。
func (s *memoryTokenLimiterStore) runCleanupLoop() {
	// 使用英文记录日志，说明清理任务已启动
	for {
		select {
		case <-s.cleanupTicker.C: // 等待定时器触发
			s.Cleanup(s.inactiveTimeout)
		case <-s.stopCleanupChan: // 收到停止信号
			s.cleanupTicker.Stop() // 停止定时器
			return
		}
	}
}

// Cleanup 从内存存储中移除在 inactiveDuration 时间内没有活动的 Limiter 实例。
// 这是为了防止内存无限增长。此方法是并发安全的。
func (s *memoryTokenLimiterStore) Cleanup(inactiveDuration time.Duration) {
	s.mu.Lock()       // 获取对 limiters map 的写锁
	s.accessMu.Lock() // 获取对 lastAccessed map 的写锁
	defer s.mu.Unlock()
	defer s.accessMu.Unlock()

	// 使用英文记录日志，包含清理前的 Limiter 数量
	now := time.Now()
	removedCount := 0
	for key, lastAccessTime := range s.lastAccessed {
		if now.Sub(lastAccessTime) > inactiveDuration {
			delete(s.limiters, key)     // 从 limiters map 中删除
			delete(s.lastAccessed, key) // 从 lastAccessed map 中删除
			removedCount++
		}
	}
}

// StopCleanup 停止后台的清理 goroutine。
// 如果没有运行清理 goroutine，此方法不执行任何操作。
func (s *memoryTokenLimiterStore) StopCleanup() {
	if s.cleanupTicker != nil {
		// 检查 stopCleanupChan 是否已关闭，避免重复关闭
		// 这可以通过一个 once 标志或尝试非阻塞发送来实现，但简单关闭通常是安全的
		select {
		case <-s.stopCleanupChan: // 已经关闭了
			return
		default:
			close(s.stopCleanupChan) // 发送停止信号
		}
	}
}

// RateLimitInfo 封装了传递给 OnLimited 回调的速率限制相关信息。
type RateLimitInfo struct {
	Key            string        // 触发限制的 key (例如 IP 地址)
	LimitPerSecond float64       // 配置的每秒平均限制速率
	Burst          int           // 配置的突发容量
	RetryAfter     time.Duration // 建议的重试等待时间 (可能为0)
}

// TokenRateLimiterOptions 配置基于 golang.org/x/time/rate 的速率限制中间件。
type TokenRateLimiterOptions struct {
	// Limit 是每秒允许的事件（请求）的平均速率。
	// 例如，rate.Limit(10) 表示每秒10个事件。
	Limit rate.Limit
	// Burst 是令牌桶的容量，即允许的最大突发请求数。
	// 例如，如果 Burst 是 20，则可以立即处理20个请求（如果桶是满的）。
	Burst int
	// TokensPerRequest 定义每个请求需要从令牌桶中消耗多少个令牌。
	// 默认为1。如果设置为大于1的值，则会更快地消耗令牌。
	// 必须小于或等于 Burst。
	TokensPerRequest int

	// KeyFunc 是一个函数，用于从 Touka Context 中为每个请求提取一个唯一的字符串 key。
	// 这个 key 用于区分不同的速率限制桶（例如，每个IP一个桶）。
	// 如果为 nil，默认使用 c.ClientIP()。
	KeyFunc func(c *touka.Context) string

	// OnLimited 是一个可选的回调函数，当请求因为达到速率限制而被拒绝时调用。
	// 它接收 RateLimitInfo，其中包含有关限制的详细信息。
	// 此函数【必须】负责向客户端发送适当的错误响应并中止 Touka Context 的处理链。
	// 如果为 nil，中间件将使用 Touka 框架的默认错误处理器发送 429 Too Many Requests 响应。
	OnLimited func(c *touka.Context, info RateLimitInfo)

	// Skip 是一个可选函数，如果返回 true，则当前请求将跳过速率限制检查。
	// 可用于实现白名单或基于其他条件跳过限制。
	// 默认为 nil，表示不跳过任何请求。
	Skip func(c *touka.Context) bool

	// Store 是用于存储和管理 rate.Limiter 实例的后端。
	// 如果为 nil，将使用一个基于内存的存储 (NewMemoryTokenLimiterStore)，
	// 默认每5分钟清理一次15分钟内不活动的限制器。
	Store TokenRateLimiterXStore
}

// defaultTokenKeyFunc 是默认的 KeyFunc 实现，使用客户端的 IP 地址。
func defaultTokenKeyFunc(c *touka.Context) string {
	ip := c.ClientIP()
	if ip == "" {
		// 如果无法获取客户端 IP (例如在某些测试环境或特殊网络设置下)，
		// 返回一个固定的字符串。注意：这会导致所有这类请求共享同一个速率限制桶。
		// 在生产环境中，应确保 ClientIP() 能可靠工作。
		return "touka-ratelimit-unknown-client-ip"
	}
	return ip
}

// defaultTokenOnLimited 是默认的 OnLimited 实现。
// 它发送一个标准的 HTTP 429 Too Many Requests 响应，并设置相关的速率限制头部。
func defaultTokenOnLimited(c *touka.Context, info RateLimitInfo) {
	// 设置 Retry-After 头部 (单位：秒)
	if info.RetryAfter > 0 {
		// 向上取整到最近的秒，因为 Retry-After 通常是整数秒
		retrySeconds := int(math.Ceil(info.RetryAfter.Seconds()))
		c.SetHeader("Retry-After", strconv.Itoa(retrySeconds))
	}
	// 设置标准的速率限制信息头部 (尽管这些头部没有统一的 RFC 标准，但被广泛使用)
	// X-RateLimit-Limit: 每秒的速率限制 (可以转换为每分钟或每小时，如果更符合业务)
	// X-RateLimit-Burst: 突发容量
	// 注意：X-RateLimit-Remaining (剩余请求数) 很难从 golang.org/x/time/rate.Limiter 精确获取，
	// 因为它内部不直接暴露当前令牌数。通常不提供此头部，或提供一个估算值。
	// X-RateLimit-Reset (重置时间的 Unix 时间戳) 也不容易精确计算，Retry-After 更实用。

	c.SetHeader("X-RateLimit-Limit-Per-Second", fmt.Sprintf("%.2f", info.LimitPerSecond))
	c.SetHeader("X-RateLimit-Burst-Capacity", strconv.Itoa(info.Burst))

	c.ErrorUseHandle(http.StatusTooManyRequests)
	// 确保 Touka 处理链被中止
	if !c.IsAborted() {
		c.Abort()
	}
}

// TokenRateLimit 返回一个基于 golang.org/x/time/rate 的速率限制中间件。
// 它为每个由 KeyFunc 生成的唯一 key 维护一个令牌桶。
func TokenRateLimit(opts TokenRateLimiterOptions) touka.HandlerFunc {
	// --- 配置默认值 ---
	if opts.Limit <= 0 { // rate.Limit 是 float64，0 或负值表示无限制或无效
		opts.Limit = rate.Limit(5) // 默认每秒5个事件 (令牌)
	}
	if opts.Burst <= 0 {
		opts.Burst = 20 // 默认突发容量为20
	}
	if opts.TokensPerRequest <= 0 {
		opts.TokensPerRequest = 1 // 每个请求默认消耗1个令牌
	}
	// 确保每次请求消耗的令牌数不超过突发容量，否则 AllowN 将永远失败
	if opts.TokensPerRequest > opts.Burst {
		// 使用英文记录警告日志
		//log.Printf("toukautil.TokenRateLimit: Warning - TokensPerRequest (%d) is greater than Burst (%d). Adjusting TokensPerRequest to Burst.", opts.TokensPerRequest, opts.Burst)
		opts.TokensPerRequest = opts.Burst
	}

	if opts.KeyFunc == nil {
		opts.KeyFunc = defaultTokenKeyFunc
	}
	if opts.OnLimited == nil {
		opts.OnLimited = defaultTokenOnLimited
	}
	if opts.Store == nil {
		// 默认使用内存存储，每5分钟清理一次15分钟内不活动的限制器
		opts.Store = NewMemoryTokenLimiterStore(5*time.Minute, 15*time.Minute)
	}

	// --- 中间件处理函数 ---
	return func(c *touka.Context) {
		// 1. 检查是否应跳过此请求的速率限制
		if opts.Skip != nil && opts.Skip(c) {
			c.Next() // 跳过，继续处理链
			return
		}

		// 2. 生成当前请求的唯一 key
		key := opts.KeyFunc(c)
		if key == "" {
			// 如果 KeyFunc 返回空字符串，通常表示无法为该请求确定一个有效的限制目标。
			// 在这种情况下，可以选择跳过限制并记录警告，或者应用一个全局的默认限制。
			// 当前选择跳过并记录。
			// 使用英文记录警告日志
			//log.Println("toukautil.TokenRateLimit: Warning - KeyFunc returned an empty string. Skipping rate limit for this request.")
			c.Warnf("toukautil.TokenRateLimit: Warning - KeyFunc returned an empty string. Skipping rate limit for this request.")
			c.Next()
			return
		}

		// 3. 从存储中获取或创建此 key 对应的 rate.Limiter 实例
		limiter := opts.Store.GetLimiter(key, opts.Limit, opts.Burst)

		// 4. 尝试从此 Limiter 中获取 N 个令牌 (N = opts.TokensPerRequest)
		// limiter.AllowN 是非阻塞的，它会立即返回是否允许。
		// 如果需要等待令牌（不适合HTTP中间件），可以使用 limiter.WaitN(ctx, n)。
		now := time.Now() // 获取当前时间，传递给 AllowN
		allowed := limiter.AllowN(now, opts.TokensPerRequest)

		// 5. 设置标准的速率限制相关的 HTTP 响应头
		// 这些头部通常在所有情况下都设置，以便客户端了解当前的限制策略。
		c.SetHeader("X-RateLimit-Limit-Per-Second", fmt.Sprintf("%.2f", float64(opts.Limit)))
		c.SetHeader("X-RateLimit-Burst-Capacity", strconv.Itoa(opts.Burst))
		// X-RateLimit-Remaining (剩余令牌数) 无法从 golang.org/x/time/rate.Limiter 精确获取。
		// 可以选择不设置此头部，或者提供一个估算值（例如，如果 allowed，则为 Burst - TokensPerRequest，但这不准确）。
		// 为简单和准确起见，这里不设置 X-RateLimit-Remaining。

		// 6. 处理限制结果
		if !allowed {
			// 请求被速率限制器拒绝
			// 计算建议的 Retry-After 时间
			reserve := limiter.ReserveN(now, opts.TokensPerRequest) // 获取一个预留
			if !reserve.OK() {
				// 如果 AllowN 返回 false，ReserveN 通常会返回 OK 并给出 Delay。
				// 如果 ReserveN 也返回 !OK，可能意味着请求的令牌数大于 Burst，
				// 或者速率是 Inf。在这种情况下，拒绝是正确的，但 RetryAfter 可能不明确。
				// 使用英文记录警告日志
				//log.Printf("toukautil.TokenRateLimit: Limiter.ReserveN indicated not OK for key '%s' after AllowN was false. This might indicate TokensPerRequest > Burst or infinite rate.", key)
				c.Warnf("toukautil.TokenRateLimit: Limiter.ReserveN indicated not OK for key '%s' after AllowN was false. This might indicate TokensPerRequest > Burst or infinite rate.", key)
				opts.OnLimited(c, RateLimitInfo{
					Key:            key,
					LimitPerSecond: float64(opts.Limit),
					Burst:          opts.Burst,
					RetryAfter:     0, // 不确定的重试时间
				})
				// OnLimited 应该已经调用了 c.Abort()
				return
			}
			// 获取需要等待的时间。如果 reserve.Delay() 返回0，表示可以立即重试（理论上不应与 AllowN=false 同时发生，除非TokensPerRequest>Burst）。
			// 如果返回非常大的值 (time.Duration(math.MaxInt64))，表示永远不可用（例如速率为0且桶空）。
			delay := reserve.DelayFrom(now) // 从当前时间计算延迟
			// reserve.CancelAt(now) // 如果我们不打算实际“使用”这个预留（我们只是用它来获取delay），可以取消它。
			// 但对于 AllowN 之后的 ReserveN，通常不需要取消，因为它不会实际消耗令牌。

			// 调用用户提供的 OnLimited 处理函数
			opts.OnLimited(c, RateLimitInfo{
				Key:            key,
				LimitPerSecond: float64(opts.Limit),
				Burst:          opts.Burst,
				RetryAfter:     delay,
			})
			// OnLimited 回调负责发送响应并中止 Touka Context
			return
		}

		// 请求被允许，继续处理链中的下一个 HandlerFunc
		c.Next()
	}
}
