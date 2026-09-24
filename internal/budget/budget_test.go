package budget

import (
	"sync"
	"testing"
)

func find(ds []Decision, id string) Decision {
	for _, d := range ds {
		if d.UnitID == id {
			return d
		}
	}
	return Decision{UnitID: id, Kind: DecisionRejected}
}

func TestPriorityAllocation(t *testing.T) {
	m := NewManager(1000)
	// 故意乱序提交，期望按优先级而非提交顺序分配
	a := m.Request([]Request{
		{UnitID: "hvac", Priority: 5, WantW: 400, MinW: 100},
		{UnitID: "drive", Priority: 0, WantW: 700, MinW: 200},
		{UnitID: "aux", Priority: 3, WantW: 300, MinW: 100},
	})
	t.Logf("工况: 预算1000, drive(p0)要700 aux(p3)要300 hvac(p5)要400(min100)")
	for _, d := range a.Decisions {
		t.Logf("  裁决 %s(p%d): %s granted=%.0f 依据=%q", d.UnitID, d.Priority, d.Kind, d.GrantedW, d.Reason)
	}
	if d := find(a.Decisions, "drive"); d.Kind != DecisionGranted || d.GrantedW != 700 {
		t.Fatalf("drive 应全额700, got %s %.0f", d.Kind, d.GrantedW)
	}
	if d := find(a.Decisions, "aux"); d.Kind != DecisionGranted || d.GrantedW != 300 {
		t.Fatalf("aux 应全额300, got %s %.0f", d.Kind, d.GrantedW)
	}
	if d := find(a.Decisions, "hvac"); d.Kind != DecisionRejected || d.GrantedW != 0 {
		t.Fatalf("hvac 剩余0<min100 应拒绝, got %s %.0f", d.Kind, d.GrantedW)
	}
	if a.AllocatedW != 1000 || a.AvailableW != 0 {
		t.Fatalf("守恒失败: allocated=%.0f available=%.0f", a.AllocatedW, a.AvailableW)
	}
}

func TestDegradeWhenBetweenMinAndWant(t *testing.T) {
	m := NewManager(500)
	a := m.Request([]Request{
		{UnitID: "drive", Priority: 0, WantW: 300, MinW: 100},
		{UnitID: "hvac", Priority: 5, WantW: 400, MinW: 100},
	})
	d := find(a.Decisions, "hvac")
	t.Logf("工况: 预算500 drive先取300, hvac剩200(100<=200<400) => %s granted=%.0f 依据=%q",
		d.Kind, d.GrantedW, d.Reason)
	if d.Kind != DecisionDegraded || d.GrantedW != 200 {
		t.Fatalf("hvac 应降级到200, got %s %.0f", d.Kind, d.GrantedW)
	}
	if a.AllocatedW != 500 || a.AvailableW != 0 {
		t.Fatalf("守恒失败: %.0f/%.0f", a.AllocatedW, a.AvailableW)
	}
}

func TestReservationKeptAndReleased(t *testing.T) {
	m := NewManager(1000)
	m.Request([]Request{{UnitID: "drive", Priority: 0, WantW: 600, MinW: 100}})
	// hvac 新申请时，drive 的预留继续占用
	a := m.Request([]Request{{UnitID: "hvac", Priority: 5, WantW: 500, MinW: 100}})
	if a.AllocatedW != 1000 {
		t.Fatalf("drive 预留应保留, 总占用应=1000(drive600+hvac400), got %.0f", a.AllocatedW)
	}
	d := find(a.Decisions, "hvac")
	t.Logf("工况: drive已预留600, hvac申请500 => %s granted=%.0f", d.Kind, d.GrantedW)
	if d.Kind != DecisionDegraded || d.GrantedW != 400 {
		t.Fatalf("hvac 应降级400, got %s %.0f", d.Kind, d.GrantedW)
	}

	// drive 释放后预算归还，hvac 重新申请可以全额
	a = m.Release("drive")
	t.Logf("工况: 释放 drive => allocated=%.0f available=%.0f", a.AllocatedW, a.AvailableW)
	if a.AllocatedW != 400 || a.AvailableW != 600 {
		t.Fatalf("释放后应只剩 hvac 400, got %.0f/%.0f", a.AllocatedW, a.AvailableW)
	}
	a = m.Request([]Request{{UnitID: "hvac", Priority: 5, WantW: 500, MinW: 100}})
	d = find(a.Decisions, "hvac")
	if d.Kind != DecisionGranted || d.GrantedW != 500 {
		t.Fatalf("释放后 hvac 应能全额500, got %s %.0f", d.Kind, d.GrantedW)
	}
	t.Logf("工况: 释放drive后hvac重新申请 => %s granted=%.0f", d.Kind, d.GrantedW)
	if !m.InvariantOK() {
		t.Fatal("守恒不变式被破坏")
	}
}

func TestBudgetShrinkReclaimsLowPriority(t *testing.T) {
	m := NewManager(1000)
	m.Request([]Request{
		{UnitID: "drive", Priority: 0, WantW: 600, MinW: 100},
		{UnitID: "hvac", Priority: 5, WantW: 400, MinW: 100},
	})
	// 故障：预算骤降到 500，应从最低优先级 hvac 开始回收
	a := m.SetBudget(500)
	t.Logf("工况: 故障导致预算 1000->500 => allocated=%.0f available=%.0f 预留=%v",
		a.AllocatedW, a.AvailableW, m.Reservations())
	if a.AllocatedW != 500 || a.AvailableW != 0 {
		t.Fatalf("应收回到500, got %.0f/%.0f", a.AllocatedW, a.AvailableW)
	}
	for _, r := range m.Reservations() {
		if r.UnitID != "drive" {
			t.Fatalf("低优先级 hvac 应先被回收, 仍存在: %+v", r)
		}
	}

	// 恢复：预算回到 1000，已回收的 hvac 不得自动重复计入
	a = m.SetBudget(1000)
	t.Logf("工况: 恢复预算1000 => allocated=%.0f(应仍为500, 不自动补发) available=%.0f",
		a.AllocatedW, a.AvailableW)
	if a.AllocatedW != 500 || a.AvailableW != 500 {
		t.Fatalf("恢复不得自动补发, got allocated=%.0f available=%.0f", a.AllocatedW, a.AvailableW)
	}
}

func TestRepeatedRequestDoesNotDoubleCount(t *testing.T) {
	m := NewManager(1000)
	for i := 0; i < 3; i++ {
		a := m.Request([]Request{{UnitID: "drive", Priority: 0, WantW: 600, MinW: 100}})
		if a.AllocatedW != 600 {
			t.Fatalf("第%d次重复申请后 allocated 应恒为600, got %.0f", i, a.AllocatedW)
		}
	}
	t.Logf("工况: 同一单元连续3轮申请600 => allocated 恒=600, 无重复计入")
}

func TestDuplicateInSameBatchRejected(t *testing.T) {
	m := NewManager(1000)
	a := m.Request([]Request{
		{UnitID: "drive", Priority: 0, WantW: 600, MinW: 100},
		{UnitID: "drive", Priority: 0, WantW: 600, MinW: 100},
	})
	rejects := 0
	for _, d := range a.Decisions {
		if d.UnitID == "drive" && d.Kind == DecisionRejected {
			rejects++
			t.Logf("工况: 同批重复申请 => 拒绝依据=%q", d.Reason)
		}
	}
	if rejects != 1 || a.AllocatedW != 600 {
		t.Fatalf("同批重复应拒绝1次且预留600, rejects=%d allocated=%.0f", rejects, a.AllocatedW)
	}
}

func TestConservationAcrossRandomSequence(t *testing.T) {
	m := NewManager(1000)
	units := []string{"drive", "steer", "brake", "hvac", "media", "lights"}
	for cycle := 0; cycle < 50; cycle++ {
		var reqs []Request
		for j, u := range units {
			if (cycle+j)%2 == 0 {
				reqs = append(reqs, Request{UnitID: u, Priority: j, WantW: 250, MinW: 50})
			}
		}
		a := m.Request(reqs)
		sum := 0.0
		for _, r := range m.Reservations() {
			sum += r.Watts
		}
		if abs(sum-a.AllocatedW) > 1e-6 || a.AllocatedW > a.TotalBudgetW+1e-6 ||
			abs(a.AvailableW-(a.TotalBudgetW-a.AllocatedW)) > 1e-6 {
			t.Fatalf("cycle %d 守恒破坏: sum=%.3f alloc=%.3f total=%.3f avail=%.3f",
				cycle, sum, a.AllocatedW, a.TotalBudgetW, a.AvailableW)
		}
		if cycle%10 == 0 {
			m.SetBudget(float64(400 + (cycle*97)%800))
		}
		if cycle%7 == 0 {
			m.Release(units[cycle%len(units)])
		}
	}
	t.Logf("工况: 50轮混合申请/释放/预算变动 => 预留和恒等于 Allocated, 不超预算, 不变式=%v", m.InvariantOK())
	if !m.InvariantOK() {
		t.Fatal("守恒不变式曾被破坏")
	}
}

func TestConcurrentRequests(t *testing.T) {
	m := NewManager(2000)
	var wg sync.WaitGroup
	for g := 0; g < 12; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			id := string(rune('a' + g))
			for i := 0; i < 200; i++ {
				m.Request([]Request{{UnitID: id, Priority: g % 4, WantW: 200, MinW: 50}})
				_ = m.Snapshot()
				if i%5 == 0 {
					m.Release(id)
				}
			}
		}(g)
	}
	wg.Wait()
	a := m.Snapshot()
	sum := 0.0
	for _, r := range m.Reservations() {
		sum += r.Watts
	}
	t.Logf("并发工况结束: total=%.0f 预留和=%.3f allocated=%.3f available=%.3f invariant=%v",
		a.TotalBudgetW, sum, a.AllocatedW, a.AvailableW, m.InvariantOK())
	if abs(sum-a.AllocatedW) > 1e-6 || a.AllocatedW > 2000+1e-6 || a.AvailableW < 0 {
		t.Fatalf("并发后守恒破坏: sum=%.3f alloc=%.3f avail=%.3f", sum, a.AllocatedW, a.AvailableW)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
