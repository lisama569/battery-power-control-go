// Package derating 依据电池与热状态推导单调、可解释的功率与转矩上限。
//
// 每条约束(温度/SOC/电压/转速/故障)独立给出 [0,1] 的可用系数，
// 最终系数取所有约束的最小值(binding constraint)。同一输入必然得到同一
// 输出；约束并列时按固定顺序 tie-break，结论不抖动。
//
// 系数曲线为分段线性函数：
//   - 温度曲线随温度单调不增；
//   - SOC 曲线随 SOC 单调不减；
//   - 电压曲线随电压单调不减；
//
// 因此上限随温度下降、随 SOC/电压上升，满足单调性要求。
package derating

import (
	"fmt"
	"sort"
	"time"

	"github.com/lisama569/battery-power-control-go/internal/fault"
)

// Constraint 标识一条限功率约束来源。
type Constraint string

const (
	ConstraintFault      Constraint = "fault"
	ConstraintOvertemp   Constraint = "trace_temp"
	ConstraintLowSOC     Constraint = "low_soc"
	ConstraintLowVoltage Constraint = "low_voltage"
	ConstraintHighRPM    Constraint = "high_rpm"
)

// constraintOrder 是约束并列(系数相同)时的确定性 tie-break 顺序。
var constraintOrder = []Constraint{
	ConstraintFault,
	ConstraintOvertemp,
	ConstraintLowSOC,
	ConstraintLowVoltage,
	ConstraintHighRPM,
}

// Contribution 单条约束给出的限幅与依据。
type Contribution struct {
	PowerLimitW   float64
	TorqueLimitNm float64
	Factor        float64
	InputValue    float64
	Threshold     float64
	Explanation   string
}

// LimitResult 是一次限幅评估的结果。
type LimitResult struct {
	MaxDischargeW float64
	MaxTorqueNm   float64
	MaxRegenW     float64
	Active        map[Constraint]Contribution
	Binding       Constraint
	ReasonText    string
	At            time.Time
}

// Curve 是一维分段线性曲线。X 必须严格单调递增；Factor 按要求单调。
type Curve struct {
	X      []float64
	Factor []float64
}

// clamp 在曲线上线性插值；超出端点取端点系数。
func (c Curve) clamp(x float64) float64 {
	if len(c.X) == 0 {
		return 1
	}
	if x <= c.X[0] {
		return c.Factor[0]
	}
	last := len(c.X) - 1
	if x >= c.X[last] {
		return c.Factor[last]
	}
	i := sort.Search(last, func(i int) bool { return c.X[i] >= x })
	// x 落在 (X[i-1], X[i])
	x0, x1 := c.X[i-1], c.X[i]
	f0, f1 := c.Factor[i-1], c.Factor[i]
	if x1 == x0 {
		return f0
	}
	return f0 + (f1-f0)*(x-x0)/(x1-x0)
}

// Params 限幅参数。
type Params struct {
	RatedPowerW     float64
	RatedTorqueNm   float64
	RatedRegenW     float64
	TempDerate      Curve // 随温度单调不增
	SocDerate       Curve // 随 SOC 单调不减
	VoltageDerate   Curve // 随电压单调不减
	RPMLimit        float64
	RPMLimitPower   float64
	FaultPowerScale map[fault.Severity]float64
}

// LimitInputs 是限幅所需的解耦输入。
type LimitInputs struct {
	Voltage  float64
	Temp     float64
	SOC      float64
	RPM      float64
	RPMValid bool
	At       time.Time
}

// Derater 依据状态计算功率/转矩上限。
type Derater struct {
	p Params
}

// DefaultParams 返回默认限幅参数。
func DefaultParams() Params {
	return Params{
		RatedPowerW:   250000,
		RatedTorqueNm: 320,
		RatedRegenW:   120000,
		// 温度：45C 以下满功率；45..85C 线性降到 0；>=85C 为 0。
		TempDerate: Curve{
			X:      []float64{45, 85},
			Factor: []float64{1, 0},
		},
		// SOC：10% 及以下不允许放电；10..30% 线性恢复；30% 以上满功率。
		SocDerate: Curve{
			X:      []float64{0.10, 0.30},
			Factor: []float64{0, 1},
		},
		// 电压：280V 及以下 0；280..340V 线性恢复；340V 以上满功率。
		VoltageDerate: Curve{
			X:      []float64{280, 340},
			Factor: []float64{0, 1},
		},
		RPMLimit:      16000,
		RPMLimitPower: 0.5, // RPM 无效或超速时的保守系数
	}
}

// NewDerater 创建限幅器。
func NewDerater(p Params) *Derater {
	if p.FaultPowerScale == nil {
		p.FaultPowerScale = map[fault.Severity]float64{
			fault.SeverityWarning:  0.6,
			fault.SeverityCritical: 0,
		}
	}
	return &Derater{p: p}
}

// RatedPower 返回额定放电功率。
func (d *Derater) RatedPower() float64 { return d.p.RatedPowerW }

// Compute 在给定输入与故障状态下计算限幅。纯函数：相同输入永远得到相同结果。
func (d *Derater) Compute(in LimitInputs, fs fault.Status) LimitResult {
	at := in.At
	active := make(map[Constraint]Contribution, len(constraintOrder))
	contrib := func(c Constraint, factor float64, value, threshold float64, explain string) {
		factor = clamp01(factor)
		active[c] = Contribution{
			PowerLimitW:   d.p.RatedPowerW * factor,
			TorqueLimitNm: d.p.RatedTorqueNm * factor,
			Factor:        factor,
			InputValue:    value,
			Threshold:     threshold,
			Explanation:   explain,
		}
	}

	// 温度
	fTemp := d.p.TempDerate.clamp(in.Temp)
	contrib(ConstraintOvertemp, fTemp, in.Temp, d.p.TempDerate.X[0],
		fmt.Sprintf("温度 %.1fC -> 温度降额系数 %.2f%%", in.Temp, fTemp*100))

	// SOC
	fSOC := d.p.SocDerate.clamp(in.SOC)
	contrib(ConstraintLowSOC, fSOC, in.SOC, d.p.SocDerate.X[len(d.p.SocDerate.X)-1],
		fmt.Sprintf("SOC %.1f%% -> SOC 降额系数 %.2f%%", in.SOC*100, fSOC*100))

	// 电压
	fV := d.p.VoltageDerate.clamp(in.Voltage)
	contrib(ConstraintLowVoltage, fV, in.Voltage, d.p.VoltageDerate.X[len(d.p.VoltageDerate.X)-1],
		fmt.Sprintf("电压 %.1fV -> 电压降额系数 %.2f%%", in.Voltage, fV*100))

	// 转速：超速按固定保守系数；转速信号无效同样保守处理(不静默信任)
	fRPM := 1.0
	rpmExplain := fmt.Sprintf("转速 %.0frpm <= %.0f, 不限速", in.RPM, d.p.RPMLimit)
	if !in.RPMValid {
		fRPM = d.p.RPMLimitPower
		rpmExplain = fmt.Sprintf("转速信号无效, 保守限速系数 %.2f%%", fRPM*100)
	} else if in.RPM >= d.p.RPMLimit {
		fRPM = d.p.RPMLimitPower
		rpmExplain = fmt.Sprintf("转速 %.0frpm >= %.0f, 高速限速系数 %.2f%%", in.RPM, d.p.RPMLimit, fRPM*100)
	}
	contrib(ConstraintHighRPM, fRPM, in.RPM, d.p.RPMLimit, rpmExplain)

	// 故障：按最高严重度取系数(锁存的 critical 也包含在内)
	fFault := 1.0
	faultExplain := "无激活故障, 不降额"
	if sev := fs.WorstSeverity(); sev != "" {
		if scale, ok := d.p.FaultPowerScale[sev]; ok {
			fFault = scale
		}
		codes := fs.Codes()
		faultExplain = fmt.Sprintf("激活故障 %v, 最高严重度=%s, 故障系数 %.2f%%", codes, sev, fFault*100)
	}
	contrib(ConstraintFault, fFault, 0, 0, faultExplain)

	// binding = 最小系数；并列时按 constraintOrder 取靠前者(更保守的约束优先解释)。
	// 无激活故障时故障约束系数为 1 但不参与 binding 提名，避免“无故障却是故障约束”的误导。
	binding := Constraint("")
	best := 2.0
	for _, c := range constraintOrder {
		if c == ConstraintFault && len(fs.Active) == 0 {
			continue
		}
		if f := active[c].Factor; f < best {
			best = f
			binding = c
		}
	}
	if binding == "" {
		// 全部系数为 1 且无故障：温度约束作为“额满”的解释来源。
		binding = ConstraintOvertemp
		best = 1
	}
	bc := active[binding]
	res := LimitResult{
		MaxDischargeW: bc.PowerLimitW,
		MaxTorqueNm:   bc.TorqueLimitNm,
		// 回馈上限同样受最严格系数约束，防止故障/过温时大回馈。
		MaxRegenW:  d.p.RatedRegenW * best,
		Active:     active,
		Binding:    binding,
		ReasonText: bc.Explanation,
		At:         at,
	}
	return res
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
