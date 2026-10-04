package budget

import (
	"math"
	"sync"
	"testing"
	"time"
)

var boostT0 = time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)

// boostUnits 是增援测试共用的三个用电单元：
// drive 50W（高优先级）、pump 30W（中）、ac 40W（低，预算内只能降级到 20W）。
func boostUnits() []Unit {
	return []Unit{
		{ID: "drive", Priority: 1, RequestW: 50, MinW: 40},
		{ID: "pump", Priority: 3, RequestW: 30, MinW: 5},
		{ID: "ac", Priority: 5, RequestW: 40, MinW: 10},
	}
}

func grantMap(a Allocation) map[string]Grant {
	m := map[string]Grant{}
	for _, g := range a.Granted {
		m[g.UnitID] = g
	}
	return m
}

func logAlloc(t *testing.T, tag string, a Allocation) {
	t.Helper()
	t.Logf("[%s] total=%.1f reserved=%.1f remaining=%.1f activeBoosts=%d",
		tag, a.TotalBudgetW, a.ReservedW, a.RemainingW, len(a.ActiveBoosts))
	for _, g := range a.Granted {
		t.Logf("  %-6s %-8s %6.1fW  %s", g.UnitID, g.Status, g.PowerW, g.Reason)
	}
	for _, b := range a.ActiveBoosts {
		t.Logf("  boost %-6s +%.1fW prio=%d until %s", b.UnitID, b.ExtraW, b.Priority,
			b.ExpiresAt.Format("15:04:05.000"))
	}
}

// newBoostScene 构造 100W 预算并完成基线分配：drive 50 / pump 30 / ac 降级 20。
func newBoostScene(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(100)
	a := m.Allocate(boostUnits())
	logAlloc(t, "baseline", a)
	checkConservation(t, a)
	g := grantMap(a)
	if g["drive"].PowerW != 50 || g["pump"].PowerW != 30 ||
		g["ac"].Status != StatusDegraded || g["ac"].PowerW != 20 {
		t.Fatalf("unexpected baseline: %+v", a.Granted)
	}
	return m
}

func TestBoostPreemptsLowerPriority(t *testing.T) {
	m := newBoostScene(t)

	// drive 发起超车增援：+40W，临时优先级 0（最高），窗口 5 分钟。
	g := m.StartBoost(
		Unit{ID: "drive", Priority: 1, RequestW: 50, MinW: 40},
		BoostSpec{ExtraW: 40, Priority: 0, TTL: 5 * time.Minute}, boostT0)
	t.Logf("[boost-start] drive status=%s power=%.1f reason=%s", g.Status, g.PowerW, g.Reason)
	if g.Status != StatusGranted || g.PowerW != 90 {
		t.Fatalf("boosted drive should hold 50+40=90W, got %s %.1f", g.Status, g.PowerW)
	}

	a := m.Snapshot()
	logAlloc(t, "during-boost", a)
	got := grantMap(a)
	// 挤占顺序从低到高：ac 先被收光（20->0 被拒），pump 再被挤到 10。
	if _, ok := got["ac"]; ok {
		t.Fatalf("ac should be fully preempted, got %+v", got["ac"])
	}
	if got["pump"].Status != StatusDegraded || got["pump"].PowerW != 10 {
		t.Fatalf("pump should be squeezed to 10W, got %+v", got["pump"])
	}
	if len(a.ActiveBoosts) != 1 || a.ActiveBoosts[0].UnitID != "drive" {
		t.Fatalf("expected one active boost for drive, got %+v", a.ActiveBoosts)
	}
	checkConservation(t, a)
	if a.RemainingW < 0 {
		t.Fatalf("remaining must never go negative, got %.1f", a.RemainingW)
	}
}

func TestBoostExpiryReturnsAsIfNeverHappened(t *testing.T) {
	m := newBoostScene(t)
	before := m.Snapshot()

	m.StartBoost(Unit{ID: "drive", Priority: 1, RequestW: 50, MinW: 40},
		BoostSpec{ExtraW: 40, Priority: 0, TTL: 5 * time.Minute}, boostT0)

	// 窗口内时间推进但未到期：不应到期。
	if expired := m.ExpireBoosts(boostT0.Add(4 * time.Minute)); len(expired) != 0 {
		t.Fatalf("no boost should expire before TTL, got %v", expired)
	}
	// 窗口结束：到期归还。
	expired := m.ExpireBoosts(boostT0.Add(5 * time.Minute))
	t.Logf("[boost-expire] expired=%v", expired)
	if len(expired) != 1 || expired[0] != "drive" {
		t.Fatalf("drive boost should expire, got %v", expired)
	}

	after := m.Snapshot()
	logAlloc(t, "after-expiry", after)
	checkConservation(t, after)
	if len(after.ActiveBoosts) != 0 {
		t.Fatalf("no boost should remain, got %+v", after.ActiveBoosts)
	}
	// 与增援前逐笔一致：整体如同增援没发生过。
	if len(before.Granted) != len(after.Granted) {
		t.Fatalf("grant count changed: before=%d after=%d", len(before.Granted), len(after.Granted))
	}
	for i := range before.Granted {
		b, a := before.Granted[i], after.Granted[i]
		if b.UnitID != a.UnitID || b.Status != a.Status || math.Abs(b.PowerW-a.PowerW) > 1e-9 {
			t.Fatalf("grant %d differs: before=%+v after=%+v", i, b, a)
		}
	}
	t.Logf("[boost-expire] allocation identical to pre-boost baseline")
}

func TestBoostWindowBudgetTighten(t *testing.T) {
	m := newBoostScene(t)
	m.StartBoost(Unit{ID: "drive", Priority: 1, RequestW: 50, MinW: 40},
		BoostSpec{ExtraW: 40, Priority: 0, TTL: 5 * time.Minute}, boostT0)

	// 窗口未结束，温度/电压变化把限功率上限压到 60W。
	a := m.ApplyBudget(60, boostT0.Add(time.Minute))
	logAlloc(t, "tighten-60", a)
	got := grantMap(a)
	// 按有效优先级从低到高回收：ac、pump 先归零，drive 降级到 60。
	if got["drive"].Status != StatusDegraded || got["drive"].PowerW != 60 {
		t.Fatalf("boosted drive should degrade to 60W, got %+v", got["drive"])
	}
	if _, ok := got["pump"]; ok {
		t.Fatalf("pump should be reclaimed first, got %+v", got["pump"])
	}
	checkConservation(t, a)
	if a.RemainingW < 0 || a.ReservedW > a.TotalBudgetW {
		t.Fatalf("invariant broken: %+v", a)
	}

	// 上限回升到 100W：增援仍在窗口内，drive 恢复 90W。
	a = m.ApplyBudget(100, boostT0.Add(2*time.Minute))
	logAlloc(t, "relax-100", a)
	if grantMap(a)["drive"].PowerW != 90 {
		t.Fatalf("boost should re-apply while window open, got %+v", grantMap(a)["drive"])
	}
	checkConservation(t, a)

	// 窗口结束后归还，回到基线。
	m.ExpireBoosts(boostT0.Add(5 * time.Minute))
	a = m.Snapshot()
	logAlloc(t, "expired-after-tighten", a)
	got = grantMap(a)
	if got["drive"].PowerW != 50 || got["pump"].PowerW != 30 || got["ac"].PowerW != 20 {
		t.Fatalf("after expiry should return to baseline, got %+v", a.Granted)
	}
	checkConservation(t, a)
}

func TestBoostFaultZeroAndRecovery(t *testing.T) {
	m := newBoostScene(t)
	m.StartBoost(Unit{ID: "drive", Priority: 1, RequestW: 50, MinW: 40},
		BoostSpec{ExtraW: 40, Priority: 0, TTL: 5 * time.Minute}, boostT0)

	// 故障把总预算打到 0：所有占用被回收，无残留、剩余不为负。
	a := m.ApplyBudget(0, boostT0.Add(time.Minute))
	logAlloc(t, "fault-zero", a)
	if a.ReservedW != 0 || a.RemainingW != 0 || len(a.Granted) != 0 {
		t.Fatalf("zero budget must reclaim everything, got %+v", a)
	}
	checkConservation(t, a)

	// 故障持续超过增援窗口：到期记录在零预算下也能干净清理。
	expired := m.ExpireBoosts(boostT0.Add(6 * time.Minute))
	t.Logf("[fault-expire] expired=%v", expired)
	if len(expired) != 1 || expired[0] != "drive" {
		t.Fatalf("boost should expire cleanly during fault, got %v", expired)
	}

	// 故障恢复：额度干净地重新可用，且不含任何增援残留。
	a = m.ApplyBudget(100, boostT0.Add(7*time.Minute))
	logAlloc(t, "recovered", a)
	got := grantMap(a)
	if got["drive"].PowerW != 50 || got["pump"].PowerW != 30 || got["ac"].PowerW != 20 {
		t.Fatalf("recovery should restore baseline without boost residue, got %+v", a.Granted)
	}
	if len(a.ActiveBoosts) != 0 {
		t.Fatalf("no boost may survive expiry, got %+v", a.ActiveBoosts)
	}
	checkConservation(t, a)
}

func TestBoostDeterministicReplay(t *testing.T) {
	script := func() Allocation {
		m := NewManager(100)
		m.Allocate(boostUnits())
		m.StartBoost(Unit{ID: "drive", Priority: 1, RequestW: 50, MinW: 40},
			BoostSpec{ExtraW: 40, Priority: 0, TTL: 5 * time.Minute}, boostT0)
		m.ApplyBudget(60, boostT0.Add(time.Minute))
		m.ApplyBudget(100, boostT0.Add(2*time.Minute))
		m.ExpireBoosts(boostT0.Add(5 * time.Minute))
		return m.Snapshot()
	}
	first, second := script(), script()
	if len(first.Granted) != len(second.Granted) {
		t.Fatalf("replay diverged: %d vs %d grants", len(first.Granted), len(second.Granted))
	}
	for i := range first.Granted {
		a, b := first.Granted[i], second.Granted[i]
		if a != b {
			t.Fatalf("replay diverged at grant %d: %+v vs %+v", i, a, b)
		}
	}
	if first.ReservedW != second.ReservedW || first.RemainingW != second.RemainingW {
		t.Fatalf("replay diverged: %+v vs %+v", first, second)
	}
	t.Logf("[replay] two identical runs produced identical allocations (reserved=%.1f)", first.ReservedW)
}

func TestConcurrentBoostRace(t *testing.T) {
	m := NewManager(1000)
	check := func(tag string) {
		a := m.Snapshot()
		sum := 0.0
		for _, g := range a.Granted {
			sum += g.PowerW
		}
		if math.Abs(sum-a.ReservedW) > 1e-6 ||
			a.ReservedW > a.TotalBudgetW+1e-6 || a.RemainingW < -1e-6 {
			t.Errorf("%s: invariant broken: %+v", tag, a)
		}
	}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			id := string(rune('A' + w))
			at := boostT0
			for i := 0; i < 150; i++ {
				at = at.Add(time.Millisecond)
				switch i % 5 {
				case 0:
					m.StartBoost(Unit{ID: id, Priority: w, RequestW: 20, MinW: 1},
						BoostSpec{ExtraW: 10, Priority: w - 1, TTL: 10 * time.Millisecond}, at)
				case 1:
					m.Request(Unit{ID: id, Priority: w, RequestW: 15, MinW: 1})
				case 2:
					m.StopBoost(id, at)
				case 3:
					m.ApplyBudget(500+float64((i*w)%500), at)
				default:
					m.ExpireBoosts(at)
				}
				check(id)
			}
			m.Release(id)
		}(w)
	}
	wg.Wait()
	a := m.Snapshot()
	t.Logf("[concurrent-boost] final reserved=%.1f remaining=%.1f holders=%d boosts=%d",
		a.ReservedW, a.RemainingW, len(a.Granted), len(a.ActiveBoosts))
	checkConservation(t, a)
}
