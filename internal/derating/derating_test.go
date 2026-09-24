package derating

import (
	"testing"
	"time"

	"github.com/lisama569/battery-power-control-go/internal/fault"
)

func healthyInputs() LimitInputs {
	return LimitInputs{Voltage: 370, Temp: 30, SOC: 0.8, RPM: 3000, RPMValid: true, At: time.Unix(1, 0)}
}

func noFault() fault.Status {
	return fault.Status{Active: map[fault.FaultCode]fault.ActiveFault{}}
}

func TestNominalNoDerating(t *testing.T) {
	d := NewDerater(DefaultParams())
	r := d.Compute(healthyInputs(), noFault())
	t.Logf("工况: V=370 T=30 SOC=80%% RPM=3000 => 放电上限=%.0fW 转矩=%.0fNm binding=%s 依据=%q",
		r.MaxDischargeW, r.MaxTorqueNm, r.Binding, r.ReasonText)
	if r.MaxDischargeW != 250000 || r.MaxTorqueNm != 320 {
		t.Fatalf("额定工况应满功率, got %.0fW/%.0fNm", r.MaxDischargeW, r.MaxTorqueNm)
	}
}

func TestMonotonicLimits(t *testing.T) {
	d := NewDerater(DefaultParams())

	// 温度升高 -> 上限单调不增
	t.Log("温度单调上升工况:")
	prev := 1.0
	for _, temp := range []float64{30, 45, 50, 60, 70, 85, 90} {
		in := healthyInputs()
		in.Temp = temp
		r := d.Compute(in, noFault())
		f := r.Active[ConstraintOvertemp].Factor
		t.Logf("  T=%.0f => factor=%.3f 上限=%.0fW binding=%s", temp, f, r.MaxDischargeW, r.Binding)
		if f > prev+1e-9 {
			t.Fatalf("温度系数必须单调不增: %.3f > %.3f @T=%.0f", f, prev, temp)
		}
		prev = f
	}

	// SOC 下降 -> 上限单调不增(SOC 升高不减功率)
	t.Log("SOC 单调下降工况:")
	prev = 1.0
	for _, soc := range []float64{0.9, 0.5, 0.3, 0.2, 0.15, 0.1, 0.05} {
		in := healthyInputs()
		in.SOC = soc
		r := d.Compute(in, noFault())
		f := r.Active[ConstraintLowSOC].Factor
		t.Logf("  SOC=%.0f%% => factor=%.3f 上限=%.0fW binding=%s", soc*100, f, r.MaxDischargeW, r.Binding)
		if f > prev+1e-9 {
			t.Fatalf("SOC 下降时系数必须单调不增: %.3f > %.3f @SOC=%.2f", f, prev, soc)
		}
		prev = f
	}

	// 电压下降 -> 上限单调不增
	t.Log("电压单调下降工况:")
	prev = 1.0
	for _, v := range []float64{380, 340, 320, 300, 280, 260} {
		in := healthyInputs()
		in.Voltage = v
		r := d.Compute(in, noFault())
		f := r.Active[ConstraintLowVoltage].Factor
		t.Logf("  V=%.0f => factor=%.3f 上限=%.0fW binding=%s", v, f, r.MaxDischargeW, r.Binding)
		if f > prev+1e-9 {
			t.Fatalf("电压下降时系数必须单调不增: %.3f > %.3f @V=%.0f", f, prev, v)
		}
		prev = f
	}
}

func TestBindingConstraintAndConsistency(t *testing.T) {
	d := NewDerater(DefaultParams())
	in := healthyInputs()
	in.Temp = 80     // fTemp = (85-80)/40 = 0.125
	in.SOC = 0.2     // fSOC = 0.5
	in.Voltage = 310 // fV = (310-280)/60 = 0.5
	r1 := d.Compute(in, noFault())
	r2 := d.Compute(in, noFault())
	t.Logf("工况: T=80(0.125) SOC=20%%(0.5) V=310(0.5) => binding=%s 上限=%.0fW 依据=%q",
		r1.Binding, r1.MaxDischargeW, r1.ReasonText)
	if r1.Binding != ConstraintOvertemp {
		t.Fatalf("最严格约束应为温度, got %s", r1.Binding)
	}
	if r1.MaxDischargeW != 250000*0.125 {
		t.Fatalf("上限应为额定的12.5%%, got %.1f", r1.MaxDischargeW)
	}
	// 同工况重复评估必须完全一致
	if r1.MaxDischargeW != r2.MaxDischargeW || r1.Binding != r2.Binding ||
		r1.MaxTorqueNm != r2.MaxTorqueNm || r1.MaxRegenW != r2.MaxRegenW {
		t.Fatalf("同一工况重复评估结果不一致: %+v vs %+v", r1, r2)
	}
}

func TestBindingTieBreakDeterministic(t *testing.T) {
	d := NewDerater(DefaultParams())
	in := healthyInputs()
	in.Temp = 85 // 0
	r := d.Compute(in, noFault())
	t.Logf("工况: 温度85(系数0)与其他系数1并列 => binding=%s(应按固定顺序取温度)", r.Binding)
	if r.Binding != ConstraintOvertemp || r.MaxDischargeW != 0 {
		t.Fatalf("系数为0的温度约束应 binding, got %s %.0f", r.Binding, r.MaxDischargeW)
	}
}

func TestFaultScalesAndCriticalZero(t *testing.T) {
	d := NewDerater(DefaultParams())

	warn := fault.Status{Active: map[fault.FaultCode]fault.ActiveFault{
		fault.FaultOvertemp: {Code: fault.FaultOvertemp, Severity: fault.SeverityWarning},
	}}
	r := d.Compute(healthyInputs(), warn)
	t.Logf("工况: warning 故障, 其余健康 => binding=%s 上限=%.0fW 依据=%q", r.Binding, r.MaxDischargeW, r.ReasonText)
	if r.Binding != ConstraintFault || r.MaxDischargeW != 150000 {
		t.Fatalf("warning 故障应限到60%%且binding故障, got %s %.0f", r.Binding, r.MaxDischargeW)
	}

	crit := fault.Status{Active: map[fault.FaultCode]fault.ActiveFault{
		fault.FaultSOCCutoff: {Code: fault.FaultSOCCutoff, Severity: fault.SeverityCritical},
	}}
	r = d.Compute(healthyInputs(), crit)
	t.Logf("工况: critical 锁存故障 => 上限=%.0fW 回馈=%.0fW", r.MaxDischargeW, r.MaxRegenW)
	if r.MaxDischargeW != 0 || r.MaxRegenW != 0 {
		t.Fatalf("critical 故障应把放电与回馈都限到0, got %.0f/%.0f", r.MaxDischargeW, r.MaxRegenW)
	}
}

func TestInvalidRPMConservative(t *testing.T) {
	d := NewDerater(DefaultParams())
	in := healthyInputs()
	in.RPMValid = false
	r := d.Compute(in, noFault())
	t.Logf("工况: 转速信号无效 => binding=%s 上限=%.0fW 依据=%q", r.Binding, r.MaxDischargeW, r.ReasonText)
	if r.Binding != ConstraintHighRPM || r.MaxDischargeW != 125000 {
		t.Fatalf("转速无效应保守限到50%%, got binding=%s %.0fW", r.Binding, r.MaxDischargeW)
	}
}

func TestCurveClampEndpoints(t *testing.T) {
	c := Curve{X: []float64{0, 10}, Factor: []float64{0, 1}}
	if v := c.clamp(-5); v != 0 {
		t.Fatalf("低于端点应取端点系数0, got %v", v)
	}
	if v := c.clamp(15); v != 1 {
		t.Fatalf("高于端点应取端点系数1, got %v", v)
	}
	if v := c.clamp(5); v != 0.5 {
		t.Fatalf("中点应插值0.5, got %v", v)
	}
	t.Log("工况: 分段线性曲线端点钳位与中点插值正确")
}
