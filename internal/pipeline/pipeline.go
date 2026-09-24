// Package pipeline 装配传感器校验、电池估计、故障判定、限幅与预算管理，
// 对外提供“一个处理周期 = 一份完整可解释结论”的入口。
//
// 并发模型：Pipeline 内部用一把互斥锁串行化 Step/Snapshot，子系统自身
// 也都是并发安全的。预算总预算在每个周期由最新限幅结果同步，因此故障与
// 恢复过程中预算收缩/恢复都和当期限幅结论严格一致。
package pipeline

import (
	"sync"
	"time"

	"github.com/lisama569/battery-power-control-go/internal/battery"
	"github.com/lisama569/battery-power-control-go/internal/budget"
	"github.com/lisama569/battery-power-control-go/internal/derating"
	"github.com/lisama569/battery-power-control-go/internal/fault"
	"github.com/lisama569/battery-power-control-go/internal/sensing"
)

// TickResult 是一个处理周期的完整可解释结论。
type TickResult struct {
	At         time.Time
	Rejected   []sensing.RejectedRecord
	Battery    battery.State
	Faults     fault.Status
	Limits     derating.LimitResult
	Allocation budget.Allocation
}

// StepRequest 是一个处理周期内的预算操作。
type StepRequest struct {
	Requests []budget.Request
	Release  []string
}

// Config 装配参数。
type Config struct {
	Sensing map[sensing.Channel]sensing.ChannelConfig
	Battery battery.Config
	Faults  fault.Thresholds
	Derate  derating.Params
}

// Pipeline 串联各子系统。
type Pipeline struct {
	mu        sync.Mutex
	now       func() time.Time
	estimator *sensing.Estimator
	battery   *battery.Estimator
	faults    *fault.Detector
	derater   *derating.Derater
	budgets   *budget.Manager
	cycle     int64
}

// DefaultConfig 返回各子系统默认配置的组合。
func DefaultConfig() Config {
	return Config{
		Sensing: sensing.DefaultConfigs(),
		Battery: battery.DefaultConfig(),
		Faults:  fault.DefaultThresholds(),
		Derate:  derating.DefaultParams(),
	}
}

// New 装配管线。
func New(cfg Config, now func() time.Time) *Pipeline {
	if now == nil {
		now = time.Now
	}
	d := derating.NewDerater(cfg.Derate)
	p := &Pipeline{
		now:       now,
		estimator: sensing.NewEstimator(cfg.Sensing, now),
		battery:   battery.NewEstimator(cfg.Battery),
		faults:    fault.NewDetector(cfg.Faults, now),
		derater:   d,
		// 初始预算 = 额定功率；首次 Step 后由限幅结论同步。
		budgets: budget.NewManager(d.RatedPower()),
	}
	return p
}

// Step 推进一个周期：采样校验 -> 电池估计 -> 故障判定 -> 限幅 -> 预算同步与分配。
// 整链在同一把锁内完成，返回的 TickResult 是自洽的单一版本结论。
func (p *Pipeline) Step(samples []sensing.Sample, req StepRequest) TickResult {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	rejected := p.estimator.Ingest(samples)
	readings := p.estimator.Readings()
	bs := p.battery.Update(readings, now)
	fin := fault.FromBatteryState(bs, now)
	fs := p.faults.Evaluate(fin)
	lr := p.derater.Compute(derating.LimitInputs{
		Voltage:  bs.Voltage,
		Temp:     bs.Temp,
		SOC:      bs.SOC,
		RPM:      bs.RPM,
		RPMValid: bs.RPMValid,
		At:       now,
	}, fs)

	// 预算上限与当期限幅严格同步。
	p.budgets.SetBudget(lr.MaxDischargeW)
	if len(req.Release) > 0 {
		p.budgets.Release(req.Release...)
	}
	var alloc budget.Allocation
	if len(req.Requests) > 0 {
		alloc = p.budgets.Request(req.Requests)
	} else {
		alloc = p.budgets.Snapshot()
	}

	p.cycle++
	return TickResult{
		At:         now,
		Rejected:   rejected,
		Battery:    bs,
		Faults:     fs,
		Limits:     lr,
		Allocation: alloc,
	}
}

// ResetLatched 显式清除锁存故障(如充电恢复流程)。
func (p *Pipeline) ResetLatched(codes ...fault.FaultCode) TickResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.faults.Reset(codes...)
	return p.snapshotLocked()
}

// Snapshot 返回当前结论快照(不摄入样本)。
func (p *Pipeline) Snapshot() TickResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshotLocked()
}

func (p *Pipeline) snapshotLocked() TickResult {
	now := p.now()
	bs := p.battery.Snapshot()
	fs := p.faults.Snapshot()
	lr := p.derater.Compute(derating.LimitInputs{
		Voltage:  bs.Voltage,
		Temp:     bs.Temp,
		SOC:      bs.SOC,
		RPM:      bs.RPM,
		RPMValid: bs.RPMValid,
		At:       now,
	}, fs)
	return TickResult{
		At:         now,
		Battery:    bs,
		Faults:     fs,
		Limits:     lr,
		Allocation: p.budgets.Snapshot(),
	}
}
