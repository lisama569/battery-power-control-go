package pipeline

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/lisama569/battery-power-control-go/internal/budget"
	"github.com/lisama569/battery-power-control-go/internal/sensor"
)

var boostUnits = []budget.Unit{
	{ID: "drive", Priority: 1, RequestW: 120000, MinW: 40000},
	{ID: "pump", Priority: 2, RequestW: 20000, MinW: 5000},
	{ID: "ac", Priority: 5, RequestW: 40000, MinW: 5000},
}

// feedAll 以递增序号喂入一组四路新鲜样本（温度可变，其余通道保持健康值）。
func feedAll(t *testing.T, p *Pipeline, seq *int64, temp float64, at time.Time) {
	t.Helper()
	*seq++
	p.Ingest(sensor.Sample{Channel: sensor.ChannelVoltage, Value: 380, Timestamp: at, Seq: *seq}, at)
	p.Ingest(sensor.Sample{Channel: sensor.ChannelCurrent, Value: 20, Timestamp: at, Seq: *seq}, at)
	p.Ingest(sensor.Sample{Channel: sensor.ChannelRPM, Value: 2500, Timestamp: at, Seq: *seq}, at)
	v := p.Ingest(sensor.Sample{Channel: sensor.ChannelTemp, Value: temp, Timestamp: at, Seq: *seq}, at)
	t.Logf("[feed] temp=%.1f seq=%d accept=%v reason=%q", temp, *seq, v.Accept, v.Reason)
}

func logReport(t *testing.T, tag string, rep Report) {
	t.Helper()
	t.Logf("[%s] limit=%.0f reserved=%.1f remaining=%.1f expiredBoosts=%v activeBoosts=%d",
		tag, rep.Decision.PowerLimitW, rep.Allocation.ReservedW,
		rep.Allocation.RemainingW, rep.ExpiredBoosts, len(rep.Allocation.ActiveBoosts))
	for _, g := range rep.Allocation.Granted {
		t.Logf("  %-6s %-8s %9.1fW  %s", g.UnitID, g.Status, g.PowerW, g.Reason)
	}
}

func checkPipelineConservation(t *testing.T, tag string, a budget.Allocation) {
	t.Helper()
	sum := 0.0
	for _, g := range a.Granted {
		sum += g.PowerW
	}
	if math.Abs(sum-a.ReservedW) > 1e-6 {
		t.Fatalf("%s: reserved %.1f != sum of grants %.1f", tag, a.ReservedW, sum)
	}
	if a.ReservedW > a.TotalBudgetW+1e-6 || a.RemainingW < -1e-6 {
		t.Fatalf("%s: invariant broken: total=%.1f reserved=%.1f remaining=%.1f",
			tag, a.TotalBudgetW, a.ReservedW, a.RemainingW)
	}
}

func grantOf(a budget.Allocation, id string) (budget.Grant, bool) {
	for _, g := range a.Granted {
		if g.UnitID == id {
			return g, true
		}
	}
	return budget.Grant{}, false
}

// 短时增援全链路：抢占 → 限功率中途收紧按优先级回收 → 窗口到期归还。
func TestPipelineBoostLifecycle(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	p := New(DefaultConfig())
	now0, _ := ingestBatch(t, p, t0, healthyBatch(t0, 30, 380))

	// 基线：150kW 满额预算，drive 120k / pump 20k / ac 降级 10k。
	rep := p.Evaluate(boostUnits, now0)
	logReport(t, "baseline", rep)
	checkPipelineConservation(t, "baseline", rep.Allocation)
	if g, _ := grantOf(rep.Allocation, "ac"); g.Status != budget.StatusDegraded || g.PowerW != 10000 {
		t.Fatalf("baseline ac should degrade to 10000, got %+v", g)
	}

	// drive 发起超车增援：+20kW，临时优先级 0，窗口 200ms。
	g := p.RequestBoost(budget.Unit{ID: "drive", Priority: 1, RequestW: 120000, MinW: 40000},
		budget.BoostSpec{ExtraW: 20000, Priority: 0, TTL: 200 * time.Millisecond}, now0)
	t.Logf("[boost-start] drive status=%s power=%.1f reason=%s", g.Status, g.PowerW, g.Reason)
	if g.Status != budget.StatusGranted || g.PowerW != 140000 {
		t.Fatalf("boosted drive should hold 140kW, got %s %.1f", g.Status, g.PowerW)
	}
	snap := p.Snapshot()
	logReport(t, "during-boost", Report{Decision: rep.Decision, Allocation: snap})
	// 总额 150k：drive 140k 足额，pump 被挤到 10k，ac 被挤空。
	if g, _ := grantOf(snap, "pump"); g.Status != budget.StatusDegraded || g.PowerW != 10000 {
		t.Fatalf("pump should be squeezed to 10kW, got %+v", g)
	}
	if _, ok := grantOf(snap, "ac"); ok {
		t.Fatal("ac should be fully preempted during boost")
	}
	if len(snap.ActiveBoosts) != 1 || snap.ActiveBoosts[0].UnitID != "drive" {
		t.Fatalf("expected active boost for drive, got %+v", snap.ActiveBoosts)
	}
	checkPipelineConservation(t, "during-boost", snap)

	// 窗口未结束，温度升至 60C：限功率压到 150k*0.70=105k。
	seq := int64(1)
	for i, temp := range []float64{42, 54, 60} {
		feedAll(t, p, &seq, temp, now0.Add(time.Duration(10+i*10)*time.Millisecond))
	}
	rep = p.Evaluate(boostUnits, now0.Add(40*time.Millisecond))
	logReport(t, "tighten-60C", rep)
	if math.Abs(rep.Decision.PowerLimitW-105000) > 1e-6 {
		t.Fatalf("60C should derate to 105kW, got %.0f", rep.Decision.PowerLimitW)
	}
	// 按有效优先级从低到高回收：ac、pump 归零，增援中的 drive 降级到 105k。
	if g, ok := grantOf(rep.Allocation, "drive"); !ok || g.PowerW != 105000 {
		t.Fatalf("boosted drive should degrade to 105kW, got %+v (ok=%v)", g, ok)
	}
	if _, ok := grantOf(rep.Allocation, "pump"); ok {
		t.Fatal("pump should be reclaimed before the boosted unit")
	}
	checkPipelineConservation(t, "tighten", rep.Allocation)

	// 温度回落到 30C，且评估时钟越过增援窗口：到期归还，恢复基线。
	for i, temp := range []float64{45, 30} {
		feedAll(t, p, &seq, temp, now0.Add(time.Duration(150+i*10)*time.Millisecond))
	}
	rep = p.Evaluate(boostUnits, now0.Add(210*time.Millisecond))
	logReport(t, "expired", rep)
	if len(rep.ExpiredBoosts) != 1 || rep.ExpiredBoosts[0] != "drive" {
		t.Fatalf("drive boost should expire in this round, got %v", rep.ExpiredBoosts)
	}
	if len(rep.Allocation.ActiveBoosts) != 0 {
		t.Fatalf("no boost may remain after expiry, got %+v", rep.Allocation.ActiveBoosts)
	}
	d, _ := grantOf(rep.Allocation, "drive")
	pu, _ := grantOf(rep.Allocation, "pump")
	ac, _ := grantOf(rep.Allocation, "ac")
	if d.PowerW != 120000 || pu.PowerW != 20000 || ac.PowerW != 10000 {
		t.Fatalf("after expiry should return to baseline, got drive=%.0f pump=%.0f ac=%.0f",
			d.PowerW, pu.PowerW, ac.PowerW)
	}
	checkPipelineConservation(t, "expired", rep.Allocation)
	t.Logf("[expired] allocation restored to pre-boost baseline, boost left no residue")
}

// 增援进行中遭遇过温故障：预算归零不卡死、无残留占用；恢复后额度干净可用。
func TestPipelineBoostFaultRecovery(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	p := New(DefaultConfig())
	now0, _ := ingestBatch(t, p, t0, healthyBatch(t0, 30, 380))
	rep := p.Evaluate(boostUnits, now0)
	logReport(t, "baseline", rep)

	// 增援窗口 100ms。
	p.RequestBoost(budget.Unit{ID: "drive", Priority: 1, RequestW: 120000, MinW: 40000},
		budget.BoostSpec{ExtraW: 20000, Priority: 0, TTL: 100 * time.Millisecond}, now0)

	// 过温故障：分步抬温到 92C，预算被打到 0。
	seq := int64(1)
	for i, temp := range []float64{42, 54, 66, 78, 88, 92} {
		feedAll(t, p, &seq, temp, now0.Add(time.Duration(10+i*10)*time.Millisecond))
	}
	rep = p.Evaluate(boostUnits, now0.Add(70*time.Millisecond))
	logReport(t, "fault", rep)
	if rep.Decision.PowerLimitW != 0 || len(rep.Decision.Faults) == 0 {
		t.Fatal("over-temp fault must cut power to zero")
	}
	if rep.Allocation.ReservedW != 0 || len(rep.Allocation.Granted) != 0 || rep.Allocation.RemainingW != 0 {
		t.Fatalf("zero budget must reclaim everything, got %+v", rep.Allocation)
	}

	// 恢复：温度经迟滞区回落到 40C；评估时钟已越过增援窗口（now0+100ms）。
	for i, temp := range []float64{85, 78, 70, 60, 50, 40} {
		feedAll(t, p, &seq, temp, now0.Add(time.Duration(80+i*10)*time.Millisecond))
	}
	dec := p.Recover(now0.Add(140 * time.Millisecond))
	t.Logf("[recover] faults=%v limit=%.0f", dec.Faults, dec.PowerLimitW)
	if len(dec.Faults) != 0 || dec.PowerLimitW == 0 {
		t.Fatal("fault should clear after cooldown")
	}
	rep = p.Evaluate(boostUnits, now0.Add(150*time.Millisecond))
	logReport(t, "recovered", rep)
	if len(rep.ExpiredBoosts) != 1 || rep.ExpiredBoosts[0] != "drive" {
		t.Fatalf("expired boost must be cleaned up, got %v", rep.ExpiredBoosts)
	}
	if len(rep.Allocation.ActiveBoosts) != 0 {
		t.Fatalf("no boost residue allowed after fault+expiry, got %+v", rep.Allocation.ActiveBoosts)
	}
	d, _ := grantOf(rep.Allocation, "drive")
	pu, _ := grantOf(rep.Allocation, "pump")
	ac, _ := grantOf(rep.Allocation, "ac")
	if d.PowerW != 120000 || pu.PowerW != 20000 || ac.PowerW != 10000 {
		t.Fatalf("recovery should restore baseline cleanly, got drive=%.0f pump=%.0f ac=%.0f",
			d.PowerW, pu.PowerW, ac.PowerW)
	}
	checkPipelineConservation(t, "recovered", rep.Allocation)
}

// 并发申请/增援/释放/评估：竞态检测下不变量始终成立。
func TestPipelineConcurrentBoost(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	p := New(DefaultConfig())
	now0, _ := ingestBatch(t, p, t0, healthyBatch(t0, 30, 380))

	check := func(tag string, a budget.Allocation) {
		sum := 0.0
		for _, g := range a.Granted {
			sum += g.PowerW
		}
		if math.Abs(sum-a.ReservedW) > 1e-6 ||
			a.ReservedW > a.TotalBudgetW+1e-6 || a.RemainingW < -1e-6 {
			t.Errorf("%s: inconsistent allocation: %+v", tag, a)
		}
	}

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			id := string(rune('a' + w))
			for i := 0; i < 100; i++ {
				switch i % 4 {
				case 0:
					rep := p.Evaluate(boostUnits, now0)
					check("evaluate", rep.Allocation)
				case 1:
					p.RequestBoost(budget.Unit{ID: id, Priority: w + 2, RequestW: 5000, MinW: 1000},
						budget.BoostSpec{ExtraW: 3000, Priority: 0, TTL: time.Minute}, now0)
				case 2:
					p.Request(budget.Unit{ID: id, Priority: w + 2, RequestW: 4000, MinW: 1000})
				default:
					p.StopBoost(id, now0)
					p.Release(id)
				}
				check("snapshot", p.Snapshot())
			}
		}(w)
	}
	wg.Wait()
	final := p.Snapshot()
	t.Logf("[concurrent-boost] final reserved=%.1f remaining=%.1f holders=%d boosts=%d",
		final.ReservedW, final.RemainingW, len(final.Granted), len(final.ActiveBoosts))
	checkPipelineConservation(t, "final", final)
}
