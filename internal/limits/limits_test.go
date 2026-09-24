package limits

import (
	"math"
	"testing"
	"time"

	"github.com/lisama569/battery-power-control-go/internal/sensor"
)

func healthyEst(tempC, soc, voltV float64, at time.Time) sensor.StateEstimate {
	return sensor.StateEstimate{
		At: at, Voltage: voltV, Current: 10, Temperature: tempC,
		RPM: 2000, SOC: soc, Valid: true,
	}
}

func TestLimitsNominalAndExplainable(t *testing.T) {
	at := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	e := NewEngine(150000, 320)
	d := e.Evaluate(healthyEst(30, 0.9, 380, at))
	t.Logf("[nominal] P=%.0fW T=%.1fNm binding=%s faults=%v",
		d.PowerLimitW, d.TorqueLimitNm, d.Binding.Name, d.Faults)
	for _, c := range d.Active {
		t.Logf("  constraint %-18s factor=%.2f reason=%s", c.Name, c.Factor, c.Reason)
	}
	if !d.Healthy {
		t.Fatalf("nominal condition should be healthy, faults=%v", d.Faults)
	}
	if math.Abs(d.PowerLimitW-150000) > 1e-6 {
		t.Fatalf("nominal power should be rated, got %.0f", d.PowerLimitW)
	}

	// 低温应成为最严约束，结论可解释。
	d = e.Evaluate(healthyEst(-20, 0.9, 380, at))
	t.Logf("[cold] P=%.0fW binding=%s reason=%s", d.PowerLimitW, d.Binding.Name, d.Binding.Reason)
	if d.Binding.Name != "thermal_derating" {
		t.Fatalf("expected thermal binding at -20C, got %s", d.Binding.Name)
	}
	if d.Binding.Factor != 0.55 {
		t.Fatalf("cold factor want 0.55, got %.2f", d.Binding.Factor)
	}
}

func TestLimitsMonotonic(t *testing.T) {
	at := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)

	// 温度升高，功率上限单调不增（在未触发故障锁存的区间）。
	temps := []float64{20, 30, 45, 50, 60, 70, 80, 85}
	prev := math.MaxFloat64
	for _, temp := range temps {
		e := NewEngine(150000, 320) // 每点独立引擎，避免迟滞标志互相影响
		d := e.Evaluate(healthyEst(temp, 0.9, 380, at))
		t.Logf("[mono-temp] temp=%.0f P=%.1f factor=%.2f", temp, d.PowerLimitW, d.Binding.Factor)
		if d.PowerLimitW > prev+1e-6 {
			t.Fatalf("power increased with temperature: %.0f -> %.0f", prev, d.PowerLimitW)
		}
		prev = d.PowerLimitW
	}

	// SOC 降低，功率上限单调不增。
	socs := []float64{1, 0.7, 0.5, 0.35, 0.25, 0.15, 0.05}
	prev = math.MaxFloat64
	for _, soc := range socs {
		e := NewEngine(150000, 320)
		d := e.Evaluate(healthyEst(30, soc, 380, at))
		t.Logf("[mono-soc] soc=%.2f P=%.1f factor=%.2f", soc, d.PowerLimitW, d.Binding.Factor)
		if d.PowerLimitW > prev+1e-6 {
			t.Fatalf("power increased as SOC dropped: %.0f -> %.0f", prev, d.PowerLimitW)
		}
		prev = d.PowerLimitW
	}

	// 电压降低，功率上限单调不增。
	volts := []float64{430, 380, 360, 340, 320, 290, 270}
	prev = math.MaxFloat64
	for _, vv := range volts {
		e := NewEngine(150000, 320)
		d := e.Evaluate(healthyEst(30, 0.9, vv, at))
		t.Logf("[mono-volt] volt=%.0f P=%.1f factor=%.2f", vv, d.PowerLimitW, d.Binding.Factor)
		if d.PowerLimitW > prev+1e-6 {
			t.Fatalf("power increased as voltage dropped: %.0f -> %.0f", prev, d.PowerLimitW)
		}
		prev = d.PowerLimitW
	}
}

func TestLimitsDeterministic(t *testing.T) {
	at := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	inputs := []sensor.StateEstimate{
		healthyEst(30, 0.9, 380, at),
		healthyEst(62, 0.18, 315, at),
		healthyEst(78, 0.08, 285, at),
	}
	for i, in := range inputs {
		var first Decision
		for rep := 0; rep < 5; rep++ {
			e := NewEngine(150000, 320)
			d := e.Evaluate(in)
			if rep == 0 {
				first = d
				continue
			}
			if d.PowerLimitW != first.PowerLimitW ||
				d.TorqueLimitNm != first.TorqueLimitNm ||
				d.Binding.Name != first.Binding.Name ||
				len(d.Active) != len(first.Active) {
				t.Fatalf("case %d rep %d not reproducible", i, rep)
			}
		}
		t.Logf("[deterministic] case=%d temp=%.0f soc=%.2f v=%.0f P=%.1f binding=%s",
			i, in.Temperature, in.SOC, in.Voltage, first.PowerLimitW, first.Binding.Name)
	}
}

func TestLimitsFaultHysteresis(t *testing.T) {
	at := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	e := NewEngine(150000, 320)

	d := e.Evaluate(healthyEst(90, 0.9, 380, at))
	if d.PowerLimitW != 0 || len(d.Faults) == 0 {
		t.Fatalf("over-temp should cut power to 0, got P=%.0f faults=%v", d.PowerLimitW, d.Faults)
	}
	t.Logf("[fault] temp=90 -> P=%.0f faults=%v reason=%s", d.PowerLimitW, d.Faults, d.Binding.Reason)

	// 温度降到 85（高于恢复阈值 80）：故障必须仍锁存，不能抖动恢复。
	d = e.Evaluate(healthyEst(85, 0.9, 380, at.Add(time.Second)))
	t.Logf("[hysteresis] temp=85 -> P=%.0f faults=%v", d.PowerLimitW, d.Faults)
	if d.PowerLimitW != 0 {
		t.Fatal("fault must remain latched above recovery threshold")
	}

	// 降到 79：清除故障，按降额曲线给功率。
	d = e.Evaluate(healthyEst(79, 0.9, 380, at.Add(2*time.Second)))
	t.Logf("[recover] temp=79 -> P=%.1f faults=%v binding=%s", d.PowerLimitW, d.Faults, d.Binding.Name)
	if d.PowerLimitW == 0 {
		t.Fatal("power should recover below clear threshold")
	}
	if ot, _ := e.Faults(); ot {
		t.Fatal("over-temp flag should be cleared")
	}
}

func TestLimitsInvalidInputSafeState(t *testing.T) {
	at := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	e := NewEngine(150000, 320)
	bad := sensor.StateEstimate{At: at, Valid: false, Notes: []string{"voltage:stale"}}
	d := e.Evaluate(bad)
	t.Logf("[invalid] P=%.0f healthy=%v faults=%v reason=%s",
		d.PowerLimitW, d.Healthy, d.Faults, d.Binding.Reason)
	if d.PowerLimitW != 0 || d.TorqueLimitNm != 0 || d.Healthy {
		t.Fatal("invalid estimate must yield zero-output safe state")
	}
}
