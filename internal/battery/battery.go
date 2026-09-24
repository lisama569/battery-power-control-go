// Package battery 依据有效电流样本估计电池荷电状态(SOC)与电池聚合状态。
//
// SOC 通过安时积分更新：soc -= I*dt / CapacityAs(放电为正)。
// 只有“当前有效”的电流读数才参与积分；电流样本缺失/陈旧时积分冻结，
// 且 State.CurrentValid=false，下游故障与限幅不得把该读数当作可信输入。
package battery

import (
	"sync"
	"time"

	"github.com/lisama569/battery-power-control-go/internal/sensing"
)

// Config 描述电池额定参数与 SOC 保护带。
type Config struct {
	CapacityAs   float64       // 额定容量，单位安培秒
	VNominal     float64       // 标称电压(V)
	SOCInit      float64       // 初始 SOC [0,1]
	SOCFloor     float64       // SOC 下限(低于则禁止放电)
	SOCEmergency float64       // 紧急下限
	UpdateMaxAge time.Duration // 电流读数允许的最大陈旧度
}

// State 是电池聚合状态。
type State struct {
	SOC          float64
	Voltage      float64
	Current      float64 // 放电为正，充电/回馈为负
	Temp         float64
	RPM          float64
	CurrentAt    time.Time
	CurrentValid bool
	VoltageValid bool
	TempValid    bool
	RPMValid     bool
}

// Estimator 通过安时积分维护 SOC。
type Estimator struct {
	mu      sync.Mutex
	cfg     Config
	state   State
	prevAt  time.Time
	started bool
}

// DefaultConfig 返回默认电池配置(示例：400V、约 60kWh 电池组)。
func DefaultConfig() Config {
	return Config{
		CapacityAs:   540000, // 150Ah * 3600s
		VNominal:     360,
		SOCInit:      0.85,
		SOCFloor:     0.20,
		SOCEmergency: 0.10,
		UpdateMaxAge: 300 * time.Millisecond,
	}
}

// NewEstimator 创建电池状态估计器。
func NewEstimator(cfg Config) *Estimator {
	if cfg.CapacityAs <= 0 {
		cfg.CapacityAs = 1
	}
	soc := cfg.SOCInit
	if soc < 0 {
		soc = 0
	}
	if soc > 1 {
		soc = 1
	}
	return &Estimator{
		cfg:   cfg,
		state: State{SOC: soc},
	}
}

// Update 用最新读数推进 SOC 估计。只有有效电流参与积分，
// 无效期间积分冻结并记录数据缺口；dt 异常(<=0 或超过 MaxAge)不积分。
func (e *Estimator) Update(readings map[sensing.Channel]sensing.Reading, now time.Time) State {
	e.mu.Lock()
	defer e.mu.Unlock()

	v := readings[sensing.ChannelVoltage]
	i := readings[sensing.ChannelCurrent]
	tp := readings[sensing.ChannelTemp]
	rp := readings[sensing.ChannelRPM]

	e.state.Voltage = v.Value
	e.state.VoltageValid = v.Valid
	e.state.Temp = tp.Value
	e.state.TempValid = tp.Valid
	e.state.RPM = rp.Value
	e.state.RPMValid = rp.Valid

	if i.Valid {
		// 仅在存在可信的上次积分时刻且时间窗口合理时积分。
		if e.started {
			dt := i.At.Sub(e.prevAt)
			if dt > 0 && dt <= e.cfg.UpdateMaxAge {
				next := e.state.SOC - i.Value*dt.Seconds()/e.cfg.CapacityAs
				if next < 0 {
					next = 0
				}
				if next > 1 {
					next = 1
				}
				e.state.SOC = next
			}
		}
		e.prevAt = i.At
		e.started = true
		e.state.Current = i.Value
		e.state.CurrentAt = i.At
		e.state.CurrentValid = true
	} else {
		// 电流缺失/陈旧：冻结积分，明确标记无效，防止静默参与下游计算。
		e.state.CurrentValid = false
		e.state.CurrentAt = i.At
	}
	return e.snapshotLocked()
}

// Snapshot 返回当前电池状态。
func (e *Estimator) Snapshot() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshotLocked()
}

func (e *Estimator) snapshotLocked() State { return e.state }
