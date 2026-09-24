package sensor

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// DefaultChannelSpecs 返回四个标准通道的默认判定参数。
func DefaultChannelSpecs() map[Channel]ChannelSpec {
	return map[Channel]ChannelSpec{
		ChannelVoltage: {Min: 240, Max: 430, MaxStep: 30, StaleAge: 250 * time.Millisecond, HasJump: true},
		ChannelCurrent: {Min: -400, Max: 400, MaxStep: 350, StaleAge: 250 * time.Millisecond, HasJump: true},
		ChannelTemp:    {Min: -40, Max: 105, MaxStep: 15, StaleAge: 2 * time.Second, HasJump: true},
		ChannelRPM:     {Min: 0, Max: 20000, MaxStep: 8000, StaleAge: 250 * time.Millisecond, HasJump: true},
	}
}

// futureTolerance 允许的采样时钟领先量。
const futureTolerance = 50 * time.Millisecond

// channelState 记录每个通道最近一次被接受样本的信息。
type channelState struct {
	hasValue bool
	value    float64
	at       time.Time
	seq      int64
}

// Validator 对样本做量程、跳变与时间戳新鲜度判定。
// 所有方法均并发安全。
type Validator struct {
	mu    sync.Mutex
	specs map[Channel]ChannelSpec
	last  map[Channel]*channelState
}

// NewValidator 构造 Validator，specs 至少需包含被校验通道。
func NewValidator(specs map[Channel]ChannelSpec) *Validator {
	copied := make(map[Channel]ChannelSpec, len(specs))
	for k, v := range specs {
		copied[k] = v
	}
	return &Validator{specs: copied, last: make(map[Channel]*channelState)}
}

func reject(s Sample, reason RejectReason, format string, args ...any) Verdict {
	return Verdict{Sample: s, Accept: false, Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// Validate 判定单条样本是否可参与后续计算。
// 判定顺序固定：未知通道 -> 未来时间戳 -> 时间戳陈旧 -> 超量程 -> 序号倒退 -> 跳变，
// 保证同一输入永远得到同一结论。
func (v *Validator) Validate(s Sample, now time.Time) Verdict {
	spec, ok := v.specs[s.Channel]
	if !ok {
		return reject(s, ReasonBadChannel, "channel %q not registered", s.Channel)
	}

	age := now.Sub(s.Timestamp)
	if age < -futureTolerance {
		return reject(s, ReasonFuture, "timestamp %s is %s ahead of now %s",
			s.Timestamp.Format(time.RFC3339Nano), -age, now.Format(time.RFC3339Nano))
	}
	if age > spec.StaleAge {
		return reject(s, ReasonStale, "sample age %s exceeds stale limit %s", age, spec.StaleAge)
	}

	if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
		return reject(s, ReasonOutOfRange, "value %v is not finite", s.Value)
	}
	if s.Value < spec.Min || s.Value > spec.Max {
		return reject(s, ReasonOutOfRange, "value %.3f outside [%.3f, %.3f]", s.Value, spec.Min, spec.Max)
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	prev := v.last[s.Channel]
	if prev != nil && prev.hasValue {
		if s.Seq <= prev.seq {
			return reject(s, ReasonJump, "seq %d regressed from %d (out-of-order sample)", s.Seq, prev.seq)
		}
		if spec.HasJump {
			step := math.Abs(s.Value - prev.value)
			if step > spec.MaxStep {
				return reject(s, ReasonJump, "value jump %.3f from %.3f exceeds max step %.3f",
					step, prev.value, spec.MaxStep)
			}
		}
	}

	if prev == nil {
		prev = &channelState{}
		v.last[s.Channel] = prev
	}
	prev.hasValue = true
	prev.value = s.Value
	prev.at = s.Timestamp
	prev.seq = s.Seq
	return Verdict{Sample: s, Accept: true}
}

// Freshness 检查各注册通道相对最近一次被接受样本是否缺失或陈旧，
// 不修改内部状态。从未收到样本返回 ReasonMissing；间隔超限返回 ReasonStale。
func (v *Validator) Freshness(now time.Time) []Verdict {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]Verdict, 0, len(v.specs))
	for ch, spec := range v.specs {
		st := v.last[ch]
		s := Sample{Channel: ch, Timestamp: now}
		switch {
		case st == nil || !st.hasValue:
			out = append(out, reject(s, ReasonMissing, "no accepted sample for %q", ch))
		case now.Sub(st.at) > spec.StaleAge:
			out = append(out, reject(s, ReasonStale, "last accepted %q sample is %s old (limit %s)",
				ch, now.Sub(st.at), spec.StaleAge))
		}
	}
	return out
}

// LastAccepted 返回某通道最近一次被接受的值与时间戳。
func (v *Validator) LastAccepted(ch Channel) (value float64, at time.Time, ok bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	st := v.last[ch]
	if st == nil || !st.hasValue {
		return 0, time.Time{}, false
	}
	return st.value, st.at, true
}
