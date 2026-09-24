// Package sensor 负责模拟传感器采样的有效性判定与状态估计。
package sensor

import "time"

// Channel 标识采样通道。
type Channel string

const (
	ChannelVoltage Channel = "voltage"
	ChannelCurrent Channel = "current"
	ChannelTemp    Channel = "temperature"
	ChannelRPM     Channel = "rpm"
)

// Sample 是一路模拟采样。
type Sample struct {
	Channel   Channel
	Value     float64
	Timestamp time.Time
	Seq       int64
}

// RejectReason 描述样本被拒绝的可区分原因。
type RejectReason string

const (
	ReasonOK           RejectReason = ""
	ReasonMissing      RejectReason = "missing"        // 缺失/未送达
	ReasonStale        RejectReason = "stale"          // 时间戳陈旧
	ReasonOutOfRange   RejectReason = "out_of_range"   // 超量程
	ReasonJump         RejectReason = "jump"           // 相对上一有效读数跳变过大
	ReasonFuture       RejectReason = "future"         // 时间戳在未来
	ReasonBadChannel   RejectReason = "bad_channel"    // 未知通道
	ReasonNoPriorValue RejectReason = "no_prior_value" // 首帧无法做跳变判定之外的估计
)

// Verdict 是单条样本的判定结果。
type Verdict struct {
	Sample Sample
	Accept bool
	Reason RejectReason
	Detail string
}

// ChannelSpec 描述一个通道的量程与跳变阈值。
type ChannelSpec struct {
	Min      float64
	Max      float64
	MaxStep  float64 // 相邻有效样本最大允许变化量
	StaleAge time.Duration
	HasJump  bool // 是否对该通道做跳变判定
}

// StateEstimate 是某一时刻融合后的电池/电机状态估计。
type StateEstimate struct {
	At          time.Time
	Voltage     float64
	Current     float64
	Temperature float64
	RPM         float64
	SOC         float64 // 0..1，由电流积分估计
	Valid       bool
	Notes       []string
}
