// Package budget 实现多单元并发功率预算申请、优先级分配与预留守恒。
//
// 不变量（任何时刻成立）：
//
//	ReservedW == Σ 各持有单元的 PowerW
//	ReservedW + RemainingW == TotalBudgetW
//
// 每个单元最多持有一笔预留；重复 Request 是对该笔预留的原子调整，
// Release 对不存在的单元是空操作，因此故障/恢复过程中不会重复计入或漏掉已释放的预留。
package budget

import (
	"fmt"
	"sort"
	"sync"
)

// eps 用于功率比较，消除浮点尾差导致的抖动。
const eps = 1e-9

// Unit 描述一个申请功率预算的驱动/用电单元。
type Unit struct {
	ID       string
	Priority int     // 数值越小优先级越高
	RequestW float64 // 期望功率
	MinW     float64 // 降级下限：可用预算低于此值则拒绝
}

// GrantStatus 表示分配结果。
type GrantStatus string

const (
	StatusGranted  GrantStatus = "granted"
	StatusDegraded GrantStatus = "degraded"
	StatusRejected GrantStatus = "rejected"
)

// Grant 是一次申请的分配结果。
type Grant struct {
	UnitID string
	Status GrantStatus
	PowerW float64
	Reason string
}

// reservation 是某单元当前持有的预留，连同最近一次授予状态一起保存，
// 使快照能区分足额与降级授予。
type reservation struct {
	priority int
	powerW   float64
	status   GrantStatus
	reason   string
}

// Allocation 是一轮分配的完整快照。
type Allocation struct {
	TotalBudgetW float64
	Granted      []Grant
	ReservedW    float64
	RemainingW   float64
}

// Manager 管理总预算与各单元预留。所有方法均并发安全且原子。
type Manager struct {
	mu     sync.Mutex
	totalW float64
	held   map[string]reservation
}

// NewManager 构造预算管理器。
func NewManager(totalBudgetW float64) *Manager {
	if totalBudgetW < 0 {
		totalBudgetW = 0
	}
	return &Manager{totalW: totalBudgetW, held: make(map[string]reservation)}
}

// reservedLocked 计算当前已预留总额。
func (m *Manager) reservedLocked() float64 {
	sum := 0.0
	for _, r := range m.held {
		sum += r.powerW
	}
	return sum
}

// snapshotLocked 在持锁状态下构造一致快照，Grant 列表按优先级、ID 确定排序。
func (m *Manager) snapshotLocked() Allocation {
	ids := make([]string, 0, len(m.held))
	for id := range m.held {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		ri, rj := m.held[ids[i]], m.held[ids[j]]
		if ri.priority != rj.priority {
			return ri.priority < rj.priority
		}
		return ids[i] < ids[j]
	})
	grants := make([]Grant, 0, len(ids))
	reserved := 0.0
	for _, id := range ids {
		r := m.held[id]
		grants = append(grants, Grant{
			UnitID: id,
			Status: r.status,
			PowerW: r.powerW,
			Reason: r.reason,
		})
		reserved += r.powerW
	}
	return Allocation{
		TotalBudgetW: m.totalW,
		Granted:      grants,
		ReservedW:    reserved,
		RemainingW:   m.totalW - reserved,
	}
}

// Snapshot 返回当前预留的一致快照。
func (m *Manager) Snapshot() Allocation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

// Request 原子地为单元申请/调整功率预留。
// 预算不足时：可用量 >= MinW 则降级授予，否则拒绝（不改变其原有预留）。
func (m *Manager) Request(u Unit) Grant {
	m.mu.Lock()
	defer m.mu.Unlock()

	if u.RequestW < 0 {
		u.RequestW = 0
	}
	if u.MinW < 0 {
		u.MinW = 0
	}

	// 把该单元现有预留暂时移出，再以“剩余预算 + 旧预留”为可支配额度重算。
	old, existed := m.held[u.ID]
	if existed {
		delete(m.held, u.ID)
	}
	available := m.totalW - m.reservedLocked()

	g := Grant{UnitID: u.ID}
	switch {
	case available+eps >= u.RequestW:
		m.held[u.ID] = reservation{
			priority: u.Priority, powerW: u.RequestW, status: StatusGranted,
			reason: fmt.Sprintf("requested %.1fW within available %.1fW", u.RequestW, available),
		}
		g.Status = StatusGranted
		g.PowerW = u.RequestW
		g.Reason = m.held[u.ID].reason
	case available+eps >= u.MinW:
		granted := available
		m.held[u.ID] = reservation{
			priority: u.Priority, powerW: granted, status: StatusDegraded,
			reason: fmt.Sprintf("budget short: available %.1fW < requested %.1fW, degraded above min %.1fW",
				available, u.RequestW, u.MinW),
		}
		g.Status = StatusDegraded
		g.PowerW = granted
		g.Reason = m.held[u.ID].reason
	default:
		// 拒绝：恢复旧预留，保证“申请失败不影响既有持有”。
		if existed {
			m.held[u.ID] = old
		}
		g.Status = StatusRejected
		g.PowerW = 0
		g.Reason = fmt.Sprintf("available %.1fW below minimum %.1fW", available, u.MinW)
	}
	return g
}

// Release 释放单元持有的预留；对未持有单元为空操作（幂等）。
// 返回实际释放的瓦数，便于调用方在日志中核对。
func (m *Manager) Release(unitID string) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.held[unitID]
	if !ok {
		return 0
	}
	delete(m.held, unitID)
	return r.powerW
}

// Allocate 对一批申请做一轮原子的优先级分配。
// 该方法只在本轮预算内授予，不触碰本轮之外已有的其他单元预留；
// 对出现在本轮列表里的单元，则以本轮申请替换其旧预留。
// 排序：优先级升序，同优先级按 ID 升序（结果可复现）。
func (m *Manager) Allocate(units []Unit) Allocation {
	m.mu.Lock()
	defer m.mu.Unlock()

	ordered := make([]Unit, len(units))
	copy(ordered, units)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Priority != ordered[j].Priority {
			return ordered[i].Priority < ordered[j].Priority
		}
		return ordered[i].ID < ordered[j].ID
	})

	// 本轮出现的单元：先撤下其旧预留，统一按本轮结果重算。
	for _, u := range ordered {
		delete(m.held, u.ID)
	}

	available := m.totalW - m.reservedLocked()

	for _, u := range ordered {
		req := u.RequestW
		if req < 0 {
			req = 0
		}
		minW := u.MinW
		if minW < 0 {
			minW = 0
		}
		switch {
		case available+eps >= req:
			m.held[u.ID] = reservation{
				priority: u.Priority, powerW: req, status: StatusGranted,
				reason: fmt.Sprintf("priority allocation: %.1fW of requested %.1fW", req, req),
			}
			available -= req
		case available+eps >= minW:
			m.held[u.ID] = reservation{
				priority: u.Priority, powerW: available, status: StatusDegraded,
				reason: fmt.Sprintf("priority allocation degraded to %.1fW (requested %.1fW, min %.1fW)",
					available, req, minW),
			}
			available = 0
		default:
			// 拒绝即不写入 held；不计入预留。
		}
	}
	return m.snapshotLocked()
}

// SetBudget 调整总预算（例如限功率结论变化后），单位瓦。
// 已持有预留不立即回收，新预算在后续 Request/Allocate 中生效。
func (m *Manager) SetBudget(totalW float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if totalW < 0 {
		totalW = 0
	}
	m.totalW = totalW
}

// TotalBudget 返回当前总预算。
func (m *Manager) TotalBudget() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.totalW
}
