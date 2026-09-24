// Command demo 用模拟传感器读数驱动整车功率预算与电池保护管线。
//
// 运行: go run ./cmd/demo
// 它用一个确定性的虚拟时钟驱动若干“工况场景”，每个周期打印:
//   - 输入工况(电压/电流/温度/转速)与被拒绝样本及原因；
//   - 电池 SOC、激活故障及其阈值依据；
//   - binding 限功率约束、放电/转矩/回馈上限与解释；
//   - 各单元预算裁决与 available/allocated 守恒数字。
package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lisama569/battery-power-control-go/internal/budget"
	"github.com/lisama569/battery-power-control-go/internal/pipeline"
	"github.com/lisama569/battery-power-control-go/internal/sensing"
)

type simClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *simClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *simClock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

type condition struct {
	name             string
	voltage, current float64
	temp, rpm        float64
	badTemp          float64 // >0 时注入一个温度跳变样本(应被拒绝)
}

func main() {
	cl := &simClock{t: time.Unix(1_700_000_000, 0)}
	p := pipeline.New(pipeline.DefaultConfig(), cl.now)

	scenarios := []condition{
		{"巡航-常温", 365, 120, 32, 3000, 0},
		{"巡航-常温", 365, 120, 33, 3000, 0},
		{"温度缓升", 362, 150, 50, 3200, 0},
		{"温度缓升", 360, 180, 58, 3400, 0},
		{"温度缓升", 358, 200, 66, 3600, 0},
		{"严重过温", 356, 100, 78, 3000, 0},
		{"严重过温-持续确认", 356, 100, 78, 3000, 0},
		{"严重过温-持续确认", 356, 100, 78, 3000, 0},
		{"注入温度跳变(应拒绝)", 356, 100, 78, 3000, 8}, // 78->8 量程内但反向跳变
		{"过温持续", 356, 80, 78, 2800, 0},
		{"缓慢恢复", 358, 100, 70, 3000, 0},
		{"缓慢恢复", 360, 120, 60, 3100, 0},
		{"缓慢恢复", 362, 130, 50, 3100, 0},
		{"缓慢恢复", 365, 140, 42, 3000, 0},
		{"恢复后巡航", 368, 150, 38, 2900, 0},
		{"恢复后巡航", 368, 150, 35, 2900, 0},
		{"恢复后巡航", 368, 150, 33, 2900, 0},
		{"恢复后巡航", 368, 150, 32, 2900, 0},
	}

	units := []budget.Request{
		{UnitID: "drive_motor_l", Priority: 0, WantW: 120000, MinW: 40000, IsDrive: true},
		{UnitID: "drive_motor_r", Priority: 0, WantW: 120000, MinW: 40000, IsDrive: true},
		{UnitID: "epas_steer", Priority: 1, WantW: 6000, MinW: 2000},
		{UnitID: "brake_by_wire", Priority: 1, WantW: 4000, MinW: 2000},
		{UnitID: "hvac", Priority: 5, WantW: 30000, MinW: 5000},
		{UnitID: "infotainment", Priority: 9, WantW: 2000, MinW: 500},
	}

	fmt.Println("=== 整车功率预算与电池保护管线 demo ===")
	for step, sc := range scenarios {
		tt := cl.advance(50 * time.Millisecond)
		samples := []sensing.Sample{
			{Channel: sensing.ChannelVoltage, Value: sc.voltage, At: tt},
			{Channel: sensing.ChannelCurrent, Value: sc.current, At: tt},
			{Channel: sensing.ChannelTemp, Value: sc.temp, At: tt},
			{Channel: sensing.ChannelRPM, Value: sc.rpm, At: tt},
		}
		if sc.badTemp > 0 {
			// 在合法采样间隔内追加第二个温度读数，制造数值跳变(应被 jump 拒绝)。
			samples = append(samples, sensing.Sample{
				Channel: sensing.ChannelTemp, Value: sc.badTemp,
				At: tt.Add(10 * time.Millisecond),
			})
			cl.advance(10 * time.Millisecond)
		}

		// 每 3 个周期让 hvac/infotainment 重新申请一次，演示保留/再申请。
		req := pipeline.StepRequest{}
		for i, u := range units {
			if i < 4 || step%3 == 0 {
				req.Requests = append(req.Requests, u)
			}
		}
		r := p.Step(samples, req)
		printTick(step, sc, r)
	}

	// 演示显式锁存复位路径(本场景未触发 cutoff，仅演示 API 可用)
	_ = p.ResetLatched()
}

func printTick(step int, sc condition, r pipeline.TickResult) {
	var b strings.Builder
	fmt.Fprintf(&b, "\n[周期 %02d] %s @ %s\n", step, sc.name, r.At.Format("15:04:05.000"))
	fmt.Fprintf(&b, "  输入: V=%.0fV I=%.0fA T=%.0fC RPM=%.0f  SOC=%.1f%%\n",
		sc.voltage, sc.current, sc.temp, sc.rpm, r.Battery.SOC*100)
	for _, rej := range r.Rejected {
		prev := "-"
		if rej.Prev != nil {
			prev = fmt.Sprintf("%.1f", *rej.Prev)
		}
		fmt.Fprintf(&b, "  拒绝样本: channel=%s value=%.1f prev=%s 原因=%s\n",
			rej.Sample.Channel, rej.Sample.Value, prev, rej.Reason)
	}
	if len(r.Faults.Active) == 0 {
		fmt.Fprintf(&b, "  故障: 无\n")
	} else {
		for _, c := range r.Faults.Codes() {
			f := r.Faults.Active[c]
			fmt.Fprintf(&b, "  故障: %s(%s) 当前值=%.2f 阈值=%.2f 依据=%s\n",
				f.Code, f.Severity, f.Value, f.Limit, f.Message)
		}
	}
	fmt.Fprintf(&b, "  限幅: 放电<=%.0fW 转矩<=%.0fNm 回馈<=%.0fW binding=%s\n",
		r.Limits.MaxDischargeW, r.Limits.MaxTorqueNm, r.Limits.MaxRegenW, r.Limits.Binding)
	fmt.Fprintf(&b, "        依据: %s\n", r.Limits.ReasonText)
	fmt.Fprintf(&b, "  预算: total=%.0f allocated=%.0f available=%.0f\n",
		r.Allocation.TotalBudgetW, r.Allocation.AllocatedW, r.Allocation.AvailableW)
	for _, d := range r.Allocation.Decisions {
		fmt.Fprintf(&b, "    - %-16s p%d %-9s granted=%7.0f (want=%.0f min=%.0f) %s\n",
			d.UnitID, d.Priority, d.Kind, d.GrantedW, d.WantW, d.MinW, d.Reason)
	}
	fmt.Print(b.String())
}
