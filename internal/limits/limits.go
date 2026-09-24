// Package limits 依据电池与热状态推导功率与转矩上限。
//
// 所有上限都表示为额定值的比例（0..1），规则为分段线性的纯函数：
// 温度越高、SOC 越低、电压越低，允许比例只降不升（单调）。
// 最终结论由全部约束取最小值得到，因此同一工况重复评估结果完全一致。
package limits

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/lisama569/battery-power-control-go/internal/sensor"
)

// knot 是降额折线上的一个点：输入 x 对应允许比例 factor。
type knot struct {
	x      float64
	factor float64
}

// interp 在折线上做分段线性插值；x 超出区间时钳位到端点比例。
// knots 必须按 x 升序排列。
func interp(knots []knot, x float64) float64 {
	if x <= knots[0].x {
		return knots[0].factor
	}
	last := knots[len(knots)-1]
	if x >= last.x {
		return last.factor
	}
	for i := 1; i < len(knots); i++ {
		if x <= knots[i].x {
			a, b := knots[i-1], knots[i]
			t := (x - a.x) / (b.x - a.x)
			return a.factor + t*(b.factor-a.factor)
		}
	}
	return last.factor
}

// 温度折线（摄氏度）：温度升高 → 比例下降。
var tempKnots = []knot{
	{-20, 0.55},
	{0, 0.85},
	{20, 1.00},
	{45, 1.00},
	{60, 0.70},
	{75, 0.30},
	{85, 0.05},
}

// SOC 折线（0..1）：SOC 下降 → 比例下降。
var socKnots = []knot{
	{0.05, 0.05},
	{0.10, 0.25},
	{0.20, 0.55},
	{0.30, 0.80},
	{0.50, 1.00},
	{1.00, 1.00},
}

// 电压折线（V）：电压下降 → 比例下降。
var voltKnots = []knot{
	{270, 0.05},
	{300, 0.30},
	{320, 0.60},
	{350, 0.90},
	{370, 1.00},
	{430, 1.00},
}

// 故障阈值（含恢复迟滞）。
const (
	faultTempCutoffC  = 90.0
	faultVoltCutoffV  = 260.0
	recoverTempClearC = 80.0
	recoverVoltClearV = 300.0
)

// Constraint 是一条可解释的限功率约束。
type Constraint struct {
	Name     string
	Source   string
	Factor   float64
	PowerW   float64
	TorqueNm float64
	Reason   string
}

// Decision 是某一工况下的限功率结论。
type Decision struct {
	At            time.Time
	PowerLimitW   float64
	TorqueLimitNm float64
	Active        []Constraint
	Binding       Constraint
	Faults        []string
	Healthy       bool
	Input         sensor.StateEstimate
}

// Engine 是限功率推导引擎。
// Evaluate 的计算部分是纯函数；Engine 仅持故障迟滞标志并加锁，保证并发安全与结论确定。
type Engine struct {
	mu            sync.Mutex
	ratedPowerW   float64
	ratedTorqueNm float64
	tempFault     bool
	voltFault     bool
}

// NewEngine 构造限功率引擎。
func NewEngine(ratedPowerW, ratedTorqueNm float64) *Engine {
	return &Engine{ratedPowerW: ratedPowerW, ratedTorqueNm: ratedTorqueNm}
}

func scale(v, factor float64) float64 {
	if factor < 0 {
		factor = 0
	}
	return v * factor
}

// Evaluate 依据状态估计给出限功率结论。
// 相同输入 + 相同内部故障标志 ⇒ 完全相同输出。
func (e *Engine) Evaluate(st sensor.StateEstimate) Decision {
	e.mu.Lock()
	defer e.mu.Unlock()

	d := Decision{At: st.At, Input: st, Healthy: true, Active: []Constraint{}}

	// 输入估计无效：进入安全零输出态，但不改动故障标志，避免无效数据造成状态抖动。
	if !st.Valid {
		d.PowerLimitW = 0
		d.TorqueLimitNm = 0
		d.Healthy = false
		d.Faults = append(d.Faults, "invalid_state_estimate")
		c := Constraint{
			Name: "invalid_input", Source: "estimator", Factor: 0,
			Reason: fmt.Sprintf("state estimate invalid: %v", st.Notes),
		}
		d.Binding = c
		d.Active = append(d.Active, c)
		return d
	}

	// 故障迟滞：置位用严格阈值，清除用更宽松阈值，防止边界抖动。
	if !e.tempFault && st.Temperature >= faultTempCutoffC {
		e.tempFault = true
	}
	if e.tempFault && st.Temperature < recoverTempClearC {
		e.tempFault = false
	}
	if !e.voltFault && st.Voltage <= faultVoltCutoffV {
		e.voltFault = true
	}
	if e.voltFault && st.Voltage > recoverVoltClearV {
		e.voltFault = false
	}

	tempF := interp(tempKnots, st.Temperature)
	socF := interp(socKnots, st.SOC)
	voltF := interp(voltKnots, st.Voltage)

	d.Active = append(d.Active, Constraint{
		Name: "thermal_derating", Source: "temperature", Factor: tempF,
		PowerW: scale(e.ratedPowerW, tempF), TorqueNm: scale(e.ratedTorqueNm, tempF),
		Reason: fmt.Sprintf("temp=%.1fC -> factor %.2f", st.Temperature, tempF),
	})
	d.Active = append(d.Active, Constraint{
		Name: "soc_derating", Source: "soc", Factor: socF,
		PowerW: scale(e.ratedPowerW, socF), TorqueNm: scale(e.ratedTorqueNm, socF),
		Reason: fmt.Sprintf("soc=%.3f -> factor %.2f", st.SOC, socF),
	})
	d.Active = append(d.Active, Constraint{
		Name: "voltage_derating", Source: "voltage", Factor: voltF,
		PowerW: scale(e.ratedPowerW, voltF), TorqueNm: scale(e.ratedTorqueNm, voltF),
		Reason: fmt.Sprintf("voltage=%.1fV -> factor %.2f", st.Voltage, voltF),
	})

	if e.tempFault {
		d.Faults = append(d.Faults, "over_temperature")
		d.Active = append(d.Active, Constraint{
			Name: "over_temperature_cutoff", Source: "fault", Factor: 0,
			Reason: fmt.Sprintf("temp=%.1fC >= %.1fC cutoff (clears below %.1fC)",
				st.Temperature, faultTempCutoffC, recoverTempClearC),
		})
	}
	if e.voltFault {
		d.Faults = append(d.Faults, "under_voltage")
		d.Active = append(d.Active, Constraint{
			Name: "under_voltage_cutoff", Source: "fault", Factor: 0,
			Reason: fmt.Sprintf("voltage=%.1fV <= %.1fV cutoff (clears above %.1fV)",
				st.Voltage, faultVoltCutoffV, recoverVoltClearV),
		})
	}

	// 确定性排序：factor 升序，相同 factor 按名称升序，避免结论顺序抖动。
	sort.SliceStable(d.Active, func(i, j int) bool {
		if d.Active[i].Factor != d.Active[j].Factor {
			return d.Active[i].Factor < d.Active[j].Factor
		}
		return d.Active[i].Name < d.Active[j].Name
	})
	d.Binding = d.Active[0]
	d.PowerLimitW = d.Binding.PowerW
	d.TorqueLimitNm = d.Binding.TorqueNm
	d.Healthy = len(d.Faults) == 0 && d.Binding.Factor >= 0.999999
	return d
}

// Faults 返回当前锁存故障标志快照。
func (e *Engine) Faults() (overTemp, underVolt bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tempFault, e.voltFault
}
