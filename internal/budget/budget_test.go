package budget

import (
	"math"
	"sync"
	"testing"
)

func checkConservation(t *testing.T, a Allocation) {
	t.Helper()
	sum := 0.0
	for _, g := range a.Granted {
		sum += g.PowerW
	}
	if math.Abs(sum-a.ReservedW) > 1e-6 {
		t.Fatalf("reserved %.6f != sum of grants %.6f", a.ReservedW, sum)
	}
	if math.Abs(a.ReservedW+a.RemainingW-a.TotalBudgetW) > 1e-6 {
		t.Fatalf("budget not conserved: reserved %.6f + remaining %.6f != total %.6f",
			a.ReservedW, a.RemainingW, a.TotalBudgetW)
	}
}

func TestAllocatePriority(t *testing.T) {
	m := NewManager(100)
	units := []Unit{
		{ID: "ac", Priority: 5, RequestW: 40, MinW: 10}, // 低优先级
		{ID: "drive", Priority: 1, RequestW: 70, MinW: 30},
		{ID: "pump", Priority: 3, RequestW: 30, MinW: 5},
	}
	a := m.Allocate(units)
	t.Logf("[priority] budget=%.0f reserved=%.1f remaining=%.1f", a.TotalBudgetW, a.ReservedW, a.RemainingW)
	got := map[string]Grant{}
	for _, g := range a.Granted {
		got[g.UnitID] = g
		t.Logf("  %-6s status=%-8s power=%.1f", g.UnitID, g.Status, g.PowerW)
	}
	// drive 先拿 70；剩 30 给 pump 足额 30；ac 无预算，可用 0 < MinW 10，被拒。
	if got["drive"].PowerW != 70 {
		t.Fatalf("drive should get 70, got %.1f", got["drive"].PowerW)
	}
	if got["pump"].PowerW != 30 {
		t.Fatalf("pump should get 30, got %.1f", got["pump"].PowerW)
	}
	if _, ok := got["ac"]; ok {
		t.Fatalf("ac should be rejected, got %+v", got["ac"])
	}
	checkConservation(t, a)
}

func TestDegradeAndReject(t *testing.T) {
	m := NewManager(100)
	// 高优先级拿走 80，低优先级申请 40，下限 10，可用 20 → 降级到 20。
	if g := m.Request(Unit{ID: "hi", Priority: 1, RequestW: 80, MinW: 80}); g.Status != StatusGranted {
		t.Fatalf("hi grant: %s %s", g.Status, g.Reason)
	}
	g := m.Request(Unit{ID: "lo", Priority: 9, RequestW: 40, MinW: 10})
	t.Logf("[degrade] lo status=%s power=%.1f reason=%s", g.Status, g.PowerW, g.Reason)
	if g.Status != StatusDegraded || math.Abs(g.PowerW-20) > 1e-9 {
		t.Fatalf("expected degraded to 20, got %s %.1f", g.Status, g.PowerW)
	}
	checkConservation(t, m.Snapshot())

	// 再来一个下限高于剩余（0）的单元：拒绝且不产生预留。
	g = m.Request(Unit{ID: "no", Priority: 9, RequestW: 10, MinW: 5})
	t.Logf("[reject] no status=%s reason=%s", g.Status, g.Reason)
	if g.Status != StatusRejected {
		t.Fatalf("expected rejection, got %s", g.Status)
	}
	snap := m.Snapshot()
	if len(snap.Granted) != 2 {
		t.Fatalf("rejected unit must not be reserved, got %d grants", len(snap.Granted))
	}
	checkConservation(t, snap)
}

func TestRequestAdjustAndRelease(t *testing.T) {
	m := NewManager(100)
	m.Request(Unit{ID: "u", Priority: 1, RequestW: 60, MinW: 10})
	if r := m.Snapshot().ReservedW; math.Abs(r-60) > 1e-9 {
		t.Fatalf("reserved want 60 got %.1f", r)
	}
	// 调整为 40。
	g := m.Request(Unit{ID: "u", Priority: 1, RequestW: 40, MinW: 10})
	if g.Status != StatusGranted || g.PowerW != 40 {
		t.Fatalf("adjust failed: %+v", g)
	}
	snap := m.Snapshot()
	if len(snap.Granted) != 1 || math.Abs(snap.ReservedW-40) > 1e-9 {
		t.Fatalf("adjust must replace, not duplicate: %+v", snap.Granted)
	}
	t.Logf("[adjust] one reservation held at %.1fW, remaining %.1f", snap.ReservedW, snap.RemainingW)

	// 释放两次：第二次为空操作，不能重复扣减。
	if rel := m.Release("u"); math.Abs(rel-40) > 1e-9 {
		t.Fatalf("release returned %.1f want 40", rel)
	}
	if rel := m.Release("u"); rel != 0 {
		t.Fatalf("second release must be no-op, returned %.1f", rel)
	}
	snap = m.Snapshot()
	if snap.ReservedW != 0 || snap.RemainingW != 100 {
		t.Fatalf("after release budget must be fully free: %+v", snap)
	}
	checkConservation(t, snap)
}

func TestFaultRecoveryNoDoubleCount(t *testing.T) {
	// 模拟故障（预算被压到 0）再恢复（预算回升）：已持有预留不得重复计入。
	m := NewManager(100)
	m.Request(Unit{ID: "drive", Priority: 1, RequestW: 50, MinW: 10})
	m.Request(Unit{ID: "fan", Priority: 2, RequestW: 20, MinW: 5})
	m.SetBudget(0)
	t.Logf("[fault] budget=0 reserved=%.1f (existing holds kept until released)", m.Snapshot().ReservedW)

	// 故障期间释放一个单元。
	if rel := m.Release("fan"); rel != 20 {
		t.Fatalf("release during fault returned %.1f", rel)
	}
	// 恢复预算。
	m.SetBudget(100)
	a := m.Snapshot()
	t.Logf("[recover] reserved=%.1f remaining=%.1f grants=%d", a.ReservedW, a.RemainingW, len(a.Granted))
	if len(a.Granted) != 1 || a.Granted[0].UnitID != "drive" || a.ReservedW != 50 {
		t.Fatalf("after recovery only drive/50W should remain, got %+v", a.Granted)
	}
	checkConservation(t, a)
}

func TestConcurrentRequestsRace(t *testing.T) {
	m := NewManager(1000)
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			id := string(rune('A' + w))
			for i := 0; i < 200; i++ {
				g := m.Request(Unit{ID: id, Priority: w, RequestW: 20, MinW: 1})
				switch i % 3 {
				case 0:
					m.Release(id)
				case 1:
					m.Request(Unit{ID: id, Priority: w, RequestW: 5, MinW: 1})
				default:
					_ = g
				}
			}
		}(w)
	}
	wg.Wait()
	a := m.Snapshot()
	t.Logf("[concurrent] final reserved=%.1f remaining=%.1f holders=%d",
		a.ReservedW, a.RemainingW, len(a.Granted))
	checkConservation(t, a)
	if a.ReservedW > a.TotalBudgetW+1e-6 {
		t.Fatal("reserved exceeds total budget")
	}
}
