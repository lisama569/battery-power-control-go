// Command demo 用模拟传感器读数在本地驱动整条功率预算与电池保护管线。
//
// 运行： go run ./cmd/demo
package main

import (
	"fmt"
	"time"

	"github.com/lisama569/battery-power-control-go/internal/budget"
	"github.com/lisama569/battery-power-control-go/internal/pipeline"
	"github.com/lisama569/battery-power-control-go/internal/sensor"
)

type simSource struct {
	seq map[sensor.Channel]int64
}

func newSimSource() *simSource { return &simSource{seq: map[sensor.Channel]int64{}} }

// reset 清空序号记录，配合 pipeline.Reset 使用。
func (s *simSource) reset() { s.seq = map[sensor.Channel]int64{} }

// push 发送一路样本，序号自动递增。
func (s *simSource) push(p *pipeline.Pipeline, ch sensor.Channel, val float64, at time.Time) sensor.Verdict {
	s.seq[ch]++
	return p.Ingest(sensor.Sample{Channel: ch, Value: val, Timestamp: at, Seq: s.seq[ch]}, at)
}

// feedHealthy 喂入一组四路完整、彼此新鲜的样本。
func (s *simSource) feedHealthy(p *pipeline.Pipeline, temp, volt float64, at time.Time) {
	base := []struct {
		ch sensor.Channel
		v  float64
	}{
		{sensor.ChannelVoltage, volt},
		{sensor.ChannelCurrent, 25},
		{sensor.ChannelTemp, temp},
		{sensor.ChannelRPM, 3000},
	}
	for i, b := range base {
		v := s.push(p, b.ch, b.v, at.Add(time.Duration(i)*time.Millisecond))
		if !v.Accept {
			fmt.Printf("    ! rejected %s=%.1f reason=%s (%s)\n", b.ch, b.v, v.Reason, v.Detail)
		}
	}
}

var units = []budget.Unit{
	{ID: "drive", Priority: 1, RequestW: 120000, MinW: 40000},
	{ID: "steer", Priority: 2, RequestW: 15000, MinW: 8000},
	{ID: "pump", Priority: 3, RequestW: 12000, MinW: 4000},
	{ID: "ac", Priority: 5, RequestW: 40000, MinW: 5000},
}

func printReport(tag string, r pipeline.Report) {
	fmt.Printf("=== %s @ %s ===\n", tag, r.At.Format("15:04:05.000"))
	fmt.Printf("estimate: valid=%v V=%.1f I=%.1f T=%.1f rpm=%.0f SOC=%.3f %v\n",
		r.Estimate.Valid, r.Estimate.Voltage, r.Estimate.Current,
		r.Estimate.Temperature, r.Estimate.RPM, r.Estimate.SOC, r.Estimate.Notes)
	fmt.Printf("decision: power=%.0fW torque=%.1fNm healthy=%v faults=%v\n",
		r.Decision.PowerLimitW, r.Decision.TorqueLimitNm, r.Decision.Healthy, r.Decision.Faults)
	fmt.Printf("  binding: %s — %s\n", r.Decision.Binding.Name, r.Decision.Binding.Reason)
	fmt.Printf("budget: total=%.0f reserved=%.1f remaining=%.1f\n",
		r.Allocation.TotalBudgetW, r.Allocation.ReservedW, r.Allocation.RemainingW)
	for _, g := range r.Allocation.Granted {
		fmt.Printf("  grant %-6s %-8s %8.1fW  (%s)\n", g.UnitID, g.Status, g.PowerW, g.Reason)
	}
	for _, b := range r.Allocation.ActiveBoosts {
		fmt.Printf("  boost  %-6s +%8.1fW prio=%d until %s\n",
			b.UnitID, b.ExtraW, b.Priority, b.ExpiresAt.Format("15:04:05.000"))
	}
	if len(r.ExpiredBoosts) > 0 {
		fmt.Printf("  expired boosts returned this round: %v\n", r.ExpiredBoosts)
	}
	for _, fv := range r.Freshness {
		fmt.Printf("  freshness: %s %s (%s)\n", fv.Sample.Channel, fv.Reason, fv.Detail)
	}
	fmt.Println()
}

func main() {
	t0 := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	p := pipeline.New(pipeline.DefaultConfig())
	src := newSimSource()

	// 场景 1：坏样本不得静默参与计算。
	fmt.Println("########## scenario 1: bad samples rejected ##########")
	v := src.push(p, sensor.ChannelVoltage, 500, t0)
	fmt.Printf("bad voltage 500V: accept=%v reason=%s (%s)\n", v.Accept, v.Reason, v.Detail)
	v = src.push(p, sensor.ChannelVoltage, 360, t0)
	fmt.Printf("first voltage 360V: accept=%v\n", v.Accept)
	v = src.push(p, sensor.ChannelVoltage, 420, t0.Add(2*time.Millisecond))
	fmt.Printf("jumping voltage 420V: accept=%v reason=%s (%s)\n", v.Accept, v.Reason, v.Detail)
	printReport("incomplete channels -> safe state", p.Evaluate(nil, t0.Add(5*time.Millisecond)))

	// 场景 2：标称工况下按优先级分配。
	fmt.Println("########## scenario 2: nominal operation ##########")
	at := t0.Add(20 * time.Millisecond)
	src.feedHealthy(p, 30, 380, at)
	printReport("nominal 30C SOC 0.8 380V", p.Evaluate(units, at.Add(5*time.Millisecond)))

	// 场景 3：低温深降额，低优先级单元被挤压。
	fmt.Println("########## scenario 3: cold derating ##########")
	p.Reset(t0)
	src.reset()
	at = t0.Add(40 * time.Millisecond)
	src.feedHealthy(p, -20, 380, at)
	printReport("cold start -20C", p.Evaluate(units, at.Add(5*time.Millisecond)))

	// 场景 4：过温故障切断输出。
	fmt.Println("########## scenario 4: over-temperature fault ##########")
	p.Reset(t0)
	src.reset()
	base := t0.Add(60 * time.Millisecond)
	for i, temp := range []float64{30, 42, 54, 66, 78, 88, 92} {
		at2 := base.Add(time.Duration(i) * 5 * time.Millisecond)
		src.feedHealthy(p, temp, 380, at2)
	}
	printReport("92C fault -> zero output", p.Evaluate(units, base.Add(31*time.Millisecond)))

	// 场景 5：迟滞恢复，温度回落后预算与预留恢复守恒。
	fmt.Println("########## scenario 5: hysteresis recovery ##########")
	rec := base.Add(40 * time.Millisecond)
	for i, temp := range []float64{85, 78, 70, 60, 50, 40} {
		at2 := rec.Add(time.Duration(i) * 5 * time.Millisecond)
		src.feedHealthy(p, temp, 380, at2)
	}
	printReport("recovered to 40C", p.Evaluate(units, rec.Add(31*time.Millisecond)))

	// 场景 6：短时功率增援——抢占低优先级额度、限功率中途收紧回收、窗口到期归还。
	fmt.Println("########## scenario 6: temporary power boost ##########")
	p.Reset(t0)
	src.reset()
	at = t0.Add(100 * time.Millisecond)
	src.feedHealthy(p, 30, 380, at)
	now := at.Add(5 * time.Millisecond)
	printReport("baseline before boost", p.Evaluate(units, now))

	// drive 发起超车增援：+30kW，临时优先级 0，窗口 200ms。
	bg := p.RequestBoost(budget.Unit{ID: "drive", Priority: 1, RequestW: 120000, MinW: 40000},
		budget.BoostSpec{ExtraW: 30000, Priority: 0, TTL: 200 * time.Millisecond}, now)
	fmt.Printf("boost requested: drive status=%s power=%.1fW (%s)\n\n", bg.Status, bg.PowerW, bg.Reason)
	printReport("boost active: lower priorities preempted", p.Evaluate(units, now.Add(10*time.Millisecond)))

	// 窗口未结束，温度升至 60C：限功率收紧到 105kW，按优先级从低到高回收。
	for i, temp := range []float64{45, 60} {
		src.feedHealthy(p, temp, 380, now.Add(time.Duration(20+i*10)*time.Millisecond))
	}
	printReport("limit tightened to 105kW mid-window", p.Evaluate(units, now.Add(50*time.Millisecond)))

	// 温度回落且评估时钟越过窗口：到期归还，整体如同增援未发生。
	for i, temp := range []float64{45, 30} {
		src.feedHealthy(p, temp, 380, now.Add(time.Duration(150+i*10)*time.Millisecond))
	}
	printReport("boost expired: budget handed back", p.Evaluate(units, now.Add(210*time.Millisecond)))
}
