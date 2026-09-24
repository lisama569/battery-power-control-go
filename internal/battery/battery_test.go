package battery

import (
	"testing"
	"time"

	"github.com/lisama569/battery-power-control-go/internal/sensing"
)

func rd(ch sensing.Channel, v float64, at time.Time, valid bool) sensing.Reading {
	return sensing.Reading{Channel: ch, Value: v, At: at, Valid: valid}
}

func TestSOCIntegrationAndFreeze(t *testing.T) {
	cfg := Config{CapacityAs: 3600, VNominal: 360, SOCInit: 0.5, UpdateMaxAge: 15 * time.Second}
	e := NewEstimator(cfg)
	t0 := time.Unix(100, 0)

	// 工况：100A 放电 10s => SOC 下降 100*10/3600 ≈ 0.2778
	rs := map[sensing.Channel]sensing.Reading{
		sensing.ChannelVoltage: rd(sensing.ChannelVoltage, 360, t0, true),
		sensing.ChannelCurrent: rd(sensing.ChannelCurrent, 100, t0, true),
		sensing.ChannelTemp:    rd(sensing.ChannelTemp, 30, t0, true),
		sensing.ChannelRPM:     rd(sensing.ChannelRPM, 1000, t0, true),
	}
	s := e.Update(rs, t0)
	t.Logf("工况 t0: I=100A => SOC=%.4f CurrentValid=%v", s.SOC, s.CurrentValid)
	if s.SOC != 0.5 {
		t.Fatalf("首个时刻无 dt, SOC 应保持 0.5, 得到 %.4f", s.SOC)
	}

	t1 := t0.Add(10 * time.Second)
	rs[sensing.ChannelCurrent] = rd(sensing.ChannelCurrent, 100, t1, true)
	s = e.Update(rs, t1)
	want := 0.5 - 100*10/3600.0
	t.Logf("工况 t1: I=100A 持续10s => SOC=%.4f 期望≈%.4f", s.SOC, want)
	if abs(s.SOC-want) > 1e-9 {
		t.Fatalf("SOC 积分错误: got %.6f want %.6f", s.SOC, want)
	}

	// 电流无效：SOC 必须冻结且 CurrentValid=false
	before := s.SOC
	t2 := t1.Add(10 * time.Second)
	rs[sensing.ChannelCurrent] = rd(sensing.ChannelCurrent, 0, t2, false)
	s = e.Update(rs, t2)
	t.Logf("工况 t2: 电流无效 => SOC=%.4f(应=%.4f) CurrentValid=%v", s.SOC, before, s.CurrentValid)
	if s.CurrentValid || s.SOC != before {
		t.Fatalf("电流无效时 SOC 应冻结, got SOC=%.6f valid=%v", s.SOC, s.CurrentValid)
	}
	if !s.VoltageValid || !s.TempValid || !s.RPMValid {
		t.Fatalf("其他通道有效性应透传, got %+v", s)
	}

	// 电流恢复：与上次有效电流的间隔过大(>UpdateMaxAge)，不得补积分，只从此刻继续
	t3 := t2.Add(10 * time.Second)
	rs[sensing.ChannelCurrent] = rd(sensing.ChannelCurrent, 100, t3, true)
	s = e.Update(rs, t3)
	t.Logf("工况 t3: 电流恢复(缺口20s) => SOC=%.4f(应仍=%.4f, 不补积分)", s.SOC, before)
	if s.SOC != before || !s.CurrentValid {
		t.Fatalf("缺口后恢复不应补积分, got %.6f valid=%v", s.SOC, s.CurrentValid)
	}
}

func TestSOCClamp(t *testing.T) {
	cfg := Config{CapacityAs: 100, SOCInit: 0.01, UpdateMaxAge: 30 * time.Second}
	e := NewEstimator(cfg)
	t0 := time.Unix(0, 0)
	e.Update(map[sensing.Channel]sensing.Reading{
		sensing.ChannelCurrent: rd(sensing.ChannelCurrent, 50, t0, true),
	}, t0)
	t1 := t0.Add(10 * time.Second)
	s := e.Update(map[sensing.Channel]sensing.Reading{
		sensing.ChannelCurrent: rd(sensing.ChannelCurrent, 50, t1, true),
	}, t1)
	t.Logf("工况: 小容量(100As) 50A*10s 过放 => SOC=%.4f(应钳位0)", s.SOC)
	if s.SOC != 0 {
		t.Fatalf("SOC 应钳位到 0, got %.4f", s.SOC)
	}
}

func TestInitClamp(t *testing.T) {
	e := NewEstimator(Config{CapacityAs: 1, SOCInit: 1.5})
	if e.Snapshot().SOC != 1 {
		t.Fatalf("初始 SOC 应钳位到 1")
	}
	e2 := NewEstimator(Config{CapacityAs: 1, SOCInit: -0.2})
	if e2.Snapshot().SOC != 0 {
		t.Fatalf("初始 SOC 应钳位到 0")
	}
	t.Log("工况: 越界初始 SOC => 钳位到 [0,1]")
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
