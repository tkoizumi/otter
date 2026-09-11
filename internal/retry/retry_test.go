package retry

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/otter-runtime/otter/internal/config"
)

func exponentialConfig(initial, max time.Duration) config.RetryConfig {
	return config.RetryConfig{
		Backoff:      config.BackoffExponential,
		InitialDelay: config.Duration(initial),
		MaxDelay:     config.Duration(max),
	}
}

func linearConfig(initial, max time.Duration) config.RetryConfig {
	return config.RetryConfig{
		Backoff:      config.BackoffLinear,
		InitialDelay: config.Duration(initial),
		MaxDelay:     config.Duration(max),
	}
}

func TestDelayExponential(t *testing.T) {
	cfg := exponentialConfig(2*time.Second, 60*time.Second)
	tests := []struct {
		failedAttempt int
		want          time.Duration
	}{
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 32 * time.Second},
		{6, 60 * time.Second}, // 64s would exceed the 60s cap
		{10, 60 * time.Second},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("attempt_%d", tc.failedAttempt), func(t *testing.T) {
			if got := Delay(cfg, tc.failedAttempt); got != tc.want {
				t.Errorf("Delay(exponential, %d) = %s, want %s", tc.failedAttempt, got, tc.want)
			}
		})
	}
}

func TestDelayLinear(t *testing.T) {
	cfg := linearConfig(2*time.Second, 60*time.Second)
	tests := []struct {
		failedAttempt int
		want          time.Duration
	}{
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 6 * time.Second},
		{4, 8 * time.Second},
		{100, 60 * time.Second},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("attempt_%d", tc.failedAttempt), func(t *testing.T) {
			if got := Delay(cfg, tc.failedAttempt); got != tc.want {
				t.Errorf("Delay(linear, %d) = %s, want %s", tc.failedAttempt, got, tc.want)
			}
		})
	}
}

func TestDelayBackoffNone(t *testing.T) {
	cfg := config.RetryConfig{
		Backoff:      config.BackoffNone,
		InitialDelay: config.Duration(2 * time.Second),
		MaxDelay:     config.Duration(60 * time.Second),
	}
	for _, attempt := range []int{1, 2, 3, 5, 10} {
		t.Run(fmt.Sprintf("attempt_%d", attempt), func(t *testing.T) {
			if got := Delay(cfg, attempt); got != 0 {
				t.Errorf("Delay(none, %d) = %s, want 0", attempt, got)
			}
		})
	}
}

func TestDelaySaturatesWithoutOverflow(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.RetryConfig
	}{
		{"exponential initial above max", exponentialConfig(time.Hour, time.Second)},
		{"linear initial above max", linearConfig(time.Hour, time.Second)},
		{"exponential huge initial", exponentialConfig(time.Duration(math.MaxInt64/2), time.Second)},
		{"linear huge initial", linearConfig(time.Duration(math.MaxInt64/2), time.Second)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, attempt := range []int{1, 2, 3, 10, 100} {
				got := Delay(tc.cfg, attempt)
				if got != time.Second {
					t.Errorf("Delay(%s, %d) = %s, want exactly max_delay 1s", tc.name, attempt, got)
				}
				if got < 0 {
					t.Errorf("Delay(%s, %d) = %s, want a non-negative delay", tc.name, attempt, got)
				}
			}
		})
	}
}

func TestDelayAttemptZeroBehavesLikeFirstAttempt(t *testing.T) {
	cfgs := []struct {
		name string
		cfg  config.RetryConfig
	}{
		{"exponential", exponentialConfig(2*time.Second, 60*time.Second)},
		{"linear", linearConfig(2*time.Second, 60*time.Second)},
		{"none", config.RetryConfig{Backoff: config.BackoffNone, InitialDelay: config.Duration(2 * time.Second), MaxDelay: config.Duration(60 * time.Second)}},
	}
	for _, tc := range cfgs {
		t.Run(tc.name, func(t *testing.T) {
			zero := Delay(tc.cfg, 0)
			one := Delay(tc.cfg, 1)
			if zero != one {
				t.Errorf("Delay(cfg, 0) = %s, want the attempt-1 value %s", zero, one)
			}
		})
	}
}

func TestShouldRetry(t *testing.T) {
	tests := []struct {
		maxAttempts   int
		failedAttempt int
		want          bool
	}{
		{0, 1, false},
		{0, 2, false},
		{1, 1, false},
		{1, 2, false},
		{2, 1, true},
		{2, 2, false},
		{3, 1, true},
		{3, 2, true},
		{3, 3, false},
		{3, 4, false},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("max%d_after%d", tc.maxAttempts, tc.failedAttempt), func(t *testing.T) {
			if got := ShouldRetry(tc.maxAttempts, tc.failedAttempt); got != tc.want {
				t.Errorf("ShouldRetry(%d, %d) = %v, want %v",
					tc.maxAttempts, tc.failedAttempt, got, tc.want)
			}
		})
	}
}

func TestRemaining(t *testing.T) {
	tests := []struct {
		maxAttempts   int
		failedAttempt int
		want          int
	}{
		{3, 1, 2},
		{3, 2, 1},
		{3, 3, 0},
		{3, 4, 0},
		{1, 1, 0},
		{0, 1, 0},
		{5, 0, 5},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("max%d_after%d", tc.maxAttempts, tc.failedAttempt), func(t *testing.T) {
			if got := Remaining(tc.maxAttempts, tc.failedAttempt); got != tc.want {
				t.Errorf("Remaining(%d, %d) = %d, want %d",
					tc.maxAttempts, tc.failedAttempt, got, tc.want)
			}
		})
	}
}
