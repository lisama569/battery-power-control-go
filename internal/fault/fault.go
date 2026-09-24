// Package fault 依据电池状态判定故障并维护带确认时间与迟滞的故障状态机。
//
// 为避免阈值附近的反复抖动，故障采用“确认窗口 + 恢复窗口 + 迟滞阈值”：
//   - 触发条件必须连续保持 ConfirmWindow 才置位；
//   - 清除条件必须连续保持 ClearWindow 才解除；
//   - 触发阈值与恢复阈值不同(迟滞)。
//
// 严重故障(soc_cutoff)为锁存故障，一旦置位只能通过显式 Reset 解除，
// 限幅器可据此把放电功率限制为零。
package fault

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/lisama569/battery-power-control-go/internal/battery"
)

// FaultCode 故障类型。
type FaultCode string

const (
	FaultSensorTimeout FaultCode = "sensor_timeout"
	FaultUndervoltage  FaultCode = "undervoltage"
	FaultOvervoltage   FaultCode = "overvoltage"
	FaultOvercurrent   FaultCode = "overcurrent"
	FaultOvertemp      FaultCode = "overtemp"
	FaultLowSOC        FaultCode = "low_soc"
	FaultSOCCutoff     FaultCode = "soc_cutoff"
)

// Severity 故障严重级别。
type Severity string

const (
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Inputs 是故障判定所需的解耦输入。
type Inputs struct {
	Voltage      float64
	Current      float64
	Temp         float64
	SOC          float64
	VoltageValid bool
	CurrentValid bool
	TempValid    bool
	RPMValid     bool
	At           time.Time
}

// ActiveFault 描述一条激活的故障及其判定依据。
type ActiveFault struct {
	Code     FaultCode
	Severity Severity
	Value    float64
	Limit    float64
	Since    time.Time
	Message  string
}

// Thresholds 故障阈值与迟滞、确认时间。
type Thresholds struct {
	MinVoltage     float64
	MaxVoltage     float64
	MaxCurrent     float64
	OvertempWarn   float64
	OvertempTrip   float64
	TempRecover    float64
	LowSOCWarn     float64
	LowSOCRecharge float64
	SOCCutoff      float64
	SOCRecharge    float64
	ConfirmWindow  time.Duration
	ClearWindow    time.Duration
}

// Status 是故障状态机的当前状态(可安全按值复制)。
type Status struct {
	Active        map[FaultCode]ActiveFault
	LatchedFaults map[FaultCode]ActiveFault // 锁存故障：只能显式 Reset 清除
	At            time.Time
}

// Detector 维护带迟滞与确认时间的故障状态机，可并发使用。
type Detector struct {
	mu      sync.Mutex
	t       Thresholds
	now     func() time.Time
	active  map[FaultCode]*track
	latched map[FaultCode]ActiveFault
}

type track struct {
	active     bool
	since      time.Time // 当前激活起点
	tripSince  time.Time // 连续满足触发条件的起点
	clearSince time.Time // 连续满足恢复条件的起点
	critSince  time.Time // 过温 critical 连续满足起点
	warnSince  time.Time // 过温 warning 连续满足起点
	last       ActiveFault
}

// DefaultThresholds 返回默认阈值。
func DefaultThresholds() Thresholds {
	return Thresholds{
		MinVoltage:     280,
		MaxVoltage:     405,
		MaxCurrent:     350,
		OvertempWarn:   55,
		OvertempTrip:   70,
		TempRecover:    48,
		LowSOCWarn:     0.30,
		LowSOCRecharge: 0.35,
		SOCCutoff:      0.10,
		SOCRecharge:    0.15,
		ConfirmWindow:  100 * time.Millisecond,
		ClearWindow:    200 * time.Millisecond,
	}
}

// NewDetector 创建故障检测器。
func NewDetector(t Thresholds, now func() time.Time) *Detector {
	if now == nil {
		now = time.Now
	}
	return &Detector{
		t:       t,
		now:     now,
		active:  map[FaultCode]*track{},
		latched: map[FaultCode]ActiveFault{},
	}
}

// FromBatteryState 将电池状态转换为故障判定输入。
func FromBatteryState(s battery.State, now time.Time) Inputs {
	return Inputs{
		Voltage:      s.Voltage,
		Current:      s.Current,
		Temp:         s.Temp,
		SOC:          s.SOC,
		VoltageValid: s.VoltageValid,
		CurrentValid: s.CurrentValid,
		TempValid:    s.TempValid,
		RPMValid:     s.RPMValid,
		At:           now,
	}
}

type faultRule struct {
	code     FaultCode
	severity Severity
	latched  bool
	// trip 为真表示越过触发阈值；recover 为真表示回到恢复阈值内。
	trip    func(in Inputs, t Thresholds) bool
	recover func(in Inputs, t Thresholds) bool
	value   func(in Inputs) float64
	limit   func(t Thresholds) float64
	message string
}

func rules() []faultRule {
	return []faultRule{
		{
			code: FaultSensorTimeout, severity: SeverityCritical,
			trip:    func(in Inputs, _ Thresholds) bool { return !in.VoltageValid || !in.CurrentValid || !in.TempValid },
			recover: func(in Inputs, _ Thresholds) bool { return in.VoltageValid && in.CurrentValid && in.TempValid },
			value:   func(in Inputs) float64 { return 0 },
			limit:   func(_ Thresholds) float64 { return 0 },
			message: "关键传感器(电压/电流/温度)读数缺失或陈旧",
		},
		{
			code: FaultUndervoltage, severity: SeverityCritical,
			trip:    func(in Inputs, t Thresholds) bool { return in.VoltageValid && in.Voltage < t.MinVoltage },
			recover: func(in Inputs, t Thresholds) bool { return in.VoltageValid && in.Voltage >= t.MinVoltage+10 },
			value:   func(in Inputs) float64 { return in.Voltage },
			limit:   func(t Thresholds) float64 { return t.MinVoltage },
			message: "母线欠压",
		},
		{
			code: FaultOvervoltage, severity: SeverityCritical,
			trip:    func(in Inputs, t Thresholds) bool { return in.VoltageValid && in.Voltage > t.MaxVoltage },
			recover: func(in Inputs, t Thresholds) bool { return in.VoltageValid && in.Voltage <= t.MaxVoltage-10 },
			value:   func(in Inputs) float64 { return in.Voltage },
			limit:   func(t Thresholds) float64 { return t.MaxVoltage },
			message: "母线过压",
		},
		{
			code: FaultOvercurrent, severity: SeverityCritical,
			trip:    func(in Inputs, t Thresholds) bool { return in.CurrentValid && in.Current > t.MaxCurrent },
			recover: func(in Inputs, t Thresholds) bool { return in.CurrentValid && in.Current <= t.MaxCurrent-20 },
			value:   func(in Inputs) float64 { return in.Current },
			limit:   func(t Thresholds) float64 { return t.MaxCurrent },
			message: "放电过流",
		},
		{
			// 单一规则：>=OvertempWarn 触发 warning，>=OvertempTrip 连续升级为 critical；
			// 回落到 OvertempTrip 以下即降回 warning，回落到 TempRecover 以下才清除。
			code: FaultOvertemp, severity: SeverityWarning,
			trip:    func(in Inputs, t Thresholds) bool { return in.TempValid && in.Temp >= t.OvertempWarn },
			recover: func(in Inputs, t Thresholds) bool { return in.TempValid && in.Temp <= t.TempRecover },
			value:   func(in Inputs) float64 { return in.Temp },
			limit:   func(t Thresholds) float64 { return t.OvertempWarn },
			message: "电池过温降额",
		},
		{
			code: FaultLowSOC, severity: SeverityWarning,
			trip:    func(in Inputs, t Thresholds) bool { return in.SOC <= t.LowSOCWarn },
			recover: func(in Inputs, t Thresholds) bool { return in.SOC >= t.LowSOCRecharge },
			value:   func(in Inputs) float64 { return in.SOC },
			limit:   func(t Thresholds) float64 { return t.LowSOCWarn },
			message: "低电量降额",
		},
		{
			code: FaultSOCCutoff, severity: SeverityCritical, latched: true,
			trip:    func(in Inputs, t Thresholds) bool { return in.SOC <= t.SOCCutoff },
			recover: func(in Inputs, t Thresholds) bool { return in.SOC >= t.SOCRecharge },
			value:   func(in Inputs) float64 { return in.SOC },
			limit:   func(t Thresholds) float64 { return t.SOCCutoff },
			message: "电量到达切断阈值，禁止放电(锁存)",
		},
	}
}

// Evaluate 依据输入推进状态机并返回当前故障状态。
func (d *Detector) Evaluate(in Inputs) Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	at := in.At
	if at.IsZero() {
		at = d.now()
	}

	for _, r := range rules() {
		if r.code == FaultOvertemp {
			d.updateOvertemp(in, at)
			continue
		}
		tr := d.trackFor(r.code)
		tripped := r.trip(in, d.t)
		recovered := r.recover(in, d.t)

		if tripped {
			if tr.tripSince.IsZero() {
				tr.tripSince = at
			}
			if at.Sub(tr.tripSince) >= d.t.ConfirmWindow {
				if !tr.active {
					tr.since = tr.tripSince
					tr.active = true
				}
				tr.last = ActiveFault{
					Code: r.code, Severity: r.severity,
					Value: r.value(in), Limit: r.limit(d.t),
					Since: tr.since, Message: r.message,
				}
				if r.latched {
					d.latched[r.code] = tr.last
				}
				tr.clearSince = time.Time{}
			}
		} else {
			tr.tripSince = time.Time{}
		}

		if !r.latched && tr.active && recovered {
			if tr.clearSince.IsZero() {
				tr.clearSince = at
			}
			if at.Sub(tr.clearSince) >= d.t.ClearWindow {
				tr.active = false
				tr.since = time.Time{}
				tr.last = ActiveFault{}
			}
		} else if !recovered {
			tr.clearSince = time.Time{}
		}
	}

	return d.snapshotLocked(at)
}

// updateOvertemp 实现过温的两级(warn/critical)独立确认与迟滞状态机。
//
//   - T >= OvertempTrip 持续 ConfirmWindow        -> critical
//   - OvertempWarn <= T < OvertempTrip 持续确认   -> warning
//   - critical 回落到 OvertempTrip 以下: 立即降为 warning(不清除，不抖动)
//   - T <= TempRecover 持续 ClearWindow           -> 完全清除
func (d *Detector) updateOvertemp(in Inputs, at time.Time) {
	tr := d.trackFor(FaultOvertemp)
	if !in.TempValid {
		// 温度无效时不改变过温状态(sensor_timeout 规则另行处理)。
		return
	}

	warn := func(value float64) ActiveFault {
		return ActiveFault{
			Code: FaultOvertemp, Severity: SeverityWarning,
			Value: value, Limit: d.t.OvertempWarn, Since: tr.since,
			Message: fmt.Sprintf("电池过温降额: %.1fC >= %.1fC", value, d.t.OvertempWarn),
		}
	}

	switch {
	case in.Temp >= d.t.OvertempTrip:
		// Zone C: critical 区间，独立确认；确认中仍保持 warning。
		tr.clearSince = time.Time{}
		tr.warnSince = time.Time{}
		if tr.critSince.IsZero() {
			tr.critSince = at
		}
		if at.Sub(tr.critSince) >= d.t.ConfirmWindow {
			if !tr.active {
				tr.since = tr.critSince
				tr.active = true
			}
			tr.last = ActiveFault{
				Code: FaultOvertemp, Severity: SeverityCritical,
				Value: in.Temp, Limit: d.t.OvertempTrip, Since: tr.since,
				Message: fmt.Sprintf("电池严重过温: %.1fC >= %.1fC", in.Temp, d.t.OvertempTrip),
			}
		}

	case in.Temp > d.t.TempRecover:
		// Zone B: recover < T < trip。critical 立即降级为 warning；
		// warn 阈值以上按 warning 确认，warn 与 recover 之间为迟滞维持带。
		tr.critSince = time.Time{}
		tr.clearSince = time.Time{}
		if tr.active && tr.last.Severity == SeverityCritical {
			// critical 回落到 trip 以下：立即降为 warning，不清除、不抖动。
			tr.last = warn(in.Temp)
			tr.warnSince = at
			break
		}
		if in.Temp >= d.t.OvertempWarn {
			if tr.warnSince.IsZero() {
				tr.warnSince = at
			}
			if at.Sub(tr.warnSince) >= d.t.ConfirmWindow {
				if !tr.active {
					tr.since = tr.warnSince
					tr.active = true
				}
				tr.last = warn(in.Temp)
			}
		} else {
			// 迟滞带(TempRecover, OvertempWarn)：维持现状。
			tr.warnSince = time.Time{}
		}

	default:
		// Zone A: T <= recover，清除计时，到期完全解除。
		tr.critSince = time.Time{}
		tr.warnSince = time.Time{}
		if tr.clearSince.IsZero() {
			tr.clearSince = at
		}
		if tr.active && at.Sub(tr.clearSince) >= d.t.ClearWindow {
			tr.active = false
			tr.since = time.Time{}
			tr.last = ActiveFault{}
			tr.clearSince = time.Time{}
		}
	}

	// 激活期间持续刷新当前测量值，使日志反映真实工况(阈值与严重度不变)。
	if tr.active {
		tr.last.Value = in.Temp
	}
}

// Reset 清除锁存故障(如充电恢复后由上层显式调用)。
func (d *Detector) Reset(codes ...FaultCode) Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(codes) == 0 {
		d.latched = map[FaultCode]ActiveFault{}
	} else {
		for _, c := range codes {
			delete(d.latched, c)
			if tr, ok := d.active[c]; ok {
				tr.active = false
				tr.since = time.Time{}
				tr.last = ActiveFault{}
				tr.tripSince = time.Time{}
				tr.clearSince = time.Time{}
				tr.critSince = time.Time{}
				tr.warnSince = time.Time{}
			}
		}
	}
	return d.snapshotLocked(d.now())
}

// Snapshot 返回当前故障状态。
func (d *Detector) Snapshot() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snapshotLocked(d.now())
}

func (d *Detector) trackFor(code FaultCode) *track {
	tr, ok := d.active[code]
	if !ok {
		tr = &track{}
		d.active[code] = tr
	}
	return tr
}

func (d *Detector) snapshotLocked(at time.Time) Status {
	out := Status{
		Active:        map[FaultCode]ActiveFault{},
		LatchedFaults: map[FaultCode]ActiveFault{},
		At:            at,
	}
	for code, tr := range d.active {
		if tr.active {
			out.Active[code] = tr.last
		}
	}
	for code, f := range d.latched {
		out.LatchedFaults[code] = f
		// 锁存故障同时体现在 Active 中，保证限幅器看到它。
		out.Active[code] = f
	}
	return out
}

// Codes 返回确定性排序的激活故障代码(日志/测试用)。
func (s Status) Codes() []FaultCode {
	codes := make([]FaultCode, 0, len(s.Active))
	for c := range s.Active {
		codes = append(codes, c)
	}
	sort.Slice(codes, func(i, j int) bool { return codes[i] < codes[j] })
	return codes
}

// Has 报告指定故障是否激活。
func (s Status) Has(code FaultCode) bool {
	_, ok := s.Active[code]
	return ok
}

// WorstSeverity 返回当前激活故障中的最高严重度；无故障返回空串。
func (s Status) WorstSeverity() Severity {
	w := Severity("")
	for _, f := range s.Active {
		if f.Severity == SeverityCritical {
			return SeverityCritical
		}
		w = SeverityWarning
	}
	return w
}
