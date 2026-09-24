package pipeline

import (
	"sync"
	"testing"
	"time"

	"github.com/lisama569/battery-power-control-go/internal/budget"
	"github.com/lisama569/battery-power-control-go/internal/fault"
	"github.com/lisama569/battery-power-control-go/internal/sensing"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

func samples(t time.Time, v, i, temp, rpm float64) []sensing.Sample {
	return []sensing.Sample{
		{Channel: sensing.ChannelVoltage, Value: v, At: t},
		{Channel: sensing.ChannelCurrent, Value: i, At: t},
		{Channel: sensing.ChannelTemp, Value: temp, At: t},
		{Channel: sensing.ChannelRPM, Value: rpm, At: t},
	}
}

// TestFaultChain 模拟 健康 -> 过温 -> 严重过温(critical,预算0) -> 恢复
// 的完整链路，验证拒绝原因、故障、限幅与预算守恒一致。
func TestFaultChainOvertemp(t *testing.T) {
	cl := &clock{t: time.Unix(10000, 0)}
	p := New(DefaultConfig(), cl.now)
	drive := []budget.Request{{UnitID: "drive", Priority: 0, WantW: 250000, MinW: 50000}}

	// 健康期：建立有效读数与预留
	for k := 0; k < 5; k++ {
		tt := cl.advance(50 * time.Millisecond)
		r := p.Step(samples(tt, 360, 100, 30, 2000), StepRequest{Requests: drive})
		if len(r.Rejected) != 0 {
			t.Fatalf("健康样本不应被拒绝: %+v", r.Rejected)
		}
	}
	r := p.Snapshot()
	t.Logf("健康工况: T=30 SOC=%.2f => 上限=%.0fW 故障=%v drive预留=%.0f",
		r.Battery.SOC, r.Limits.MaxDischargeW, r.Faults.Codes(), r.Allocation.AllocatedW)
	if r.Limits.MaxDischargeW != 250000 || r.Allocation.AllocatedW != 250000 {
		t.Fatalf("健康期应满功率且drive全额, got %.0f/%.0f", r.Limits.MaxDischargeW, r.Allocation.AllocatedW)
	}

	// 注入一个跳变样本(温度30->100 超 MaxDelta=20)，应被拒绝且不影响结论
	tt := cl.advance(50 * time.Millisecond)
	bad := samples(tt, 360, 100, 100, 2000)
	r = p.Step(bad, StepRequest{})
	if len(r.Rejected) != 1 || r.Rejected[0].Reason != sensing.RejectJump {
		t.Fatalf("温度跳变应被判定 jump, got %+v", r.Rejected)
	}
	t.Logf("注入工况: 温度30->100跳变 => 拒绝原因=%q, 限幅仍=%.0fW",
		r.Rejected[0].Reason, r.Limits.MaxDischargeW)
	if r.Limits.MaxDischargeW != 250000 {
		t.Fatalf("被拒绝样本不得参与限幅")
	}

	// 温度快速爬升(每次+5, 在 MaxDelta=20 内)，直到 critical
	var crit TickResult
	for temp := 35.0; temp <= 80; temp += 5 {
		tt = cl.advance(50 * time.Millisecond)
		crit = p.Step(samples(tt, 360, 100, temp, 2000), StepRequest{})
	}
	t.Logf("过温工况: T=80 => 故障=%v 严重度=%s binding=%s 上限=%.0fW 依据=%q",
		crit.Faults.Codes(), crit.Faults.WorstSeverity(), crit.Limits.Binding,
		crit.Limits.MaxDischargeW, crit.Limits.ReasonText)
	if !crit.Faults.Has(fault.FaultOvertemp) || crit.Faults.WorstSeverity() != fault.SeverityCritical {
		t.Fatalf("80C 持续后应 critical 过温, got %v", crit.Faults.Codes())
	}
	if crit.Limits.MaxDischargeW != 0 {
		t.Fatalf("critical 过温应限功率为0, got %.0f", crit.Limits.MaxDischargeW)
	}
	// 预算收缩到0后，旧的 drive 预留必须被回收，不能超出新预算
	if crit.Allocation.AllocatedW != 0 || crit.Allocation.TotalBudgetW != 0 {
		t.Fatalf("预算0时预留必须回收, got total=%.0f alloc=%.0f",
			crit.Allocation.TotalBudgetW, crit.Allocation.AllocatedW)
	}

	// 恢复期：温度按 <=MaxDelta(20) 渐进下降到迟滞恢复阈值(48)以下并保持 ClearWindow
	var rec TickResult
	var recovered bool
	temp := 75.0
	for k := 0; k < 30; k++ {
		tt = cl.advance(50 * time.Millisecond)
		if temp > 40 {
			temp -= 5
		}
		rec = p.Step(samples(tt, 360, 100, temp, 2000), StepRequest{})
		if !rec.Faults.Has(fault.FaultOvertemp) {
			recovered = true
			break
		}
	}
	t.Logf("恢复工况: 温度渐进降到 %.0f => 故障=%v 上限=%.0fW 被拒样本=%d",
		temp, rec.Faults.Codes(), rec.Limits.MaxDischargeW, len(rec.Rejected))
	if !recovered || rec.Limits.MaxDischargeW != 250000 {
		t.Fatalf("迟滞恢复后故障应清除且满功率, got %v %.0f", rec.Faults.Codes(), rec.Limits.MaxDischargeW)
	}
	// 恢复后预算恢复，但旧预留不自动补发；重新申请应成功
	if rec.Allocation.AllocatedW != 0 {
		t.Fatalf("恢复不得自动补发旧预留, got %.0f", rec.Allocation.AllocatedW)
	}
	tt = cl.advance(50 * time.Millisecond)
	rec = p.Step(samples(tt, 360, 100, 40, 2000), StepRequest{Requests: drive})
	if rec.Allocation.AllocatedW != 250000 {
		t.Fatalf("重新申请应全额, got %.0f", rec.Allocation.AllocatedW)
	}
	t.Logf("恢复后重申请工况: drive => allocated=%.0f available=%.0f",
		rec.Allocation.AllocatedW, rec.Allocation.AvailableW)
}

func TestStaleSensorTriggersTimeout(t *testing.T) {
	cl := &clock{t: time.Unix(20000, 0)}
	p := New(DefaultConfig(), cl.now)
	tt := cl.t
	p.Step(samples(tt, 360, 100, 30, 2000), StepRequest{})

	// 时间推进但不提供新样本：读数陈旧；sensor_timeout 需确认窗口(100ms)
	tt = cl.advance(500 * time.Millisecond)
	r := p.Step(nil, StepRequest{})
	tt = cl.advance(110 * time.Millisecond)
	r = p.Step(nil, StepRequest{})
	t.Logf("缺失工况: 610ms 无有效样本 => 拒绝=%d 故障=%v 上限=%.0fW",
		len(r.Rejected), r.Faults.Codes(), r.Limits.MaxDischargeW)
	if !r.Faults.Has(fault.FaultSensorTimeout) {
		t.Fatalf("关键传感器陈旧应触发 sensor_timeout, got %v", r.Faults.Codes())
	}
	if r.Limits.MaxDischargeW != 0 {
		t.Fatalf("传感器超时时应禁止功率输出, got %.0f", r.Limits.MaxDischargeW)
	}
}

func TestSameInputsDeterministic(t *testing.T) {
	cl := &clock{t: time.Unix(30000, 0)}
	p1 := New(DefaultConfig(), cl.now)
	p2 := New(DefaultConfig(), cl.now)
	reqs := []budget.Request{
		{UnitID: "drive", Priority: 0, WantW: 200000, MinW: 50000},
		{UnitID: "hvac", Priority: 5, WantW: 80000, MinW: 20000},
	}
	for k := 0; k < 6; k++ {
		tt := cl.advance(50 * time.Millisecond)
		r1 := p1.Step(samples(tt, 350, 100, 50, 2000), StepRequest{Requests: reqs})
		r2 := p2.Step(samples(tt, 350, 100, 50, 2000), StepRequest{Requests: reqs})
		if r1.Limits.MaxDischargeW != r2.Limits.MaxDischargeW ||
			r1.Allocation.AllocatedW != r2.Allocation.AllocatedW ||
			len(r1.Faults.Active) != len(r2.Faults.Active) {
			t.Fatalf("相同输入序列结论不一致: %.1f/%.0f/%v vs %.1f/%.0f/%v",
				r1.Limits.MaxDischargeW, r1.Allocation.AllocatedW, r1.Faults.Codes(),
				r2.Limits.MaxDischargeW, r2.Allocation.AllocatedW, r2.Faults.Codes())
		}
	}
	t.Log("工况: 相同输入序列驱动两个管线 => 限幅/故障/分配完全一致")
}

func TestConcurrentSteps(t *testing.T) {
	cl := &clock{t: time.Unix(40000, 0)}
	p := New(DefaultConfig(), cl.now)
	var wg sync.WaitGroup
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			id := string(rune('a' + g))
			req := []budget.Request{{UnitID: id, Priority: g % 3, WantW: 40000, MinW: 5000}}
			for k := 0; k < 50; k++ {
				tt := cl.advance(50 * time.Millisecond)
				r := p.Step(samples(tt, 360, 100, 35, 2000), StepRequest{Requests: req})
				if r.Allocation.AllocatedW > r.Allocation.TotalBudgetW+1e-6 {
					t.Errorf("并发分配超预算: %.0f > %.0f", r.Allocation.AllocatedW, r.Allocation.TotalBudgetW)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	s := p.Snapshot()
	t.Logf("并发工况结束: 上限=%.0fW allocated=%.0f available=%.0f 预留数=%d",
		s.Limits.MaxDischargeW, s.Allocation.AllocatedW, s.Allocation.AvailableW,
		len(s.Allocation.Decisions))
}
