// Package sensing 对模拟传感器读数进行有效性判定与状态估计。
//
// 读数跳变、缺失、超量程或时间戳陈旧的样本不会静默参与计算，
// 而是被拒绝并返回可区分的拒绝原因(RejectReason)。
//
// 单个样本按固定优先级判定，只返回首个命中的原因：
//
//  1. out_of_range  : 通道未知或数值超出 [Min, Max]
//  2. stale         : 样本时间戳早于当前时间 MaxAge 以上(或落在未来)
//  3. bad_interval  : 与上一有效样本的时间间隔超出 [MinInterval, MaxInterval]
//  4. jump          : 与上一有效样本的数值变化超过 MaxDelta
//
// 通道没有任何有效样本(缺失)或最新有效样本已过新鲜度窗口时，
// Readings 会把该通道标记为 Valid=false 并给出 missing/stale 原因。
package sensing

import (
	"sort"
	"sync"
	"time"
)

// Channel 标识采样通道。
type Channel string

const (
	ChannelVoltage Channel = "voltage"
	ChannelCurrent Channel = "current"
	ChannelTemp    Channel = "temp"
	ChannelRPM     Channel = "rpm"
)

// RejectReason 样本被拒绝的可区分原因。
type RejectReason string

const (
	RejectNone        RejectReason = ""
	RejectOutOfRange  RejectReason = "out_of_range"
	RejectStale       RejectReason = "stale"
	RejectJump        RejectReason = "jump"
	RejectMissing     RejectReason = "missing"
	RejectBadInterval RejectReason = "bad_interval"
	RejectUnknownChan RejectReason = "unknown_channel"
)

// Sample 是一路传感器的一次原始采样。
type Sample struct {
	Channel Channel
	Value   float64
	At      time.Time
}

// ChannelConfig 描述一路通道的量程、跳变与新鲜度约束。
type ChannelConfig struct {
	Min         float64
	Max         float64
	MaxDelta    float64       // 相邻有效样本最大允许变化量(绝对值)
	MaxAge      time.Duration // 读数被视为陈旧前的最大年龄
	MinInterval time.Duration // 相邻有效样本最小间隔
	MaxInterval time.Duration // 相邻有效样本最大间隔
}

// RejectedRecord 记录一次被拒绝的样本及其原因。
type RejectedRecord struct {
	Sample Sample
	Reason RejectReason
	Prev   *float64 // 拒绝时上一有效样本值(如有)，用于日志解释
}

// Reading 是一路通道经校验后的最新估计。
type Reading struct {
	Channel Channel
	Value   float64
	At      time.Time
	Valid   bool
	Reason  RejectReason // 无效时为 missing 或 stale
}

// ChannelEstimator 对单通道样本做校验并维护估计值。
type ChannelEstimator struct {
	cfg    ChannelConfig
	last   *float64
	lastAt time.Time
}

// Estimator 汇总多路通道估计器，可并发使用。
type Estimator struct {
	mu       sync.Mutex
	channels map[Channel]*ChannelEstimator
	now      func() time.Time
}

// DefaultConfigs 返回四路通道的默认校验配置。
func DefaultConfigs() map[Channel]ChannelConfig {
	return map[Channel]ChannelConfig{
		ChannelVoltage: {
			Min: 240, Max: 420,
			MaxDelta:    80,
			MaxAge:      250 * time.Millisecond,
			MinInterval: 5 * time.Millisecond,
			MaxInterval: 500 * time.Millisecond,
		},
		ChannelCurrent: {
			Min: -200, Max: 400,
			MaxDelta:    250,
			MaxAge:      250 * time.Millisecond,
			MinInterval: 5 * time.Millisecond,
			MaxInterval: 500 * time.Millisecond,
		},
		ChannelTemp: {
			Min: -40, Max: 105,
			MaxDelta:    20,
			MaxAge:      500 * time.Millisecond,
			MinInterval: 5 * time.Millisecond,
			MaxInterval: 1000 * time.Millisecond,
		},
		ChannelRPM: {
			Min: 0, Max: 20000,
			MaxDelta:    6000,
			MaxAge:      300 * time.Millisecond,
			MinInterval: 5 * time.Millisecond,
			MaxInterval: 600 * time.Millisecond,
		},
	}
}

// NewEstimator 创建多通道估计器。
func NewEstimator(configs map[Channel]ChannelConfig, now func() time.Time) *Estimator {
	ch := make(map[Channel]*ChannelEstimator, len(configs))
	for c, cfg := range configs {
		cc := cfg
		ch[c] = &ChannelEstimator{cfg: cc}
	}
	return &Estimator{channels: ch, now: now}
}

// Ingest 接收一批原始样本(可乱序、可含多通道)，按通道与时间排序后
// 依次校验，返回所有被拒绝的记录(顺序与判定原因可复现)。
func (e *Estimator) Ingest(samples []Sample) []RejectedRecord {
	e.mu.Lock()
	defer e.mu.Unlock()

	ordered := make([]Sample, len(samples))
	copy(ordered, samples)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Channel != ordered[j].Channel {
			return ordered[i].Channel < ordered[j].Channel
		}
		return ordered[i].At.Before(ordered[j].At)
	})

	now := e.now()
	var rejected []RejectedRecord
	for _, s := range ordered {
		ce, ok := e.channels[s.Channel]
		if !ok {
			rejected = append(rejected, RejectedRecord{Sample: s, Reason: RejectUnknownChan})
			continue
		}
		if reason, accepted := ce.Accept(s, now); !accepted {
			rec := RejectedRecord{Sample: s, Reason: reason}
			if ce.last != nil {
				v := *ce.last
				rec.Prev = &v
			}
			rejected = append(rejected, rec)
		}
	}
	return rejected
}

// Readings 返回当前各通道最新有效读数；
// 从未收到有效样本标记 missing，超过新鲜度窗口标记 stale。
func (e *Estimator) Readings() map[Channel]Reading {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	out := make(map[Channel]Reading, len(e.channels))
	for c, ce := range e.channels {
		v, at, valid, reason := ce.value(now)
		out[c] = Reading{Channel: c, Value: v, At: at, Valid: valid, Reason: reason}
	}
	return out
}

// NewChannelEstimator 创建单通道估计器（测试用）。
func NewChannelEstimator(cfg ChannelConfig) *ChannelEstimator {
	return &ChannelEstimator{cfg: cfg}
}

// Accept 判定单个样本是否可接受；可接受时更新最新有效估计。
func (c *ChannelEstimator) Accept(s Sample, now time.Time) (RejectReason, bool) {
	if s.Value < c.cfg.Min || s.Value > c.cfg.Max {
		return RejectOutOfRange, false
	}
	age := now.Sub(s.At)
	if age > c.cfg.MaxAge || age < 0 {
		return RejectStale, false
	}
	if c.last != nil {
		dt := s.At.Sub(c.lastAt)
		if dt < c.cfg.MinInterval || dt > c.cfg.MaxInterval {
			return RejectBadInterval, false
		}
		if d := s.Value - *c.last; d > c.cfg.MaxDelta || d < -c.cfg.MaxDelta {
			return RejectJump, false
		}
	}
	v := s.Value
	c.last = &v
	c.lastAt = s.At
	return RejectNone, true
}

// Value 返回当前估计值与有效性(供单通道测试使用)。
func (c *ChannelEstimator) Value(now time.Time) (float64, bool) {
	v, _, valid, _ := c.value(now)
	return v, valid
}

func (c *ChannelEstimator) value(now time.Time) (float64, time.Time, bool, RejectReason) {
	if c.last == nil {
		return 0, time.Time{}, false, RejectMissing
	}
	if now.Sub(c.lastAt) > c.cfg.MaxAge {
		return *c.last, c.lastAt, false, RejectStale
	}
	return *c.last, c.lastAt, true, RejectNone
}

// LastAt 返回最近一次有效样本时间(测试用)。
func (c *ChannelEstimator) LastAt() time.Time { return c.lastAt }
