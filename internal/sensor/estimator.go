package sensor

import (
	"sync"
	"time"
)

// requiredChannels 是产出有效状态估计所必需的通道集合。
var requiredChannels = []Channel{ChannelVoltage, ChannelCurrent, ChannelTemp, ChannelRPM}

// point 保存一路通道的最近值。
type point struct {
	value float64
	at    time.Time
}

// Estimator 融合已接受的样本，给出状态估计（含库仑积分 SOC）。
// 约定：电流为正表示放电，SOC 随时间积分下降。
// 所有方法均并发安全。
type Estimator struct {
	mu         sync.Mutex
	capacityAh float64
	specs      map[Channel]ChannelSpec
	values     map[Channel]point
	soc        float64
	lastCurrAt time.Time
}

// NewEstimator 构造 Estimator。capacityAh 为电池容量（安时），specs 用于新鲜度判定。
func NewEstimator(capacityAh float64, specs map[Channel]ChannelSpec) *Estimator {
	copied := make(map[Channel]ChannelSpec, len(specs))
	for k, v := range specs {
		copied[k] = v
	}
	return &Estimator{
		capacityAh: capacityAh,
		specs:      copied,
		values:     make(map[Channel]point),
		soc:        0.8,
	}
}

// Reset 清空积分并设定初始 SOC（0..1）。
func (e *Estimator) Reset(initialSOC float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if initialSOC < 0 {
		initialSOC = 0
	}
	if initialSOC > 1 {
		initialSOC = 1
	}
	e.values = make(map[Channel]point)
	e.soc = initialSOC
	e.lastCurrAt = time.Time{}
}

// Apply 接收一条已通过校验的样本并更新估计。
func (e *Estimator) Apply(s Sample) {
	e.mu.Lock()
	defer e.mu.Unlock()

	prev := e.values[s.Channel]
	e.values[s.Channel] = point{value: s.Value, at: s.Timestamp}

	if s.Channel == ChannelCurrent && e.capacityAh > 0 {
		if !e.lastCurrAt.IsZero() && s.Timestamp.After(e.lastCurrAt) {
			dt := s.Timestamp.Sub(e.lastCurrAt).Seconds()
			// 用区间平均电流（梯形积分）降低离散误差。
			avg := 0.5 * (prev.value + s.Value)
			dSOC := -avg * dt / 3600.0 / e.capacityAh
			e.soc += dSOC
			if e.soc < 0 {
				e.soc = 0
			}
			if e.soc > 1 {
				e.soc = 1
			}
		}
		e.lastCurrAt = s.Timestamp
	}
}

// Estimate 返回当前时刻的融合状态估计。
// Valid 为 false 时 Notes 给出具体缺失/陈旧通道，调用方不得用该估计做功率决策。
func (e *Estimator) Estimate(now time.Time) StateEstimate {
	e.mu.Lock()
	defer e.mu.Unlock()

	st := StateEstimate{
		At:    now,
		SOC:   e.soc,
		Valid: true,
	}
	if p, ok := e.values[ChannelVoltage]; ok {
		st.Voltage = p.value
	}
	if p, ok := e.values[ChannelCurrent]; ok {
		st.Current = p.value
	}
	if p, ok := e.values[ChannelTemp]; ok {
		st.Temperature = p.value
	}
	if p, ok := e.values[ChannelRPM]; ok {
		st.RPM = p.value
	}

	for _, ch := range requiredChannels {
		p, ok := e.values[ch]
		spec, hasSpec := e.specs[ch]
		switch {
		case !ok:
			st.Valid = false
			st.Notes = append(st.Notes, string(ch)+":"+string(ReasonMissing))
		case hasSpec && now.Sub(p.at) > spec.StaleAge:
			st.Valid = false
			st.Notes = append(st.Notes, string(ch)+":"+string(ReasonStale))
		}
	}
	return st
}

// SOC 返回当前荷电状态（0..1）。
func (e *Estimator) SOC() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.soc
}
