package sensor

import (
	"math"
	"testing"
	"time"
)

func feed(t *testing.T, e *Estimator, ch Channel, vals []float64, t0 time.Time, dt time.Duration) {
	t.Helper()
	for i, val := range vals {
		e.Apply(Sample{Channel: ch, Value: val, Timestamp: t0.Add(time.Duration(i) * dt), Seq: int64(i + 1)})
	}
}

func TestEstimatorSOCIntegration(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	specs := DefaultChannelSpecs()
	e := NewEstimator(80, specs)
	e.Reset(0.5)

	// 80A 放电：100 个样本形成 99 个 18s 间隔 = 1782s ≈ 0.495h，
	// SOC 下降 80*0.495/80 = 0.495。
	dt := 18 * time.Second
	curr := make([]float64, 100)
	for i := range curr {
		curr[i] = 80
	}
	feed(t, e, ChannelCurrent, curr, t0, dt)

	soc := e.SOC()
	want := 0.005
	t.Logf("[soc] after 80A/1782s discharge from 0.5: soc=%.4f expect %.3f", soc, want)
	if math.Abs(soc-want) > 1e-9 {
		t.Fatalf("soc=%v want %v", soc, want)
	}

	// 负电流（充电）应让 SOC 回升：-16A * 0.495h / 80Ah = -0.099。
	e.Reset(0.2)
	for i := range curr {
		curr[i] = -16
	}
	feed(t, e, ChannelCurrent, curr, t0, dt)
	soc = e.SOC()
	want = 0.299
	t.Logf("[soc] after -16A/1782s charge from 0.2: soc=%.4f expect %.3f", soc, want)
	if math.Abs(soc-want) > 1e-9 {
		t.Fatalf("soc=%v want %v", soc, want)
	}
}

func TestEstimatorValidity(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	e := NewEstimator(80, DefaultChannelSpecs())

	st := e.Estimate(t0)
	t.Logf("[validity] empty estimate valid=%v notes=%v", st.Valid, st.Notes)
	if st.Valid {
		t.Fatal("empty estimator must produce invalid estimate")
	}

	feed(t, e, ChannelVoltage, []float64{360}, t0, time.Millisecond)
	feed(t, e, ChannelCurrent, []float64{10}, t0, time.Millisecond)
	feed(t, e, ChannelTemp, []float64{35}, t0, time.Millisecond)
	feed(t, e, ChannelRPM, []float64{2000}, t0, time.Millisecond)
	st = e.Estimate(t0)
	t.Logf("[validity] full estimate valid=%v v=%.1f i=%.1f t=%.1f rpm=%.0f soc=%.2f",
		st.Valid, st.Voltage, st.Current, st.Temperature, st.RPM, st.SOC)
	if !st.Valid {
		t.Fatalf("all channels fresh, estimate should be valid: %v", st.Notes)
	}

	// 电压通道陈旧（其 stale 限制 250ms）。
	st = e.Estimate(t0.Add(1 * time.Second))
	t.Logf("[validity] after 1s valid=%v notes=%v", st.Valid, st.Notes)
	if st.Valid {
		t.Fatal("stale voltage must invalidate estimate")
	}
}
