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
	"time"
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

// reservation 记录某单元的登记意愿与当前持有：
//   - desiredW/minW/priority 是最近一次申请登记的“意愿”，重算时以此为输入；
//   - powerW/status/reason 是最近一次重算（或 Request）得出的实际授予；
//   - effPriority 是最近一次重算使用的有效优先级（增援窗口内为临时优先级），
//     仅供快照确定排序。
//
// 被重算拒绝的单元保留登记（powerW=0, status=rejected），因此预算回升或
// 增援到期后能被确定地恢复；Release 才会把登记整体移除。
type reservation struct {
	priority    int
	desiredW    float64
	minW        float64
	powerW      float64
	status      GrantStatus
	reason      string
	effPriority int
}

// Allocation 是一轮分配的完整快照。
type Allocation struct {
	TotalBudgetW float64
	Granted      []Grant
	ReservedW    float64
	RemainingW   float64
	// ActiveBoosts 是快照时钟下仍在窗口内的增援，按单元 ID 升序，便于日志核对。
	ActiveBoosts []BoostInfo
}

// Manager 管理总预算与各单元预留。所有方法均并发安全且原子。
type Manager struct {
	mu     sync.Mutex
	totalW float64
	held   map[string]reservation
	boosts map[string]boost
	// now 是 Manager 的内部时钟：由带时间参数的方法（StartBoost/StopBoost/
	// ExpireBoosts/ApplyBudget）单调推进，用于判定增援窗口是否仍处于有效期。
	now time.Time
}

// NewManager 构造预算管理器。
func NewManager(totalBudgetW float64) *Manager {
	if totalBudgetW < 0 {
		totalBudgetW = 0
	}
	return &Manager{
		totalW: totalBudgetW,
		held:   make(map[string]reservation),
		boosts: make(map[string]boost),
	}
}

// reservedLocked 计算当前已预留总额。
func (m *Manager) reservedLocked() float64 {
	sum := 0.0
	for _, r := range m.held {
		sum += r.powerW
	}
	return sum
}

// snapshotLocked 在持锁状态下构造一致快照。
// Granted 只包含实际持有功率的单元（被重算拒绝、powerW=0 的登记不出现），
// 列表按有效优先级、ID 确定排序。
func (m *Manager) snapshotLocked() Allocation {
	ids := make([]string, 0, len(m.held))
	for id, r := range m.held {
		if r.status == StatusRejected {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		ri, rj := m.held[ids[i]], m.held[ids[j]]
		if ri.effPriority != rj.effPriority {
			return ri.effPriority < rj.effPriority
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
	boosts := make([]BoostInfo, 0, len(m.boosts))
	for id, b := range m.boosts {
		if m.now.Before(b.expiresAt) {
			boosts = append(boosts, BoostInfo{
				UnitID: id, ExtraW: b.extraW, Priority: b.priority, ExpiresAt: b.expiresAt,
			})
		}
	}
	sort.Slice(boosts, func(i, j int) bool { return boosts[i].UnitID < boosts[j].UnitID })
	return Allocation{
		TotalBudgetW: m.totalW,
		Granted:      grants,
		ReservedW:    reserved,
		RemainingW:   m.totalW - reserved,
		ActiveBoosts: boosts,
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
			priority: u.Priority, desiredW: u.RequestW, minW: u.MinW,
			powerW: u.RequestW, status: StatusGranted, effPriority: u.Priority,
			reason: fmt.Sprintf("requested %.1fW within available %.1fW", u.RequestW, available),
		}
		g.Status = StatusGranted
		g.PowerW = u.RequestW
		g.Reason = m.held[u.ID].reason
	case available+eps >= u.MinW:
		granted := available
		m.held[u.ID] = reservation{
			priority: u.Priority, desiredW: u.RequestW, minW: u.MinW,
			powerW: granted, status: StatusDegraded, effPriority: u.Priority,
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
// 该单元的增援登记（如有）一并清除，不留残留占用。
// 返回实际释放的瓦数，便于调用方在日志中核对。
func (m *Manager) Release(unitID string) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.held[unitID]
	if !ok {
		return 0
	}
	delete(m.held, unitID)
	delete(m.boosts, unitID)
	return r.powerW
}

// Allocate 对一批申请做一轮原子的优先级分配。
// 本轮出现的单元以其申请更新登记意愿，随后对全部登记单元做一次确定重算
// （见 recomputeLocked），因此本轮之外已有预留也会被一并纳入守恒校验。
// 排序：有效优先级升序，同优先级按 ID 升序（结果可复现）。
func (m *Manager) Allocate(units []Unit) Allocation {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, u := range units {
		req := u.RequestW
		if req < 0 {
			req = 0
		}
		minW := u.MinW
		if minW < 0 {
			minW = 0
		}
		// 登记/更新意愿；实际授予由重算统一得出。
		r := m.held[u.ID]
		r.priority = u.Priority
		r.desiredW = req
		r.minW = minW
		r.powerW = 0
		r.status = StatusRejected
		r.reason = "pending recompute"
		r.effPriority = u.Priority
		m.held[u.ID] = r
	}
	m.recomputeLocked()
	return m.snapshotLocked()
}

// recomputeLocked 在持锁状态下对全部登记单元做一次确定的贪心重算：
// 按有效优先级（增援窗口内使用临时优先级）升序、同优先级按 ID 升序依次授予，
// 可用量不足 MinW 的单元本轮持有 0（登记保留，预算回升后可恢复）。
// 重算后恒有 0 <= ReservedW <= TotalBudgetW，即剩余额度不为负、
// 任何单元占用不超过总量；预算收紧时自然从低优先级单元开始回收。
func (m *Manager) recomputeLocked() {
	type entry struct {
		id      string
		effPrio int
		wantW   float64
		minW    float64
		boosted bool
	}
	entries := make([]entry, 0, len(m.held))
	for id, r := range m.held {
		e := entry{id: id, effPrio: r.priority, wantW: r.desiredW, minW: r.minW}
		if b, ok := m.boosts[id]; ok && m.now.Before(b.expiresAt) {
			e.effPrio = b.priority
			e.wantW = r.desiredW + b.extraW
			e.boosted = true
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].effPrio != entries[j].effPrio {
			return entries[i].effPrio < entries[j].effPrio
		}
		return entries[i].id < entries[j].id
	})

	available := m.totalW
	for _, e := range entries {
		r := m.held[e.id]
		r.effPriority = e.effPrio
		note := ""
		if e.boosted {
			note = fmt.Sprintf(" [boost +%.1fW active, effective priority %d]",
				e.wantW-r.desiredW, e.effPrio)
		}
		switch {
		case available+eps >= e.wantW:
			r.powerW = e.wantW
			r.status = StatusGranted
			r.reason = fmt.Sprintf("recompute: granted %.1fW of wanted %.1fW (available %.1fW)%s",
				e.wantW, e.wantW, available, note)
			available -= e.wantW
		case available+eps >= e.minW:
			r.powerW = available
			r.status = StatusDegraded
			r.reason = fmt.Sprintf("recompute: degraded to %.1fW (wanted %.1fW, min %.1fW)%s",
				available, e.wantW, e.minW, note)
			available = 0
		default:
			r.powerW = 0
			r.status = StatusRejected
			r.reason = fmt.Sprintf("recompute: reclaimed, available %.1fW below min %.1fW%s",
				available, e.minW, note)
		}
		m.held[e.id] = r
	}
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
