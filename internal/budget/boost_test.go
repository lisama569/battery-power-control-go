package budget

import (
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

// testClock 是可手动推进的时钟，保证增援到期行为可复现。
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }

var boostT0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func newBoostManager(total float64) (*Manager, *testClock) {
	clk := &testClock{now: boostT0}
	m := NewManager(total)
	m.SetClock(clk.Now)
	return m, clk
}

func grantOf(a Allocation, id string) (Grant, bool) {
	for _, g := range a.Granted {
		if g.UnitID == id {
			return g, true
		}
	}
	return Grant{}, false
}

// 短时增援：窗口内从低优先级单元挤占，到期后按原申请归还，如同增援未发生。
func TestBoostPreemptAndExpiryRestore(t *testing.T) {
	m, clk := newBoostManager(100)
	m.Request(Unit{ID: "drive", Priority: 2, RequestW: 30, MinW: 10})
	m.Request(Unit{ID: "ac", Priority: 9, RequestW: 60, MinW: 10})
	before := m.Snapshot()
	t.Logf("[before] reserved=%.1f remaining=%.1f", before.ReservedW, before.RemainingW)

	// 超车增援：申请 50W，窗口 2s，临时优先级 0（最高）。
	// 可用 10W，需从 ac（优先级 9）挤占 40W。
	g := m.RequestBoost(BoostSpec{
		Unit:     Unit{ID: "overtake", Priority: 2, RequestW: 50, MinW: 20},
		Priority: 0,
		TTL:      2 * time.Second,
	})
	t.Logf("[boost] status=%s power=%.1f reason=%s", g.Status, g.PowerW, g.Reason)
	if g.Status != StatusBoosted || g.PowerW != 50 {
		t.Fatalf("boost should be granted 50W, got %s %.1f", g.Status, g.PowerW)
	}
	during := m.Snapshot()
	ac, _ := grantOf(during, "ac")
	t.Logf("[during] ac degraded to %.1f (%s); reserved=%.1f remaining=%.1f boosts=%d",
		ac.PowerW, ac.Reason, during.ReservedW, during.RemainingW, len(during.Boosts))
	if math.Abs(ac.PowerW-20) > 1e-9 {
		t.Fatalf("ac should be preempted to 20W, got %.1f", ac.PowerW)
	}
	if len(during.Boosts) != 1 || during.Boosts[0].UnitID != "overtake" {
		t.Fatalf("expected one active boost, got %+v", during.Boosts)
	}
	if want := boostT0.Add(2 * time.Second); !during.Boosts[0].ExpiresAt.Equal(want) {
		t.Fatalf("expiry want %v got %v", want, during.Boosts[0].ExpiresAt)
	}
	checkConservation(t, during)

	// 窗口未结束：增援仍在。
	clk.now = boostT0.Add(1999 * time.Millisecond)
	if n := len(m.Snapshot().Boosts); n != 1 {
		t.Fatalf("boost must still be active before expiry, got %d", n)
	}

	// 窗口一过：增援预留移除，ac 按原申请 60W 归还，整体如同增援未发生。
	clk.now = boostT0.Add(2 * time.Second)
	after := m.Snapshot()
	t.Logf("[expired] reserved=%.1f remaining=%.1f boosts=%d", after.ReservedW, after.RemainingW, len(after.Boosts))
	if len(after.Boosts) != 0 {
		t.Fatalf("boost must expire, got %+v", after.Boosts)
	}
	if _, ok := grantOf(after, "overtake"); ok {
		t.Fatal("boost reservation must be removed after expiry")
	}
	ac, _ = grantOf(after, "ac")
	if ac.Status != StatusGranted || math.Abs(ac.PowerW-60) > 1e-9 {
		t.Fatalf("ac must be restored to 60W, got %s %.1f", ac.Status, ac.PowerW)
	}
	if math.Abs(after.ReservedW-before.ReservedW) > 1e-9 ||
		math.Abs(after.RemainingW-before.RemainingW) > 1e-9 {
		t.Fatalf("after expiry state must equal pre-boost: before=%+v after=%+v", before, after)
	}
	checkConservation(t, after)
}

// 增援窗口内限功率收紧：按优先级从低到高回收，增援（最高优先级）最后被触及。
func TestBoostTighteningReclaimByPriority(t *testing.T) {
	m, _ := newBoostManager(100)
	m.Request(Unit{ID: "pump", Priority: 5, RequestW: 30, MinW: 5})
	m.Request(Unit{ID: "ac", Priority: 9, RequestW: 20, MinW: 5})
	m.RequestBoost(BoostSpec{
		Unit:     Unit{ID: "evade", Priority: 1, RequestW: 50, MinW: 20},
		Priority: 0,
		TTL:      5 * time.Second,
	})
	before := m.Snapshot()
	t.Logf("[before-tighten] reserved=%.1f remaining=%.1f", before.ReservedW, before.RemainingW)
	checkConservation(t, before)

	// 温度恶化，上限从 100 压到 60：先整笔收回 ac(20)，再从 pump 收回 20（30→10，>=MinW 5）。
	m.SetBudget(60)
	a := m.Snapshot()
	for _, g := range a.Granted {
		t.Logf("[tightened] %-6s status=%-8s power=%.1f reason=%s", g.UnitID, g.Status, g.PowerW, g.Reason)
	}
	if _, ok := grantOf(a, "ac"); ok {
		t.Fatal("lowest priority ac must be fully reclaimed")
	}
	pump, _ := grantOf(a, "pump")
	if math.Abs(pump.PowerW-10) > 1e-9 {
		t.Fatalf("pump should be reclaimed to 10W, got %.1f", pump.PowerW)
	}
	evade, _ := grantOf(a, "evade")
	if evade.Status != StatusBoosted || math.Abs(evade.PowerW-50) > 1e-9 {
		t.Fatalf("boost must be reclaimed last (untouched here), got %s %.1f", evade.Status, evade.PowerW)
	}
	if a.RemainingW < 0 || a.ReservedW > a.TotalBudgetW+1e-9 {
		t.Fatalf("conservation violated: %+v", a)
	}
	checkConservation(t, a)

	// 继续收紧到 40：pump 差额 10 会击穿 MinW 5？pump 持有 10，需收 20 → 整笔收回；
	// 仍差 10，从增援 evade 收（50→40，>=MinW 20）。
	m.SetBudget(40)
	a = m.Snapshot()
	for _, g := range a.Granted {
		t.Logf("[tightened-2] %-6s status=%-8s power=%.1f reason=%s", g.UnitID, g.Status, g.PowerW, g.Reason)
	}
	if _, ok := grantOf(a, "pump"); ok {
		t.Fatal("pump must be fully reclaimed when reclaim would break its MinW")
	}
	evade, _ = grantOf(a, "evade")
	if math.Abs(evade.PowerW-40) > 1e-9 {
		t.Fatalf("boost should be trimmed to 40W, got %.1f", evade.PowerW)
	}
	checkConservation(t, a)
}

// 增援进行中故障把预算打到零：不卡死、不留残留占用；恢复后额度干净可用。
func TestBoostFaultZeroAndRecovery(t *testing.T) {
	m, clk := newBoostManager(100)
	m.Request(Unit{ID: "ac", Priority: 9, RequestW: 40, MinW: 10})
	m.RequestBoost(BoostSpec{
		Unit:     Unit{ID: "overtake", Priority: 1, RequestW: 50, MinW: 20},
		Priority: 0,
		TTL:      3 * time.Second,
	})
	t.Logf("[boost-active] reserved=%.1f", m.Snapshot().ReservedW)

	// 故障：预算归零，全部预留（含增援）被收清。
	m.SetBudget(0)
	a := m.Snapshot()
	t.Logf("[fault] reserved=%.1f remaining=%.1f holders=%d", a.ReservedW, a.RemainingW, len(a.Granted))
	if a.ReservedW != 0 || a.RemainingW != 0 || len(a.Granted) != 0 {
		t.Fatalf("fault to zero must leave no residual holds, got %+v", a)
	}
	// 故障期间操作不卡死、幂等。
	if rel := m.Release("overtake"); rel != 0 {
		t.Fatalf("release of reclaimed boost must be no-op, got %.1f", rel)
	}
	if g := m.Request(Unit{ID: "x", Priority: 1, RequestW: 10, MinW: 5}); g.Status != StatusRejected {
		t.Fatalf("request during zero budget must be rejected, got %s", g.Status)
	}

	// 恢复：额度干净可用，重新申请足额授予。
	m.SetBudget(100)
	g := m.Request(Unit{ID: "drive", Priority: 1, RequestW: 60, MinW: 20})
	if g.Status != StatusGranted || g.PowerW != 60 {
		t.Fatalf("budget must be cleanly reusable after recovery, got %+v", g)
	}
	// 增援窗口随后到期：ac 的原始申请在当前可用额度内归还，守恒保持。
	clk.now = boostT0.Add(3 * time.Second)
	a = m.Snapshot()
	t.Logf("[recovered+expired] reserved=%.1f remaining=%.1f boosts=%d", a.ReservedW, a.RemainingW, len(a.Boosts))
	if len(a.Boosts) != 0 {
		t.Fatalf("no boost may survive past expiry, got %+v", a.Boosts)
	}
	checkConservation(t, a)
}

// 并发发起增援/申请/释放：竞态安全、守恒始终成立。
func TestConcurrentBoostsRace(t *testing.T) {
	m, _ := newBoostManager(1000)
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			id := fmt.Sprintf("U%c", 'A'+w)
			for i := 0; i < 200; i++ {
				switch i % 4 {
				case 0:
					m.RequestBoost(BoostSpec{
						Unit:     Unit{ID: id, Priority: w, RequestW: 30, MinW: 5},
						Priority: w / 2,
						TTL:      time.Minute,
					})
				case 1:
					m.Request(Unit{ID: id, Priority: w, RequestW: 20, MinW: 1})
				case 2:
					m.Release(id)
				default:
					m.SetBudget(1000 - float64(i%5)*100)
				}
			}
		}(w)
	}
	wg.Wait()
	m.SetBudget(1000)
	a := m.Snapshot()
	t.Logf("[concurrent-boost] final reserved=%.1f remaining=%.1f holders=%d boosts=%d",
		a.ReservedW, a.RemainingW, len(a.Granted), len(a.Boosts))
	checkConservation(t, a)
	if a.ReservedW > a.TotalBudgetW+1e-6 || a.RemainingW < 0 {
		t.Fatalf("conservation violated under concurrency: %+v", a)
	}
}

// 同一串输入跑两遍，快照逐字段一致（可复现）。
func TestBoostDeterministicReplay(t *testing.T) {
	run := func() []Allocation {
		m, clk := newBoostManager(120)
		snaps := []Allocation{}
		m.Request(Unit{ID: "a", Priority: 3, RequestW: 40, MinW: 10})
		m.Request(Unit{ID: "b", Priority: 7, RequestW: 50, MinW: 10})
		m.RequestBoost(BoostSpec{Unit: Unit{ID: "c", Priority: 5, RequestW: 60, MinW: 15}, Priority: 0, TTL: time.Second})
		snaps = append(snaps, m.Snapshot())
		m.SetBudget(80)
		snaps = append(snaps, m.Snapshot())
		clk.now = boostT0.Add(time.Second)
		snaps = append(snaps, m.Snapshot())
		m.RequestBoost(BoostSpec{Unit: Unit{ID: "d", Priority: 6, RequestW: 30, MinW: 30}, Priority: 1, TTL: time.Minute})
		snaps = append(snaps, m.Snapshot())
		return snaps
	}
	first, second := run(), run()
	if len(first) != len(second) {
		t.Fatal("replay length mismatch")
	}
	for i := range first {
		a, b := first[i], second[i]
		if a.TotalBudgetW != b.TotalBudgetW || a.ReservedW != b.ReservedW || a.RemainingW != b.RemainingW {
			t.Fatalf("round %d totals differ: %+v vs %+v", i, a, b)
		}
		if fmt.Sprintf("%+v", a.Granted) != fmt.Sprintf("%+v", b.Granted) ||
			fmt.Sprintf("%+v", a.Boosts) != fmt.Sprintf("%+v", b.Boosts) {
			t.Fatalf("round %d grants/boosts differ:\n%+v\n%+v", i, a, b)
		}
		t.Logf("[replay-%d] reserved=%.1f remaining=%.1f grants=%d boosts=%d (identical across runs)",
			i, a.ReservedW, a.RemainingW, len(a.Granted), len(a.Boosts))
	}
}

// 可挤占额度仍达不到 MinW 时增援被拒绝，且不改写任何既有预留。
func TestBoostRejectedWhenInfeasible(t *testing.T) {
	m, _ := newBoostManager(100)
	m.Request(Unit{ID: "drive", Priority: 1, RequestW: 70, MinW: 30})
	m.Request(Unit{ID: "ac", Priority: 9, RequestW: 25, MinW: 5})
	before := m.Snapshot()

	// 可用 5W + 可挤占 25W（ac 优先级 9 < 增援优先级 4）= 30W < MinW 40 → 拒绝。
	g := m.RequestBoost(BoostSpec{
		Unit:     Unit{ID: "overtake", Priority: 2, RequestW: 60, MinW: 40},
		Priority: 4,
		TTL:      time.Second,
	})
	t.Logf("[boost-reject] status=%s reason=%s", g.Status, g.Reason)
	if g.Status != StatusRejected {
		t.Fatalf("expected rejection, got %s %.1f", g.Status, g.PowerW)
	}
	after := m.Snapshot()
	if fmt.Sprintf("%+v", before.Granted) != fmt.Sprintf("%+v", after.Granted) {
		t.Fatalf("rejected boost must not disturb existing holds:\n%+v\n%+v", before.Granted, after.Granted)
	}
	// 高优先级单元不可被挤占：drive 优先级 1 高于增援优先级 4。
	if d, _ := grantOf(after, "drive"); d.PowerW != 70 {
		t.Fatalf("higher-priority unit must never be preempted, got %.1f", d.PowerW)
	}
	checkConservation(t, after)
}
