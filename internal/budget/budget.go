// Package budget 管理多个驱动与用电单元的并发功率预算申请。
//
// 分配协议(每个周期的一批申请在单个临界区内原子裁决):
//
//  1. 未在本批申请、也未被释放的单元，其既有预留继续保留；
//  2. 本批申请按优先级(数值小者优先, 同优先级按 UnitID 字典序)排序；
//  3. 每个单元依次在剩余预算上裁决：
//     available >= Want       -> granted  全额
//     Min <= available < Want -> degraded 降级到可用值
//     available < Min         -> rejected 拒绝(0W)
//  4. 预算收缩(故障)时，从优先级最低的预留开始回收，直到总和不超过预算；
//     预算恢复(故障解除)不会自动补发，单元需重新申请，杜绝重复计入。
//
// 守恒不变式：所有预留之和恒等于 AllocatedW，且
// AvailableW = TotalBudgetW - AllocatedW >= 0。
package budget

import (
	"fmt"
	"sort"
	"sync"
)

// Request 是一个用电单元的功率申请。
type Request struct {
	UnitID   string
	Priority int // 数值越小优先级越高
	WantW    float64
	MinW     float64 // 可接受的最低功率；低于则拒绝
	IsDrive  bool
}

// DecisionKind 是申请裁决类型。
type DecisionKind string

const (
	DecisionGranted  DecisionKind = "granted"
	DecisionDegraded DecisionKind = "degraded"
	DecisionRejected DecisionKind = "rejected"
)

// Decision 单个申请的裁决明细。
type Decision struct {
	UnitID   string
	Kind     DecisionKind
	GrantedW float64
	WantW    float64
	MinW     float64
	Priority int
	Reason   string
}

// Allocation 是一轮预算分配的结果快照。
type Allocation struct {
	Cycle        int64
	TotalBudgetW float64
	AllocatedW   float64
	AvailableW   float64
	Decisions    []Decision
}

// Reservation 是某单元当前持有的功率预留。
type Reservation struct {
	UnitID   string
	Priority int
	Watts    float64
	IsDrive  bool
}

// Manager 并发安全地管理预算与预留。
type Manager struct {
	mu          sync.Mutex
	total       float64
	reserved    map[string]Reservation
	cycle       int64
	invariantOK bool
}

// NewManager 创建预算管理器。
func NewManager(totalBudgetW float64) *Manager {
	if totalBudgetW < 0 {
		totalBudgetW = 0
	}
	return &Manager{
		total:       totalBudgetW,
		reserved:    map[string]Reservation{},
		invariantOK: true,
	}
}

// SetBudget 更新总预算。收缩时按优先级从低到高回收预留。
func (m *Manager) SetBudget(totalW float64) Allocation {
	m.mu.Lock()
	defer m.mu.Unlock()
	if totalW < 0 {
		totalW = 0
	}
	m.total = totalW
	m.cycle++
	m.rebalanceLocked()
	return m.snapshotLocked(nil)
}

// Request 原子处理一批申请并返回本批裁决；既有其他预留保留。
func (m *Manager) Request(reqs []Request) Allocation {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cycle++

	// 去重：同一单元在一批中出现多次时，仅首个参与分配，其余拒绝。
	seen := map[string]bool{}
	uniq := make([]Request, 0, len(reqs))
	dupRejected := []Decision{}
	for _, r := range reqs {
		if r.WantW < 0 {
			r.WantW = 0
		}
		if r.MinW < 0 {
			r.MinW = 0
		}
		if r.MinW > r.WantW {
			r.MinW = r.WantW
		}
		if seen[r.UnitID] {
			dupRejected = append(dupRejected, Decision{
				UnitID: r.UnitID, Kind: DecisionRejected,
				WantW: r.WantW, MinW: r.MinW, Priority: r.Priority,
				Reason: "同一周期内重复提交申请",
			})
			continue
		}
		seen[r.UnitID] = true
		uniq = append(uniq, r)
	}

	sort.SliceStable(uniq, func(i, j int) bool {
		if uniq[i].Priority != uniq[j].Priority {
			return uniq[i].Priority < uniq[j].Priority
		}
		return uniq[i].UnitID < uniq[j].UnitID
	})

	decisions := make([]Decision, 0, len(uniq)+len(dupRejected))
	// 重新申请的单元：先归还其旧预留，再按新申请裁决。
	for _, r := range uniq {
		delete(m.reserved, r.UnitID)
	}

	for _, r := range uniq {
		allocated := m.sumLocked()
		available := m.total - allocated
		var d Decision
		switch {
		case available+1e-9 >= r.WantW:
			d = Decision{r.UnitID, DecisionGranted, r.WantW, r.WantW, r.MinW, r.Priority,
				fmt.Sprintf("剩余预算 %.0fW 满足申请 %.0fW", available, r.WantW)}
			m.reserved[r.UnitID] = Reservation{r.UnitID, r.Priority, r.WantW, r.IsDrive}
		case available+1e-9 >= r.MinW:
			grant := available
			d = Decision{r.UnitID, DecisionDegraded, grant, r.WantW, r.MinW, r.Priority,
				fmt.Sprintf("预算不足: 申请 %.0fW, 仅剩 %.0fW(>=最低 %.0fW), 降级", r.WantW, available, r.MinW)}
			m.reserved[r.UnitID] = Reservation{r.UnitID, r.Priority, grant, r.IsDrive}
		default:
			d = Decision{r.UnitID, DecisionRejected, 0, r.WantW, r.MinW, r.Priority,
				fmt.Sprintf("预算不足: 仅剩 %.0fW < 最低需求 %.0fW, 拒绝", available, r.MinW)}
		}
		decisions = append(decisions, d)
	}
	decisions = append(decisions, dupRejected...)

	// 裁决后仍可能因精度问题越界，做一次回收校正。
	m.rebalanceLocked()
	return m.snapshotLocked(decisions)
}

// Release 释放指定单元的预留。
func (m *Manager) Release(unitIDs ...string) Allocation {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cycle++
	for _, id := range unitIDs {
		delete(m.reserved, id)
	}
	return m.snapshotLocked(nil)
}

// Snapshot 返回当前分配快照(含预留守恒校验)。
func (m *Manager) Snapshot() Allocation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked(nil)
}

// Reservations 返回当前全部预留的确定性(按优先级、UnitID)快照。
func (m *Manager) Reservations() []Reservation {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Reservation, 0, len(m.reserved))
	for _, r := range m.reserved {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].UnitID < out[j].UnitID
	})
	return out
}

// InvariantOK 返回内部守恒校验是否始终成立(测试用)。
func (m *Manager) InvariantOK() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.invariantOK
}

func (m *Manager) sumLocked() float64 {
	s := 0.0
	for _, r := range m.reserved {
		s += r.Watts
	}
	return s
}

// rebalanceLocked 在总预算收缩时从最低优先级预留开始回收。
func (m *Manager) rebalanceLocked() {
	for m.sumLocked() > m.total+1e-9 {
		// 找优先级数值最大(最低优先)、同优先级 Watts 最大的预留先回收。
		victim := ""
		var worst Reservation
		for id, r := range m.reserved {
			if victim == "" || r.Priority > worst.Priority ||
				(r.Priority == worst.Priority && r.Watts > worst.Watts) ||
				(r.Priority == worst.Priority && r.Watts == worst.Watts && id > victim) {
				victim, worst = id, r
			}
		}
		excess := m.sumLocked() - m.total
		if worst.Watts <= excess+1e-9 {
			delete(m.reserved, victim)
		} else {
			worst.Watts -= excess
			m.reserved[victim] = worst
		}
	}
}

func (m *Manager) snapshotLocked(decisions []Decision) Allocation {
	allocated := m.sumLocked()
	available := m.total - allocated
	if allocated < -1e-9 || available < -1e-9 {
		m.invariantOK = false
	}
	if allocated < 0 {
		allocated = 0
	}
	if available < 0 {
		available = 0
	}
	return Allocation{
		Cycle:        m.cycle,
		TotalBudgetW: m.total,
		AllocatedW:   allocated,
		AvailableW:   available,
		Decisions:    decisions,
	}
}
