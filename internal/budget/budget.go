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

// reservation 是某单元当前持有的预留，连同最近一次授予状态与原始申请参数
// 一起保存：快照能区分足额与降级授予，增援到期/收紧回收也能按原始申请归还或重算。
type reservation struct {
	priority int
	powerW   float64
	status   GrantStatus
	reason   string
	reqW     float64 // 原始申请功率
	minW     float64 // 原始降级下限
}

// Allocation 是一轮分配的完整快照。
type Allocation struct {
	TotalBudgetW float64
	Granted      []Grant
	ReservedW    float64
	RemainingW   float64
	Boosts       []Boost // 当前仍在窗口期内的增援，按 (优先级, ID) 确定排序
}

// Manager 管理总预算与各单元预留。所有方法均并发安全且原子。
type Manager struct {
	mu     sync.Mutex
	totalW float64
	held   map[string]reservation
	boosts map[string]boostState
	nowFn  func() time.Time // 时钟可注入，测试与管线用固定时间保证可复现
}

// NewManager 构造预算管理器。
func NewManager(totalBudgetW float64) *Manager {
	if totalBudgetW < 0 {
		totalBudgetW = 0
	}
	return &Manager{
		totalW: totalBudgetW,
		held:   make(map[string]reservation),
		boosts: make(map[string]boostState),
		nowFn:  time.Now,
	}
}

// SetClock 注入时钟（默认 time.Now）。管线与测试用它驱动增援到期，
// 保证同一串输入重复运行结论完全一致。
func (m *Manager) SetClock(nowFn func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if nowFn != nil {
		m.nowFn = nowFn
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
	boosts := make([]Boost, 0, len(m.boosts))
	for _, id := range ids {
		b, ok := m.boosts[id]
		if !ok {
			continue
		}
		boosts = append(boosts, Boost{
			UnitID:    id,
			Priority:  m.held[id].priority,
			PowerW:    m.held[id].powerW,
			ExpiresAt: b.expiresAt,
		})
	}
	remaining := m.totalW - reserved
	if remaining < 0 && remaining > -1e-6 {
			remaining = 0 // 浮点尾差钳位，剩余额度对外永不为负
	}
	return Allocation{
		TotalBudgetW: m.totalW,
		Granted:      grants,
		ReservedW:    reserved,
		RemainingW:   remaining,
		Boosts:       boosts,
	}
}

// Snapshot 返回当前预留的一致快照。
func (m *Manager) Snapshot() Allocation {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(m.nowFn())
	return m.snapshotLocked()
}

// Request 原子地为单元申请/调整功率预留。
// 预算不足时：可用量 >= MinW 则降级授予，否则拒绝（不改变其原有预留）。
// 若该单元持有生效中的增援，普通申请视为提前结束增援：先归还被挤占额度再按普通申请处理。
func (m *Manager) Request(u Unit) Grant {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(m.nowFn())
	if _, ok := m.boosts[u.ID]; ok {
		m.cancelBoostLocked(u.ID)
	}
	return m.requestLocked(u)
}

// requestLocked 是 Request 的持锁内部实现，也用于增援到期后的归还重算。
func (m *Manager) requestLocked(u Unit) Grant {
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
			reqW:   u.RequestW,
			minW:   u.MinW,
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
			reqW: u.RequestW,
			minW: u.MinW,
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
// 若该单元持有生效中的增援，释放即提前结束增援：增援预留移除、被挤占额度归还。
// 返回实际释放的瓦数，便于调用方在日志中核对。
func (m *Manager) Release(unitID string) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(m.nowFn())
	if _, ok := m.boosts[unitID]; ok {
		released := m.held[unitID].powerW
		m.cancelBoostLocked(unitID)
		return released
	}
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
// 例外：持有生效中增援的单元即使出现在本轮列表里也跳过，其预留由增援管理。
// 排序：优先级升序，同优先级按 ID 升序（结果可复现）。
func (m *Manager) Allocate(units []Unit) Allocation {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(m.nowFn())

	ordered := make([]Unit, 0, len(units))
	for _, u := range units {
		if _, ok := m.boosts[u.ID]; ok {
			continue // 增援窗口内该单元预留由增援管理，本轮不覆盖
		}
		ordered = append(ordered, u)
	}
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
				reqW:   req,
				minW:   minW,
			}
			available -= req
		case available+eps >= minW:
			m.held[u.ID] = reservation{
				priority: u.Priority, powerW: available, status: StatusDegraded,
				reason: fmt.Sprintf("priority allocation degraded to %.1fW (requested %.1fW, min %.1fW)",
					available, req, minW),
				reqW: req,
				minW: minW,
			}
			available = 0
		default:
			// 拒绝即不写入 held；不计入预留。
		}
	}
	return m.snapshotLocked()
}

// SetBudget 调整总预算（例如限功率结论变化后），单位瓦。
// 预算下调时立即按“优先级从低到高（同优先级按 ID 逆序）”回收已持有预留，
// 直到满足 ReservedW <= TotalBudgetW：边界单元回收差额（不低于其 MinW，
// 否则整笔收回），更低优先级单元整笔收回。因此任何时刻剩余额度不为负、
// 总量/已占用/剩余三者守恒；故障把预算打到 0 时全部预留被收清，不留残留占用。
// 被收回的单元可在后续 Request/Allocate 中按新预算重新申请。
func (m *Manager) SetBudget(totalW float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if totalW < 0 {
		totalW = 0
	}
	m.totalW = totalW
	m.sweepLocked(m.nowFn()) // 到期增援按新预算归还
	m.reclaimLocked()
}

// reclaimLocked 在预算收紧后按优先级从低到高回收预留，直到恢复守恒。
func (m *Manager) reclaimLocked() {
	for {
		excess := m.reservedLocked() - m.totalW
		if excess <= eps {
			return
		}
		id := m.lowestPriorityHolderLocked()
		if id == "" {
			return
		}
		r := m.held[id]
		newPower := r.powerW - excess
		if newPower > eps && newPower+eps >= r.minW {
			r.powerW = newPower
			r.status = StatusDegraded
			r.reason = fmt.Sprintf("budget tightened to %.1fW: reclaimed %.1fW by priority",
				m.totalW, excess)
			m.held[id] = r
		} else {
			delete(m.held, id) // 差额会击穿其 MinW：整笔收回
		}
	}
}

// lowestPriorityHolderLocked 返回当前优先级最低（数值最大；同优先级 ID 逆序）的持有单元。
func (m *Manager) lowestPriorityHolderLocked() string {
	best := ""
	for id, r := range m.held {
		if best == "" {
			best = id
			continue
		}
		br := m.held[best]
		if r.priority > br.priority || (r.priority == br.priority && id > best) {
			best = id
		}
	}
	return best
}

// TotalBudget 返回当前总预算。
func (m *Manager) TotalBudget() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.totalW
}
