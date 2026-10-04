// 本文件实现短时功率增援（boost）：单元在有限窗口内临时抬高优先级，
// 可从优先级更低的单元挤占额度；窗口结束或被取消后，被挤占的额度按原申请归还，
// 整体效果如同增援从未发生。
//
// 规则要点：
//   - 增援始终受当前总预算约束：可挤占对象仅为生效优先级严格低于增援优先级的持有单元，
//     挤占顺序为优先级从低到高（同优先级按 ID 逆序），结果可复现；
//   - 可挤占总量仍不足以达到 MinW 时，增援被拒绝且不改写任何既有预留；
//   - 到期（sweep）或主动释放/普通申请（cancel）都会结束增援：增援预留移除，
//     被挤占单元按（优先级升序、ID 升序）以原始申请在当前可用额度内重新授予，
//     可能因预算已收紧而降级或被拒——始终满足守恒不变量；
//   - 预算收紧（SetBudget 下调，含故障归零）由 reclaimLocked 统一按优先级回收，
//     增援预留与其他预留一样参与回收，不留残留占用。
package budget

import (
	"fmt"
	"sort"
	"time"
)

// StatusBoosted 表示该笔授予来自一次生效中的短时增援。
const StatusBoosted GrantStatus = "boosted"

// BoostSpec 描述一次短时增援申请。
type BoostSpec struct {
	Unit     Unit          // 申请单元（使用其 RequestW/MinW）
	Priority int           // 窗口内生效的临时优先级（数值小者优先）
	TTL      time.Duration // 增援窗口时长，必须为正
}

// Boost 是一笔仍在窗口期内的增援的快照（用于 Allocation 与日志）。
type Boost struct {
	UnitID    string
	Priority  int
	PowerW    float64
	ExpiresAt time.Time
}

// boostState 是增援的内部状态：到期时间与被挤占单元的原始申请（用于归还）。
type boostState struct {
	expiresAt time.Time
	preempted []Unit
}

// RequestBoost 发起一次短时增援：在 TTL 窗口内以临时优先级占用功率，
// 可用额度不足时从优先级严格更低的持有单元挤占。整个操作原子完成。
func (m *Manager) RequestBoost(spec BoostSpec) Grant {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.nowFn()
	m.sweepLocked(now)

	u := spec.Unit
	if u.RequestW < 0 {
		u.RequestW = 0
	}
	if u.MinW < 0 {
		u.MinW = 0
	}
	if spec.TTL <= 0 {
		return Grant{UnitID: u.ID, Status: StatusRejected,
			Reason: fmt.Sprintf("boost TTL %v must be positive", spec.TTL)}
	}

	// 同一单元重复增援：先取消旧增援（归还其挤占），再按新申请处理。
	if _, ok := m.boosts[u.ID]; ok {
		m.cancelBoostLocked(u.ID)
	}
	// 暂存该单元既有普通预留，拒绝时原样恢复（申请失败不影响既有持有）。
	old, existed := m.held[u.ID]
	if existed {
		delete(m.held, u.ID)
	}

	available := m.totalW - m.reservedLocked()
	expiresAt := now.Add(spec.TTL)
	grant := Grant{UnitID: u.ID}

	// 候选挤占对象：生效优先级严格低于增援优先级的持有单元，
	// 按优先级从低到高、同优先级按 ID 逆序排列（可复现）。
	type candidate struct {
		id string
		r  reservation
	}
	cands := []candidate{}
	if available+eps < u.RequestW {
		ids := make([]string, 0, len(m.held))
		for id := range m.held {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool {
			pi, pj := m.held[ids[i]].priority, m.held[ids[j]].priority
			if pi != pj {
				return pi > pj
			}
			return ids[i] > ids[j]
		})
		for _, id := range ids {
			if m.held[id].priority > spec.Priority {
				cands = append(cands, candidate{id: id, r: m.held[id]})
			}
		}
	}

	preemptable := 0.0
	for _, c := range cands {
		preemptable += c.r.powerW
	}

	switch {
	case available+eps >= u.RequestW:
		// 无需挤占，直接授予。
		grant.Status = StatusBoosted
		grant.PowerW = u.RequestW
		grant.Reason = fmt.Sprintf("boost %.1fW within available %.1fW until %s",
			u.RequestW, available, expiresAt.Format("15:04:05.000"))
		m.held[u.ID] = reservation{
			priority: spec.Priority, powerW: u.RequestW, status: StatusBoosted,
			reason: grant.Reason, reqW: u.RequestW, minW: u.MinW,
		}
		m.boosts[u.ID] = boostState{expiresAt: expiresAt}
	case available+preemptable+eps < u.MinW:
		// 挤占全部低优先级额度仍达不到下限：拒绝，恢复既有预留。
		if existed {
			m.held[u.ID] = old
		}
		grant.Status = StatusRejected
		grant.PowerW = 0
		grant.Reason = fmt.Sprintf("boost infeasible: available %.1fW + preemptable %.1fW below min %.1fW",
			available, preemptable, u.MinW)
	default:
		// 执行挤占：只挤占满足本次授予所需的额度。
		granted := u.RequestW
		if available+preemptable < granted {
			granted = available + preemptable
		}
		toPreempt := granted - available
		preempted := []Unit{}
		for _, c := range cands {
			if toPreempt <= eps {
				break
			}
			take := c.r.powerW
			if take > toPreempt {
				take = toPreempt
			}
			preempted = append(preempted, Unit{
				ID: c.id, Priority: c.r.priority, RequestW: c.r.reqW, MinW: c.r.minW,
			})
			cur := m.held[c.id]
			if take+eps >= cur.powerW {
				delete(m.held, c.id) // 整笔挤占
			} else {
				cur.powerW -= take
				cur.status = StatusDegraded
				cur.reason = fmt.Sprintf("preempted %.1fW by boost %q", take, u.ID)
				m.held[c.id] = cur
			}
			toPreempt -= take
		}
		status := StatusBoosted
		if granted+eps < u.RequestW {
			status = StatusDegraded
		}
		grant.Status = status
		grant.PowerW = granted
		grant.Reason = fmt.Sprintf("boost %.1fW until %s: preempted %.1fW from %d lower-priority unit(s)",
			granted, expiresAt.Format("15:04:05.000"), granted-available, len(preempted))
		m.held[u.ID] = reservation{
			priority: spec.Priority, powerW: granted, status: status,
			reason: grant.Reason, reqW: u.RequestW, minW: u.MinW,
		}
		m.boosts[u.ID] = boostState{expiresAt: expiresAt, preempted: preempted}
	}
	return grant
}

// sweepLocked 回收所有已到期的增援并归还被挤占额度。
// 多笔同时到期时按（到期时间、单元 ID）升序处理，结果可复现。
func (m *Manager) sweepLocked(now time.Time) {
	if len(m.boosts) == 0 {
		return
	}
	expired := make([]string, 0, len(m.boosts))
	for id, b := range m.boosts {
		if !now.Before(b.expiresAt) {
			expired = append(expired, id)
		}
	}
	sort.Slice(expired, func(i, j int) bool {
		ti, tj := m.boosts[expired[i]].expiresAt, m.boosts[expired[j]].expiresAt
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return expired[i] < expired[j]
	})
	for _, id := range expired {
		m.cancelBoostLocked(id)
	}
}

// cancelBoostLocked 结束指定单元的增援：移除增援预留，
// 并按（优先级升序、ID 升序）以原始申请在当前可用额度内归还被挤占单元。
func (m *Manager) cancelBoostLocked(unitID string) {
	b, ok := m.boosts[unitID]
	if !ok {
		return
	}
	delete(m.boosts, unitID)
	delete(m.held, unitID) // 增援预留移除（若已被收紧回收则为空操作）

	restoring := make([]Unit, len(b.preempted))
	copy(restoring, b.preempted)
	sort.Slice(restoring, func(i, j int) bool {
		if restoring[i].Priority != restoring[j].Priority {
			return restoring[i].Priority < restoring[j].Priority
		}
		return restoring[i].ID < restoring[j].ID
	})
	for _, u := range restoring {
		m.requestLocked(u) // 以原始申请重算：足额/降级/拒绝取决于当前可用额度
	}
}
