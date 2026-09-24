package sensor

import (
	"sync"
	"testing"
	"time"
)

func logVerdict(t *testing.T, name string, s Sample, v Verdict) {
	t.Helper()
	t.Logf("[%s] input={ch=%s val=%.3f ts=%s seq=%d} accept=%v reason=%q detail=%s",
		name, s.Channel, s.Value, s.Timestamp.Format(time.RFC3339Nano), s.Seq,
		v.Accept, v.Reason, v.Detail)
}

func TestValidatorAcceptReasons(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	v := NewValidator(DefaultChannelSpecs())

	cases := []struct {
		name   string
		s      Sample
		reason RejectReason
		accept bool
	}{
		{"first_valid", Sample{ChannelVoltage, 360, now, 1}, ReasonOK, true},
		{"in_range", Sample{ChannelVoltage, 370, now.Add(5 * time.Millisecond), 2}, ReasonOK, true},
		{"out_of_range_high", Sample{ChannelVoltage, 500, now.Add(10 * time.Millisecond), 3}, ReasonOutOfRange, false},
		{"out_of_range_low", Sample{ChannelVoltage, 100, now.Add(15 * time.Millisecond), 4}, ReasonOutOfRange, false},
		{"stale", Sample{ChannelVoltage, 365, now.Add(-1 * time.Second), 5}, ReasonStale, false},
		{"future", Sample{ChannelVoltage, 365, now.Add(1 * time.Second), 6}, ReasonFuture, false},
		{"jump", Sample{ChannelVoltage, 420, now.Add(20 * time.Millisecond), 7}, ReasonJump, false},
		{"bad_channel", Sample{Channel("humidity"), 50, now.Add(25 * time.Millisecond), 8}, ReasonBadChannel, false},
		{"seq_first", Sample{ChannelCurrent, 10, now, 1}, ReasonOK, true},
		{"seq_regressed", Sample{ChannelCurrent, 12, now.Add(5 * time.Millisecond), 1}, ReasonJump, false},
	}

	for _, c := range cases {
		got := v.Validate(c.s, now)
		logVerdict(t, c.name, c.s, got)
		if got.Accept != c.accept {
			t.Fatalf("%s: accept=%v want %v (%s)", c.name, got.Accept, c.accept, got.Detail)
		}
		if !c.accept && got.Reason != c.reason {
			t.Fatalf("%s: reason=%q want %q", c.name, got.Reason, c.reason)
		}
	}
}

func TestValidatorRangeBoundaries(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	// 量程端点应合法；在独立 validator 上各自验证首帧，避免相邻大跳变干扰。
	for _, val := range []float64{-40, 105} {
		v := NewValidator(map[Channel]ChannelSpec{
			ChannelTemp: {Min: -40, Max: 105, MaxStep: 15, StaleAge: 2 * time.Second, HasJump: true},
		})
		s := Sample{ChannelTemp, val, now, 1}
		got := v.Validate(s, now)
		logVerdict(t, "range_boundary", s, got)
		if !got.Accept {
			t.Fatalf("boundary value %.1f rejected: %s", val, got.Detail)
		}
	}
	// 端点之外必须拒绝。
	v := NewValidator(map[Channel]ChannelSpec{
		ChannelTemp: {Min: -40, Max: 105, MaxStep: 1000, StaleAge: 2 * time.Second, HasJump: true},
	})
	v.Validate(Sample{ChannelTemp, 20, now, 1}, now)
	for _, val := range []float64{-40.01, 105.01} {
		got := v.Validate(Sample{ChannelTemp, val, now.Add(time.Millisecond), 2}, now)
		logVerdict(t, "range_outside", got.Sample, got)
		if got.Accept || got.Reason != ReasonOutOfRange {
			t.Fatalf("value %.2f should be out_of_range, got accept=%v reason=%q", val, got.Accept, got.Reason)
		}
	}
}

func TestValidatorFreshness(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	v := NewValidator(DefaultChannelSpecs())

	missing := v.Freshness(t0)
	if len(missing) != 4 {
		t.Fatalf("expected 4 missing channels, got %d", len(missing))
	}
	for _, vd := range missing {
		t.Logf("[freshness-missing] ch=%s reason=%q detail=%s", vd.Sample.Channel, vd.Reason, vd.Detail)
		if vd.Reason != ReasonMissing {
			t.Fatalf("expected missing, got %q", vd.Reason)
		}
	}

	v.Validate(Sample{ChannelVoltage, 360, t0, 1}, t0)
	stale := v.Freshness(t0.Add(1 * time.Second))
	var found bool
	for _, vd := range stale {
		if vd.Sample.Channel == ChannelVoltage && vd.Reason == ReasonStale {
			found = true
			t.Logf("[freshness-stale] detail=%s", vd.Detail)
		}
	}
	if !found {
		t.Fatal("expected voltage stale verdict")
	}
}

func TestValidatorConcurrentSafety(t *testing.T) {
	// 每个 goroutine 独占一个通道、时间戳与 seq 单调，馈入彼此不冲突；
	// 该用例主要在 -race 下验证内部状态无数据竞争，且结果对同一馈入可复现。
	run := func() int {
		now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
		v := NewValidator(DefaultChannelSpecs())
		chans := []Channel{ChannelVoltage, ChannelCurrent, ChannelTemp, ChannelRPM}
		// 各通道取量程中部的基准值，避免量程拒绝干扰并发安全验证。
		base := map[Channel]float64{
			ChannelVoltage: 350, ChannelCurrent: 50, ChannelTemp: 40, ChannelRPM: 3000,
		}
		var wg sync.WaitGroup
		accepted := make([]int, len(chans))
		for w, ch := range chans {
			wg.Add(1)
			go func(w int, ch Channel) {
				defer wg.Done()
				for i := 0; i < 100; i++ {
					ts := now.Add(time.Duration(i) * time.Millisecond)
					s := Sample{ch, base[ch] + float64(i%3), ts, int64(i + 1)}
					if vd := v.Validate(s, ts); vd.Accept {
						accepted[w]++
					}
				}
			}(w, ch)
		}
		wg.Wait()
		total := 0
		for _, n := range accepted {
			total += n
		}
		return total
	}
	a, b := run(), run()
	t.Logf("[concurrent] accepted run1=%d run2=%d", a, b)
	if a != b {
		t.Fatalf("non-conflicting concurrent feeds should be reproducible: %d vs %d", a, b)
	}
	if a != 400 {
		t.Fatalf("expected all 400 samples accepted, got %d", a)
	}
}
