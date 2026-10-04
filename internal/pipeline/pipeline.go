// Package pipeline 将采样校验、状态估计、限功率推导与预算分配串成一条管线。
package pipeline

import (
	"sync"
	"time"

	"github.com/lisama569/battery-power-control-go/internal/budget"
	"github.com/lisama569/battery-power-control-go/internal/limits"
	"github.com/lisama569/battery-power-control-go/internal/sensor"
)

// Config 是管线构造参数。
type Config struct {
	CapacityAh    float64
	RatedPowerW   float64
	RatedTorqueNm float64
	InitialSOC    float64
	Specs         map[sensor.Channel]sensor.ChannelSpec
}

// DefaultConfig 返回一套可用的默认配置。
func DefaultConfig() Config {
	return Config{
		CapacityAh:    80,
		RatedPowerW:   150000,
		RatedTorqueNm: 320,
		InitialSOC:    0.8,
		Specs:         sensor.DefaultChannelSpecs(),
	}
}

// RejectedSample 记录被拒绝样本及原因。
type RejectedSample struct {
	Sample sensor.Sample
	Reason sensor.RejectReason
	Detail string
}

// Report 是管线一次评估的完整、可解释报告。
type Report struct {
	At         time.Time
	Estimate   sensor.StateEstimate
	Decision   limits.Decision
	Allocation budget.Allocation
	Rejected   []RejectedSample
	Freshness  []sensor.Verdict // 本轮评估时各通道缺失/陈旧情况
	// ExpiredBoosts 是本轮评估中到期归还的增援单元（按 ID 升序）。
	ExpiredBoosts []string
}

// Pipeline 持有整条管线的状态。Evaluate 全程持锁，
// 保证“限功率结论 → 总预算 → 分配快照”三者作为一个一致原子对外可见。
type Pipeline struct {
	mu        sync.Mutex
	validator *sensor.Validator
	estimator *sensor.Estimator
	engine    *limits.Engine
	manager   *budget.Manager
	cfg       Config
}

// New 按配置构造管线。
func New(cfg Config) *Pipeline {
	v := sensor.NewValidator(cfg.Specs)
	e := sensor.NewEstimator(cfg.CapacityAh, cfg.Specs)
	e.Reset(cfg.InitialSOC)
	return &Pipeline{
		validator: v,
		estimator: e,
		engine:    limits.NewEngine(cfg.RatedPowerW, cfg.RatedTorqueNm),
		manager:   budget.NewManager(cfg.RatedPowerW),
		cfg:       cfg,
	}
}

// Ingest 接收一条样本：通过校验才送入估计器；否则记录拒绝原因。
// 方法本身并发安全；拒绝样本可由返回的 Verdict 直接区分原因。
func (p *Pipeline) Ingest(s sensor.Sample, now time.Time) sensor.Verdict {
	p.mu.Lock()
	defer p.mu.Unlock()
	verdict := p.validator.Validate(s, now)
	if verdict.Accept {
		p.estimator.Apply(s)
	}
	return verdict
}

// Evaluate 用当前状态估计与本轮功率申请产出一份原子一致的报告。
// 限功率结论先确定，总预算随结论更新，再在该预算内完成本轮分配。
func (p *Pipeline) Evaluate(units []budget.Unit, now time.Time) Report {
	p.mu.Lock()
	defer p.mu.Unlock()

	rep := Report{At: now, Rejected: []RejectedSample{}}

	for _, fv := range p.validator.Freshness(now) {
		rep.Freshness = append(rep.Freshness, fv)
	}

	est := p.estimator.Estimate(now)
	rep.Estimate = est

	dec := p.engine.Evaluate(est)
	rep.Decision = dec

	// 先结算到期增援（归还被挤占额度），再把总预算对齐到最新限功率结论
	// （收紧时按优先级从低到高回收，剩余额度不为负），最后做本轮分配。
	// 三步在同一临界区内完成，外部看到的结论、预算、快照始终一致。
	rep.ExpiredBoosts = p.manager.ExpireBoosts(now)
	p.manager.ApplyBudget(dec.PowerLimitW, now)
	rep.Allocation = p.manager.Allocate(units)
	return rep
}

// RequestBoost 为单元发起一段有时限的功率增援（如超车、避障）：
// 窗口内其有效优先级被临时抬高，可从更低优先级单元挤占额度；
// 窗口到期（下一次 Evaluate/ExpireBoosts）或 StopBoost 后自动归还。
func (p *Pipeline) RequestBoost(u budget.Unit, spec budget.BoostSpec, now time.Time) budget.Grant {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.manager.StartBoost(u, spec, now)
}

// StopBoost 提前结束单元的增援并立即归还被挤占额度（幂等）。
func (p *Pipeline) StopBoost(unitID string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.manager.StopBoost(unitID, now)
}

// Request 单独申请/调整一个单元的预留（使用当前限功率预算）。
func (p *Pipeline) Request(u budget.Unit) budget.Grant {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.manager.Request(u)
}

// Release 释放单元预留，返回释放的瓦数（幂等）。
func (p *Pipeline) Release(unitID string) float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.manager.Release(unitID)
}

// Recover 在喂入新的有效样本后重新评估，用于从故障中恢复。
// 若当前估计仍无效，保持安全零输出态并原样返回结论。
// 预算恢复时立即重算：故障期间被回收的登记按优先级干净复原，不留残留占用。
func (p *Pipeline) Recover(now time.Time) limits.Decision {
	p.mu.Lock()
	defer p.mu.Unlock()
	est := p.estimator.Estimate(now)
	dec := p.engine.Evaluate(est)
	p.manager.ApplyBudget(dec.PowerLimitW, now)
	return dec
}

// Snapshot 返回当前预算的一致快照。
func (p *Pipeline) Snapshot() budget.Allocation {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.manager.Snapshot()
}

// Reset 清空采样与故障历史并回到初始 SOC，但保留总预算配置。
// 用于把彼此独立的模拟工况隔开；预算表中的预留也一并清空。
func (p *Pipeline) Reset(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.validator = sensor.NewValidator(p.cfg.Specs)
	p.estimator = sensor.NewEstimator(p.cfg.CapacityAh, p.cfg.Specs)
	p.estimator.Reset(p.cfg.InitialSOC)
	p.engine = limits.NewEngine(p.cfg.RatedPowerW, p.cfg.RatedTorqueNm)
	p.manager = budget.NewManager(p.cfg.RatedPowerW)
}
