package pipeline

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/lisama569/battery-power-control-go/internal/budget"
	"github.com/lisama569/battery-power-control-go/internal/sensor"
)

type feed struct {
	ch    sensor.Channel
	value float64
	dt    time.Duration // 相对基准时间
	seq   int64
}

// ingestBatch 喂入一批样本，以 batchNow 作为统一评估时钟，返回评估时钟与各样本判定。
func ingestBatch(t *testing.T, p *Pipeline, t0 time.Time, fs []feed) (time.Time, []sensor.Verdict) {
	t.Helper()
	// 取批次内最晚时间戳再推进 1ms 作为“现在”，使负偏移样本可被判定为陈旧。
	var latest time.Time
	for _, f := range fs {
		if cand := t0.Add(f.dt); cand.After(latest) {
			latest = cand
		}
	}
	batchNow := latest.Add(time.Millisecond)
	vs := make([]sensor.Verdict, 0, len(fs))
	for _, f := range fs {
		s := sensor.Sample{Channel: f.ch, Value: f.value, Timestamp: t0.Add(f.dt), Seq: f.seq}
		v := p.Ingest(s, batchNow)
		vs = append(vs, v)
		t.Logf("ingest ch=%-11s val=%8.2f accept=%v reason=%q %s",
			f.ch, f.value, v.Accept, v.Reason, v.Detail)
	}
	return batchNow, vs
}

func healthyBatch(t0 time.Time, temp, volt float64) []feed {
	return []feed{
		{sensor.ChannelVoltage, volt, 0, 1},
		{sensor.ChannelCurrent, 20, time.Millisecond, 1},
		{sensor.ChannelTemp, temp, 2 * time.Millisecond, 1},
		{sensor.ChannelRPM, 2500, 3 * time.Millisecond, 1},
	}
}

func TestPipelineRejectsBadSamples(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	p := New(DefaultConfig())

	bad := []feed{
		{sensor.ChannelVoltage, 500, 0, 1},                       // 超量程
		{sensor.ChannelVoltage, 360, -time.Second, 2},            // 陈旧
		{sensor.ChannelVoltage, 360, 0, 3},                       // 有效首帧
		{sensor.ChannelVoltage, 420, 3 * time.Millisecond, 4},    // 跳变
		{sensor.Channel("pressure"), 3, 4 * time.Millisecond, 1}, // 未知通道
	}
	_, vs := ingestBatch(t, p, t0, bad)
	want := []sensor.RejectReason{
		sensor.ReasonOutOfRange, sensor.ReasonStale, sensor.ReasonOK,
		sensor.ReasonJump,
		sensor.ReasonBadChannel,
	}
	for i, v := range vs {
		reason := v.Reason
		if v.Accept {
			reason = sensor.ReasonOK
		}
		if reason != want[i] {
			t.Fatalf("sample %d: reason=%q want %q (%s)", i, reason, want[i], v.Detail)
		}
	}

	// 只有电压首帧（bad[2]）进入估计器；其余通道缺失 → 估计无效 → 安全零输出。
	rep := p.Evaluate(nil, t0.Add(6*time.Millisecond))
	t.Logf("[partial] estimate valid=%v notes=%v P=%.0f healthy=%v",
		rep.Estimate.Valid, rep.Estimate.Notes, rep.Decision.PowerLimitW, rep.Decision.Healthy)
	if rep.Estimate.Valid || rep.Decision.PowerLimitW != 0 {
		t.Fatal("incomplete samples must yield invalid estimate and zero power")
	}
}

func TestPipelineNominalAllocation(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	p := New(DefaultConfig())
	now, _ := ingestBatch(t, p, t0, healthyBatch(t0, 30, 380))

	units := []budget.Unit{
		{ID: "drive", Priority: 1, RequestW: 120000, MinW: 40000},
		{ID: "ac", Priority: 4, RequestW: 60000, MinW: 5000},
		{ID: "pump", Priority: 2, RequestW: 20000, MinW: 5000},
	}
	rep := p.Evaluate(units, now)
	t.Logf("[nominal] P_limit=%.0f reserved=%.1f remaining=%.1f",
		rep.Decision.PowerLimitW, rep.Allocation.ReservedW, rep.Allocation.RemainingW)
	for _, g := range rep.Allocation.Granted {
		t.Logf("  grant %-6s %-8s %.1fW", g.UnitID, g.Status, g.PowerW)
	}
	if math.Abs(rep.Decision.PowerLimitW-150000) > 1e-6 {
		t.Fatalf("rated power expected, got %.0f", rep.Decision.PowerLimitW)
	}
	// drive 120000 + pump 20000 = 140000；ac 剩 10000 >= min 5000，降级到 10000。
	grants := map[string]budget.Grant{}
	for _, g := range rep.Allocation.Granted {
		grants[g.UnitID] = g
	}
	if grants["ac"].Status != budget.StatusDegraded || math.Abs(grants["ac"].PowerW-10000) > 1e-6 {
		t.Fatalf("ac should degrade to 10000, got %+v", grants["ac"])
	}
	if math.Abs(rep.Allocation.ReservedW+rep.Allocation.RemainingW-rep.Allocation.TotalBudgetW) > 1e-6 {
		t.Fatal("budget not conserved")
	}
}

func TestPipelineFaultChainAndRecovery(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	p := New(DefaultConfig())
	_, _ = ingestBatch(t, p, t0, healthyBatch(t0, 30, 380))

	units := []budget.Unit{{ID: "drive", Priority: 1, RequestW: 100000, MinW: 40000}}
	rep := p.Evaluate(units, t0.Add(5*time.Millisecond))
	if rep.Decision.PowerLimitW == 0 {
		t.Fatal("pre-fault should have power")
	}

	// 过温故障：在其余通道仍新鲜的窗口内把温度分步抬升至 92C（步长 < 15C 跳变限）。
	step := []float64{42, 54, 66, 78, 88, 92}
	base := t0.Add(10 * time.Millisecond)
	for i, temp := range step {
		at := base.Add(time.Duration(i) * 10 * time.Millisecond)
		v := p.Ingest(sensor.Sample{Channel: sensor.ChannelTemp, Value: temp, Timestamp: at, Seq: int64(i + 2)}, at)
		t.Logf("[fault-feed] temp=%.0f accept=%v reason=%q", temp, v.Accept, v.Reason)
	}
	// 在所有通道新鲜期内评估（首帧在 t0，电压 250ms 后才陈旧）。
	rep = p.Evaluate(units, base.Add(55*time.Millisecond))
	t.Logf("[fault] faults=%v P_limit=%.0f reserved=%.1f",
		rep.Decision.Faults, rep.Decision.PowerLimitW, rep.Allocation.ReservedW)
	if rep.Decision.PowerLimitW != 0 || len(rep.Decision.Faults) == 0 {
		t.Fatal("over-temp fault must cut power")
	}
	// 故障预算为 0：单元无法再获得预留。
	if len(rep.Allocation.Granted) != 0 {
		t.Fatalf("no grants allowed during zero-budget fault, got %d", len(rep.Allocation.Granted))
	}

	// 恢复：温度先降到 85（迟滞区，仍故障），再降到 40（<80 清除且脱离深降额区）。
	rec := base.Add(60 * time.Millisecond)
	tempSeq := int64(20)
	for i, temp := range []float64{85, 78, 70, 60, 50, 40} {
		at := rec.Add(time.Duration(i) * 10 * time.Millisecond)
		// 同步喂新鲜的其余通道，防止估计失效。
		p.Ingest(sensor.Sample{Channel: sensor.ChannelVoltage, Value: 380, Timestamp: at, Seq: int64(100 + i)}, at)
		p.Ingest(sensor.Sample{Channel: sensor.ChannelCurrent, Value: 20, Timestamp: at, Seq: int64(100 + i)}, at)
		p.Ingest(sensor.Sample{Channel: sensor.ChannelRPM, Value: 2500, Timestamp: at, Seq: int64(100 + i)}, at)
		v := p.Ingest(sensor.Sample{Channel: sensor.ChannelTemp, Value: temp, Timestamp: at.Add(time.Millisecond), Seq: tempSeq + int64(i)}, at.Add(time.Millisecond))
		t.Logf("[recovery-feed] temp=%.0f accept=%v", temp, v.Accept)
	}
	evalAt := rec.Add(70 * time.Millisecond)
	dec := p.Recover(evalAt)
	t.Logf("[recovery] faults=%v P=%.1f binding=%s", dec.Faults, dec.PowerLimitW, dec.Binding.Name)
	if len(dec.Faults) != 0 || dec.PowerLimitW == 0 {
		t.Fatal("fault should clear after temperature drops below hysteresis threshold")
	}
	rep = p.Evaluate(units, evalAt)
	if len(rep.Allocation.Granted) != 1 || rep.Allocation.Granted[0].PowerW != 100000 {
		t.Fatalf("post-recovery at 40C drive should be granted 100000W, got %+v", rep.Allocation.Granted)
	}
	t.Logf("[recovery] re-granted: %+v", rep.Allocation.Granted)
}

func TestPipelineConcurrentEvaluate(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	p := New(DefaultConfig())
	now, _ := ingestBatch(t, p, t0, healthyBatch(t0, 30, 380))

	units := []budget.Unit{
		{ID: "drive", Priority: 1, RequestW: 90000, MinW: 30000},
		{ID: "ac", Priority: 5, RequestW: 30000, MinW: 5000},
	}

	var wg sync.WaitGroup
	for w := 0; w < 12; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				rep := p.Evaluate(units, now)
				sum := 0.0
				for _, g := range rep.Allocation.Granted {
					sum += g.PowerW
				}
				if math.Abs(sum-rep.Allocation.ReservedW) > 1e-6 ||
					rep.Allocation.ReservedW > rep.Allocation.TotalBudgetW+1e-6 {
					t.Errorf("inconsistent allocation under concurrency")
					return
				}
			}
		}()
	}
	wg.Wait()
	final := p.Snapshot()
	t.Logf("[concurrent] final reserved=%.1f remaining=%.1f holders=%d",
		final.ReservedW, final.RemainingW, len(final.Granted))
}
