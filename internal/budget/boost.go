package budget

import (
	"fmt"
	"sort"
	"time"
)

// BoostSpec 描述一次短时功率增援（超车、避障等短时峰值场景）。
//
// 窗口内该单元的有效优先级被临时抬高为 Priority，期望功率在登记意愿上
// 追加 ExtraW；多出的功率先取自剩余额度，不足时按优先级从低到高挤占。
// 窗口结束（ExpireBoosts/StopBoost）后按各单元登记意愿整体重算，
// 被挤占的额度自动归还，效果等同于增援从未发生。
type BoostSpec struct {
	ExtraW   float64       // 窗口内追加的功率（瓦），负值按 0 处理
	Priority int           // 窗口内临时优先级（数值小者优先）
	TTL      time.Duration // 增援窗口时长，必须为正，否则申请被拒绝
}

// BoostInfo 是快照时仍处于窗口内的增援描述，用于日志与核对。
type BoostInfo struct {
	UnitID    string
	ExtraW    float64
	Priority  int
	ExpiresAt time.Time
}

// boost 是 Manager 内部保存的增援记录。
type boost struct {
	extraW    float64
	priority  int
	expiresAt time.Time
}

// advanceClock 把内部时钟单调推进到 now（并发下过期/乱序的时钟不会回拨，
// 因此增援窗口的判定对同一串单调输入永远一致）。
func (m *Manager) advanceClock(now time.Time) {
	if now.After(m.now) {
		m.now = now
	}
}

// StartBoost 为单元发起短时增援：登记基础申请与增援记录，并按有效优先级
// 立即重算（可能挤占更低优先级单元的额度）。整个操作原子完成。
// TTL 非正时不改变任何状态，直接返回拒绝。
func (m *Manager) StartBoost(u Unit, spec BoostSpec, now time.Time) Grant {
	m.mu.Lock()
	defer m.mu.Unlock()

	if spec.TTL <= 0 {
		return Grant{
			UnitID: u.ID,
			Status: StatusRejected,
			Reason: fmt.Sprintf("boost TTL %v must be positive", spec.TTL),
		}
	}
	if spec.ExtraW < 0 {
		spec.ExtraW = 0
	}
	req := u.RequestW
	if req < 0 {
		req = 0
	}
	minW := u.MinW
	if minW < 0 {
		minW = 0
	}

	// 登记基础意愿与增援记录，重算统一得出实际授予。
	r := m.held[u.ID]
	r.priority = u.Priority
	r.desiredW = req
	r.minW = minW
	m.held[u.ID] = r
	m.boosts[u.ID] = boost{extraW: spec.ExtraW, priority: spec.Priority, expiresAt: now.Add(spec.TTL)}
	m.advanceClock(now)
	m.recomputeLocked()

	held := m.held[u.ID]
	return Grant{UnitID: u.ID, Status: held.status, PowerW: held.powerW, Reason: held.reason}
}

// StopBoost 提前结束增援（幂等）：移除增援记录并立即重算，
// 被挤占的额度随重算归还。返回是否确实存在进行中的增援。
func (m *Manager) StopBoost(unitID string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.boosts[unitID]; !ok {
		return false
	}
	delete(m.boosts, unitID)
	m.advanceClock(now)
	m.recomputeLocked()
	return true
}

// ExpireBoosts 结束所有到期增援并重算，归还被挤占额度。
// 返回到期单元 ID（按 ID 升序，便于日志核对与复现）。
func (m *Manager) ExpireBoosts(now time.Time) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.advanceClock(now)
	expired := make([]string, 0, len(m.boosts))
	for id, b := range m.boosts {
		if !m.now.Before(b.expiresAt) {
			expired = append(expired, id)
		}
	}
	if len(expired) == 0 {
		return nil
	}
	sort.Strings(expired)
	for _, id := range expired {
		delete(m.boosts, id)
	}
	m.recomputeLocked()
	return expired
}

// ApplyBudget 设置总预算并立即重算：预算收紧时按有效优先级从低到高回收，
// 保证任何时刻 0 <= ReservedW <= TotalBudgetW，剩余额度不为负；
// 预算为 0（故障）时所有持有被回收但登记保留，预算恢复后重算即可干净复原。
func (m *Manager) ApplyBudget(totalW float64, now time.Time) Allocation {
	m.mu.Lock()
	defer m.mu.Unlock()
	if totalW < 0 {
		totalW = 0
	}
	m.totalW = totalW
	m.advanceClock(now)
	m.recomputeLocked()
	return m.snapshotLocked()
}
