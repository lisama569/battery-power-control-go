package pipeline

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/lisama569/battery-power-control-go/internal/budget"
	"github.com/lisama569/battery-power-control-go/internal/sensor"
)

// feedAll 在 at 时刻喂入四路健康样本（温度可调），seq 全局递增保证序号合法。
func feedAll(t *testing.T, p *Pipeline, seq *int64, at time.Time, temp float64) {
	t.Helper()
	for i, s := range []sensor.Sample{
		{Channel: sensor.ChannelVoltage, Value: 380, Timestamp: at, Seq: *seq},
		{Channel: sensor.ChannelCurrent, Value: 20, Timestamp: at.Add(time.Millisecond), Seq: *seq + 1},
		{Channel: sensor.ChannelTemp, Value: temp, Timestamp: at.Add(2 * time.Millisecond), Seq: *seq + 2},
		{Channel: sensor.ChannelRPM, Value: 2500, Timestamp: at.Add(3 * time.Millisecond), Seq: *seq + 3},
	} {
		v := p.Ingest(s, at.Add(4*time.Millisecond))
		if !v.Accept {
			t.Fatalf("sample %d (temp=%.0f) rejected: %q %s", i, temp, v.Reason, v.Detail)
		}
	}
	*seq += 4
	t.Logf("[feed] at=+%dms temp=%.0fC accepted", at.Sub(boostBase)/time.Millisecond, temp)
}

var boostBase = time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)

func boostUnits() []budget.Unit {
	return []budget.Unit{
		{ID: "drive", Priority: 1, RequestW: 120000, MinW: 40000},
		{ID: "pump", Priority: 2, RequestW: 20000, MinW: 5000},
		{ID: "ac", Priority: 4, RequestW: 60000, MinW: 5000},
	}
}

func grantsByID(a budget.Allocation) map[string]budget.Grant {
	out := map[string]budget.Grant{}
	for _, g := range a.Granted {
		out[g.UnitID] = g
	}
	return out
}

func checkPipelineConservation(t *testing.T, a budget.Allocation, stage string) {
	t.Helper()
	sum := 0.0
	for _, g := range a.Granted {
		sum += g.PowerW
	}
	if math.Abs(sum-a.ReservedW) > 1e-6 ||
		math.Abs(a.ReservedW+a.RemainingW-a.TotalBudgetW) > 1e-6 ||
		a.RemainingW < 0 || a.ReservedW > a.TotalBudgetW+1e-6 {
		t.Fatalf("[%s] conservation violated: %+v", stage, a)
	}
}

// 端到端：短时增援的抢占 → 限功率中途收紧按优先级回收 → 故障归零无残留 →
// 恢复后干净可用 → 窗口到期归还，整体如同增援未发生。
func TestPipelineBoostLifecycle(t *testing.T) {
	t0 := boostBase
	p := New(DefaultConfig())
	seq := int64(1)
	units := boostUnits()

	// 1) 标称工况：drive 120000 + pump 20000 足额，ac 降级到 10000。
	feedAll(t, p, &seq, t0, 30)
	rep := p.Evaluate(units, t0.Add(4*time.Millisecond))
	t.Logf("[nominal] P_limit=%.0f reserved=%.1f remaining=%.1f",
		rep.Decision.PowerLimitW, rep.Allocation.ReservedW, rep.Allocation.RemainingW)
	for _, g := range rep.Allocation.Granted {
		t.Logf("  grant %-8s %-8s %.1fW (%s)", g.UnitID, g.Status, g.PowerW, g.Reason)
	}
	nominal := grantsByID(rep.Allocation)
	if math.Abs(rep.Decision.PowerLimitW-150000) > 1e-6 {
		t.Fatalf("rated power expected, got %.0f", rep.Decision.PowerLimitW)
	}
	checkPipelineConservation(t, rep.Allocation, "nominal")

	// 2) 超车增援：40000W / 窗口 1.5s / 临时优先级 0。
	// 可用为 0，按优先级从低到高挤占：ac(10000) 与 pump(20000) 整笔让出，drive 让出 10000。
	tb := t0.Add(10 * time.Millisecond)
	bg := p.RequestBoost(budget.BoostSpec{
		Unit:     budget.Unit{ID: "overtake", Priority: 1, RequestW: 40000, MinW: 10000},
		Priority: 0,
		TTL:      1500 * time.Millisecond,
	}, tb)
	t.Logf("[boost] status=%s power=%.1f reason=%s", bg.Status, bg.PowerW, bg.Reason)
	if bg.Status != budget.StatusBoosted || bg.PowerW != 40000 {
		t.Fatalf("boost should be granted 40000W, got %s %.1f", bg.Status, bg.PowerW)
	}
	snap := p.Snapshot()
	during := grantsByID(snap)
	if _, ok := during["ac"]; ok {
		t.Fatal("ac must be fully preempted")
	}
	if _, ok := during["pump"]; ok {
		t.Fatal("pump must be fully preempted")
	}
	if math.Abs(during["drive"].PowerW-110000) > 1e-6 {
		t.Fatalf("drive should be preempted to 110000W, got %.1f", during["drive"].PowerW)
	}
	if len(snap.Boosts) != 1 || snap.Boosts[0].UnitID != "overtake" {
		t.Fatalf("expected active boost, got %+v", snap.Boosts)
	}
	t.Logf("[boost-active] drive=%.1f overtake=%.1f reserved=%.1f remaining=%.1f",
		during["drive"].PowerW, during["overtake"].PowerW, snap.ReservedW, snap.RemainingW)
	checkPipelineConservation(t, snap, "boost-active")

	// 3) 窗口内温度恶化到 60C：上限压到 105000，按优先级从低到高回收 45000（drive 110000→65000，增援不动）。
	feedAll(t, p, &seq, tb.Add(100*time.Millisecond), 45)
	feedAll(t, p, &seq, tb.Add(110*time.Millisecond), 60)
	rep = p.Evaluate(nil, tb.Add(120*time.Millisecond))
	t.Logf("[tighten] P_limit=%.0f binding=%s reserved=%.1f remaining=%.1f",
		rep.Decision.PowerLimitW, rep.Decision.Binding.Name, rep.Allocation.ReservedW, rep.Allocation.RemainingW)
	tight := grantsByID(rep.Allocation)
	if math.Abs(rep.Decision.PowerLimitW-105000) > 1e-6 {
		t.Fatalf("60C should derate to 105000W, got %.0f", rep.Decision.PowerLimitW)
	}
	if math.Abs(tight["drive"].PowerW-65000) > 1e-6 {
		t.Fatalf("reclaim must hit lowest priority first: drive want 65000 got %.1f", tight["drive"].PowerW)
	}
	if tight["overtake"].Status != budget.StatusBoosted || math.Abs(tight["overtake"].PowerW-40000) > 1e-6 {
		t.Fatalf("boost must be reclaimed last (untouched), got %+v", tight["overtake"])
	}
	checkPipelineConservation(t, rep.Allocation, "tighten")

	// 4) 过温故障：预算归零，全部预留（含增援）收清，无残留占用。
	feedAll(t, p, &seq, tb.Add(200*time.Millisecond), 75)
	feedAll(t, p, &seq, tb.Add(210*time.Millisecond), 88)
	feedAll(t, p, &seq, tb.Add(220*time.Millisecond), 92)
	rep = p.Evaluate(nil, tb.Add(230*time.Millisecond))
	t.Logf("[fault] faults=%v P_limit=%.0f reserved=%.1f holders=%d boosts=%d",
		rep.Decision.Faults, rep.Decision.PowerLimitW, rep.Allocation.ReservedW,
		len(rep.Allocation.Granted), len(rep.Allocation.Boosts))
	if rep.Decision.PowerLimitW != 0 || rep.Allocation.ReservedW != 0 || len(rep.Allocation.Granted) != 0 {
		t.Fatalf("fault must cut budget to zero with no residual holds, got %+v", rep.Allocation)
	}
	checkPipelineConservation(t, rep.Allocation, "fault")

	// 5) 恢复：温度回到 30C（步长 <= 15C 跳变限），故障清除，预算干净可用。
	feedAll(t, p, &seq, tb.Add(300*time.Millisecond), 78)
	feedAll(t, p, &seq, tb.Add(310*time.Millisecond), 64)
	feedAll(t, p, &seq, tb.Add(320*time.Millisecond), 50)
	feedAll(t, p, &seq, tb.Add(330*time.Millisecond), 36)
	feedAll(t, p, &seq, tb.Add(340*time.Millisecond), 30)
	dec := p.Recover(tb.Add(350 * time.Millisecond))
	t.Logf("[recover] faults=%v P_limit=%.0f", dec.Faults, dec.PowerLimitW)
	if len(dec.Faults) != 0 || math.Abs(dec.PowerLimitW-150000) > 1e-6 {
		t.Fatalf("recovery should restore rated power, got faults=%v P=%.0f", dec.Faults, dec.PowerLimitW)
	}

	// 6) 增援窗口到期：被挤占单元按原申请归还，分配结果与增援前完全一致。
	feedAll(t, p, &seq, tb.Add(1400*time.Millisecond), 30)
	rep = p.Evaluate(units, tb.Add(1500*time.Millisecond))
	t.Logf("[expired] reserved=%.1f remaining=%.1f boosts=%d",
		rep.Allocation.ReservedW, rep.Allocation.RemainingW, len(rep.Allocation.Boosts))
	for _, g := range rep.Allocation.Granted {
		t.Logf("  grant %-8s %-8s %.1fW (%s)", g.UnitID, g.Status, g.PowerW, g.Reason)
	}
	if len(rep.Allocation.Boosts) != 0 {
		t.Fatalf("boost must be expired, got %+v", rep.Allocation.Boosts)
	}
	if _, ok := grantsByID(rep.Allocation)["overtake"]; ok {
		t.Fatal("boost reservation must not survive expiry")
	}
	restored := grantsByID(rep.Allocation)
	for id, want := range nominal {
		got, ok := restored[id]
		if !ok || got.Status != want.Status || math.Abs(got.PowerW-want.PowerW) > 1e-6 {
			t.Fatalf("unit %s not restored to pre-boost state: want %+v got %+v", id, want, got)
		}
	}
	if math.Abs(rep.Allocation.ReservedW-150000) > 1e-6 || rep.Allocation.RemainingW != 0 {
		t.Fatalf("post-expiry allocation must equal nominal, got %+v", rep.Allocation)
	}
	checkPipelineConservation(t, rep.Allocation, "expired")
}

// 并发混合操作：增援/申请/释放/评估同时打满管线，竞态安全且每份报告自洽。
func TestPipelineConcurrentBoost(t *testing.T) {
	t0 := boostBase
	p := New(DefaultConfig())
	seq := int64(1)
	feedAll(t, p, &seq, t0, 30)
	now := t0.Add(4 * time.Millisecond)
	units := boostUnits()

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			id := string(rune('x' + w))
			for i := 0; i < 100; i++ {
				switch i % 4 {
				case 0:
					p.RequestBoost(budget.BoostSpec{
						Unit:     budget.Unit{ID: id, Priority: 5 + w, RequestW: 20000, MinW: 2000},
						Priority: w,
						TTL:      time.Minute,
					}, now)
				case 1:
					p.Request(budget.Unit{ID: id, Priority: 5 + w, RequestW: 10000, MinW: 1000})
				case 2:
					p.Release(id)
				default:
					rep := p.Evaluate(units, now)
					checkPipelineConservation(t, rep.Allocation, "concurrent")
				}
			}
		}(w)
	}
	wg.Wait()
	final := p.Snapshot()
	t.Logf("[concurrent-boost] reserved=%.1f remaining=%.1f holders=%d boosts=%d",
		final.ReservedW, final.RemainingW, len(final.Granted), len(final.Boosts))
	checkPipelineConservation(t, final, "final")
}
