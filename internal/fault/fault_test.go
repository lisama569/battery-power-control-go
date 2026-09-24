package fault

import (
	"testing"
	"time"
)

func baseInputs(t time.Time) Inputs {
	return Inputs{
		Voltage: 360, Current: 100, Temp: 30, SOC: 0.8,
		VoltageValid: true, CurrentValid: true, TempValid: true, RPMValid: true,
		At: t,
	}
}

func TestConfirmWindowPreventsChattering(t *testing.T) {
	t0 := time.Unix(1000, 0)
	d := NewDetector(DefaultThresholds(), nil)

	in := baseInputs(t0)
	in.Temp = 80 // >= OvertempTrip=70
	s := d.Evaluate(in)
	t.Logf("工况: 温度80 刚出现 => 激活故障=%v (确认窗口未满, 应为空)", s.Codes())
	if len(s.Active) != 0 {
		t.Fatalf("确认窗口内不应激活故障, got %v", s.Codes())
	}

	in.At = t0.Add(50 * time.Millisecond)
	s = d.Evaluate(in)
	if s.Has(FaultOvertemp) {
		t.Fatalf("50ms < ConfirmWindow=100ms 不应激活")
	}

	in.At = t0.Add(110 * time.Millisecond)
	s = d.Evaluate(in)
	f, ok := s.Active[FaultOvertemp]
	t.Logf("工况: 温度80 持续110ms => %s severity=%s 依据 value=%.1f>=limit=%.1f",
		f.Code, f.Severity, f.Value, f.Limit)
	if !ok || f.Severity != SeverityCritical {
		t.Fatalf("持续过确认窗口应激活 critical 过温, got %+v", s.Active)
	}
}

func TestHysteresisRecovery(t *testing.T) {
	t0 := time.Unix(2000, 0)
	d := NewDetector(DefaultThresholds(), nil)
	in := baseInputs(t0)
	in.Temp = 60 // warn: 60>=55, recover: <=48
	d.Evaluate(in)
	in.At = t0.Add(110 * time.Millisecond)
	s := d.Evaluate(in)
	if !s.Has(FaultOvertemp) || s.Active[FaultOvertemp].Severity != SeverityWarning {
		t.Fatalf("应激活 warning 过温, got %+v", s.Active)
	}
	t.Logf("工况: 60C 持续 => warning 激活 (依据 %.1f>=55)", s.Active[FaultOvertemp].Value)

	// 温度回落到迟滞带内(50, >48)：故障应保持，不抖动清除
	in.At = t0.Add(300 * time.Millisecond)
	in.Temp = 50
	s = d.Evaluate(in)
	t.Logf("工况: 回落到50C(迟滞带 48<50<55) => 保持=%v", s.Has(FaultOvertemp))
	if !s.Has(FaultOvertemp) {
		t.Fatalf("迟滞带内不应清除故障")
	}

	// 回到恢复阈值以下，但未满 ClearWindow
	in.At = t0.Add(400 * time.Millisecond)
	in.Temp = 45
	s = d.Evaluate(in)
	if !s.Has(FaultOvertemp) {
		t.Fatalf("恢复窗口未满不应清除")
	}
	// 满足恢复条件持续 ClearWindow 后清除
	in.At = t0.Add(650 * time.Millisecond)
	s = d.Evaluate(in)
	t.Logf("工况: 45C 持续恢复200ms+ => 激活=%v (应清除)", s.Codes())
	if s.Has(FaultOvertemp) {
		t.Fatalf("迟滞+恢复窗口后应清除, got %v", s.Codes())
	}
}

func TestSeverityEscalationAndDowngrade(t *testing.T) {
	t0 := time.Unix(3000, 0)
	d := NewDetector(DefaultThresholds(), nil)
	in := baseInputs(t0)
	in.Temp = 60
	d.Evaluate(in)
	in.At = t0.Add(110 * time.Millisecond)
	s := d.Evaluate(in)
	if s.Active[FaultOvertemp].Severity != SeverityWarning {
		t.Fatalf("应为 warning")
	}
	// 升级：>=trip 后需独立满足确认窗口
	in.At = t0.Add(150 * time.Millisecond)
	in.Temp = 75
	s = d.Evaluate(in)
	if s.Active[FaultOvertemp].Severity != SeverityWarning {
		t.Fatalf("critical 确认窗口未满应仍为 warning")
	}
	in.At = t0.Add(260 * time.Millisecond)
	s = d.Evaluate(in)
	t.Logf("工况: 75C 持续确认110ms => severity=%s (应 critical)", s.Active[FaultOvertemp].Severity)
	if s.Active[FaultOvertemp].Severity != SeverityCritical {
		t.Fatalf("应升级 critical")
	}
	// 降回 warn 区间：严重度立即降为 warning，但不清除
	in.At = t0.Add(300 * time.Millisecond)
	in.Temp = 58
	s = d.Evaluate(in)
	t.Logf("工况: 75C->58C => severity=%s (应 warning, 仍激活)", s.Active[FaultOvertemp].Severity)
	if s.Active[FaultOvertemp].Severity != SeverityWarning {
		t.Fatalf("应降为 warning")
	}
	// 再次升到 trip 以上需重新确认，不得立即跳 critical
	in.At = t0.Add(340 * time.Millisecond)
	in.Temp = 75
	s = d.Evaluate(in)
	if s.Active[FaultOvertemp].Severity != SeverityWarning {
		t.Fatalf("迟滞带内回升不应立即跳 critical")
	}
}

func TestSensorTimeout(t *testing.T) {
	t0 := time.Unix(4000, 0)
	d := NewDetector(DefaultThresholds(), nil)
	in := baseInputs(t0)
	in.TempValid = false
	d.Evaluate(in)
	in.At = t0.Add(110 * time.Millisecond)
	s := d.Evaluate(in)
	if !s.Has(FaultSensorTimeout) {
		t.Fatalf("温度无效持续应触发 sensor_timeout")
	}
	t.Logf("工况: 温度读数无效持续110ms => %s %q", FaultSensorTimeout, s.Active[FaultSensorTimeout].Message)
}

func TestLatchedSOCCutoffRequiresReset(t *testing.T) {
	t0 := time.Unix(5000, 0)
	d := NewDetector(DefaultThresholds(), nil)
	in := baseInputs(t0)
	in.SOC = 0.05
	d.Evaluate(in)
	in.At = t0.Add(110 * time.Millisecond)
	s := d.Evaluate(in)
	if !s.Has(FaultSOCCutoff) {
		t.Fatalf("SOC 低于切断阈值应触发锁存故障")
	}
	t.Logf("工况: SOC=%.2f<=0.10 => 锁存故障激活, 依据: %s", in.SOC, s.Active[FaultSOCCutoff].Message)

	// 即使 SOC 回升超过恢复阈值(模拟充电)，锁存故障仍不自动清除
	in.At = t0.Add(400 * time.Millisecond)
	in.SOC = 0.5
	s = d.Evaluate(in)
	if !s.Has(FaultSOCCutoff) {
		t.Fatalf("锁存故障不得自动清除")
	}
	t.Logf("工况: SOC 回升到0.50 => 锁存仍=%v (需显式复位)", s.Has(FaultSOCCutoff))

	s = d.Reset(FaultSOCCutoff)
	if s.Has(FaultSOCCutoff) {
		t.Fatalf("显式复位后应清除")
	}
}

func TestDeterministicRepeatedEvaluation(t *testing.T) {
	t0 := time.Unix(6000, 0)
	in := baseInputs(t0)
	d1 := NewDetector(DefaultThresholds(), nil)
	d2 := NewDetector(DefaultThresholds(), nil)
	for i := 1; i <= 5; i++ {
		in.At = t0.Add(time.Duration(i*30) * time.Millisecond)
		s1 := d1.Evaluate(in)
		s2 := d2.Evaluate(in)
		if len(s1.Active) != len(s2.Active) {
			t.Fatalf("相同工况重复评估结果不一致: %v vs %v", s1.Codes(), s2.Codes())
		}
	}
	s := d1.Snapshot()
	t.Logf("工况: 同一输入序列两次评估 => 激活集合一致 %v", s.Codes())
}
