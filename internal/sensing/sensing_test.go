package sensing

import (
	"sync"
	"testing"
	"time"
)

func newTestEstimator(now *time.Time) *Estimator {
	return NewEstimator(DefaultConfigs(), func() time.Time { return *now })
}

func TestChannelAcceptReasons(t *testing.T) {
	now := time.Unix(1000, 0)
	base := now.Add(-80 * time.Millisecond) // 首个样本年龄 80ms (<=MaxAge)
	cfg := ChannelConfig{
		Min: 0, Max: 100, MaxDelta: 10,
		MaxAge:      100 * time.Millisecond,
		MinInterval: 10 * time.Millisecond,
		MaxInterval: 200 * time.Millisecond,
	}
	c := NewChannelEstimator(cfg)

	cases := []struct {
		name   string
		s      Sample
		reason RejectReason
		accept bool
	}{
		{"first_valid", Sample{ChannelVoltage, 50, base}, RejectNone, true},
		{"out_of_range_high", Sample{ChannelVoltage, 101, base.Add(20 * time.Millisecond)}, RejectOutOfRange, false},
		{"out_of_range_low", Sample{ChannelVoltage, -1, base.Add(20 * time.Millisecond)}, RejectOutOfRange, false},
		{"jump", Sample{ChannelVoltage, 99, base.Add(20 * time.Millisecond)}, RejectJump, false},
		{"bad_interval_too_soon", Sample{ChannelVoltage, 51, base.Add(3 * time.Millisecond)}, RejectBadInterval, false},
		{"stale", Sample{ChannelVoltage, 51, now.Add(-200 * time.Millisecond)}, RejectStale, false},
		{"future_ts", Sample{ChannelVoltage, 51, now.Add(50 * time.Millisecond)}, RejectStale, false},
		{"valid_next", Sample{ChannelVoltage, 55, base.Add(50 * time.Millisecond)}, RejectNone, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, accepted := c.Accept(tc.s, now)
			t.Logf("工况: value=%.1f 样本年龄=%v => 判定 reason=%q accepted=%v",
				tc.s.Value, now.Sub(tc.s.At), reason, accepted)
			if accepted != tc.accept || reason != tc.reason {
				t.Fatalf("期望 reason=%q accepted=%v, 实际 reason=%q accepted=%v",
					tc.reason, tc.accept, reason, accepted)
			}
		})
	}
}

func TestReadingsMissingAndStale(t *testing.T) {
	now := time.Unix(2000, 0)
	e := newTestEstimator(&now)

	missing := e.Readings()
	for ch, r := range missing {
		t.Logf("初始工况 channel=%s => valid=%v reason=%q", ch, r.Valid, r.Reason)
		if r.Valid || r.Reason != RejectMissing {
			t.Fatalf("通道 %s 初始应为 missing, 得到 %q", ch, r.Reason)
		}
	}

	rejected := e.Ingest([]Sample{{ChannelTemp, 25, now}})
	if len(rejected) != 0 {
		t.Fatalf("首个有效温度样本不应被拒绝, 实际: %+v", rejected)
	}
	r := e.Readings()[ChannelTemp]
	t.Logf("工况: temp=25 刚摄入 => valid=%v value=%.1f", r.Valid, r.Value)
	if !r.Valid || r.Value != 25 {
		t.Fatalf("温度读数应有效且为 25, 得到 %+v", r)
	}

	now = now.Add(600 * time.Millisecond) // 超过 temp MaxAge=500ms
	r = e.Readings()[ChannelTemp]
	t.Logf("工况: 推进 %v 后 => valid=%v reason=%q", 600*time.Millisecond, r.Valid, r.Reason)
	if r.Valid || r.Reason != RejectStale {
		t.Fatalf("陈旧温度应为 stale, 得到 %+v", r)
	}
}

func TestUnknownChannelRejected(t *testing.T) {
	now := time.Unix(3000, 0)
	e := newTestEstimator(&now)
	recs := e.Ingest([]Sample{{Channel("humidity"), 50, now}})
	if len(recs) != 1 || recs[0].Reason != RejectUnknownChan {
		t.Fatalf("未知通道应被拒绝为 unknown_channel, 得到 %+v", recs)
	}
	t.Logf("工况: 未知通道 humidity => 判定 %q", recs[0].Reason)
}

func TestRejectedSampleDoesNotMutateEstimate(t *testing.T) {
	now := time.Unix(4000, 0)
	e := newTestEstimator(&now)
	e.Ingest([]Sample{{ChannelVoltage, 350, now}})
	// 推进时钟后，量程内但跳变的样本必须被拒绝
	now = now.Add(20 * time.Millisecond)
	recs := e.Ingest([]Sample{{ChannelVoltage, 250, now}})
	if len(recs) != 1 || recs[0].Reason != RejectJump {
		t.Fatalf("期望 jump 拒绝, 得到 %+v", recs)
	}
	// 下一正常样本的跳变判定仍应基于 350 而非被拒绝的 250
	now = now.Add(20 * time.Millisecond)
	recs = e.Ingest([]Sample{{ChannelVoltage, 360, now}})
	if len(recs) != 0 {
		t.Fatalf("被拒绝样本不得污染估计, 但收到拒绝: %+v", recs)
	}
	t.Logf("工况: 350 -> 250(jump拒绝) -> 360 => 估计保持 350, 360 正常接受")
}

func TestConcurrentIngestAndRead(t *testing.T) {
	var clockMu sync.Mutex
	tNow := time.Unix(5000, 0)
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return tNow
	}
	e := NewEstimator(DefaultConfigs(), clock)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(2)
		base := 300.0 + float64(g)
		go func(base float64) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				clockMu.Lock()
				tNow = tNow.Add(time.Millisecond) // 推进共享时钟，避免未来时间戳
				ts := tNow
				clockMu.Unlock()
				e.Ingest([]Sample{{ChannelVoltage, base, ts}})
			}
		}(base)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_ = e.Readings()
			}
		}()
	}
	wg.Wait()
	r := e.Readings()[ChannelVoltage]
	t.Logf("并发工况结束: voltage valid=%v value=%.1f reason=%q", r.Valid, r.Value, r.Reason)
	if !r.Valid {
		t.Fatalf("并发结束后电压读数应有效, 原因: %q", r.Reason)
	}
}

func TestRejectReasonDistinct(t *testing.T) {
	reasons := []RejectReason{RejectOutOfRange, RejectStale, RejectJump, RejectMissing, RejectBadInterval, RejectUnknownChan}
	seen := map[RejectReason]bool{}
	for _, r := range reasons {
		if seen[r] {
			t.Fatalf("拒绝原因重复: %q", r)
		}
		seen[r] = true
	}
	t.Logf("拒绝原因互异集合: %v", reasons)
}
