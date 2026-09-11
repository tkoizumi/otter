// Package retry computes retry backoff delays.
//
// The calculation is deliberately deterministic (no jitter) so that operator
// expectations and tests match the manifest exactly:
//
//	attempts: 5, initial_delay: 2s, backoff: exponential
//	→ 2s, 4s, 8s, 16s between attempts
package retry

import (
	"time"

	"github.com/otter-runtime/otter/internal/config"
)

// Delay returns how long to wait before the attempt that follows a failure of
// attempt number failedAttempt (1-based).
func Delay(cfg config.RetryConfig, failedAttempt int) time.Duration {
	if failedAttempt < 1 {
		failedAttempt = 1
	}

	initial := cfg.InitialDelay.Duration()
	maxDelay := cfg.MaxDelay.Duration()

	var d time.Duration
	switch cfg.Backoff {
	case config.BackoffNone:
		d = 0
	case config.BackoffLinear:
		d = multiply(initial, failedAttempt, maxDelay)
	default: // config.BackoffExponential
		d = double(initial, failedAttempt-1, maxDelay)
	}

	if d < 0 {
		d = 0
	}
	if maxDelay > 0 && d > maxDelay {
		d = maxDelay
	}
	return d
}

// ShouldRetry reports whether another attempt is permitted after
// failedAttempt, given the total number of attempts allowed. failedAttempt is
// 1-based; anything lower is treated as the first attempt.
func ShouldRetry(maxAttempts, failedAttempt int) bool {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if failedAttempt < 1 {
		failedAttempt = 1
	}
	return failedAttempt < maxAttempts
}

// Remaining returns how many further attempts are allowed.
func Remaining(maxAttempts, failedAttempt int) int {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if failedAttempt >= maxAttempts {
		return 0
	}
	return maxAttempts - failedAttempt
}

// multiply scales base by factor, saturating at maxDelay to avoid overflow.
func multiply(base time.Duration, factor int, maxDelay time.Duration) time.Duration {
	if factor <= 0 {
		return 0
	}
	if base <= 0 {
		return 0
	}
	if maxDelay > 0 && base >= maxDelay {
		return maxDelay
	}
	d := base
	for i := 1; i < factor; i++ {
		d += base
		if d < 0 || (maxDelay > 0 && d >= maxDelay) {
			return maxDelay
		}
	}
	return d
}

// double multiplies base by 2 shifts times, saturating at maxDelay.
func double(base time.Duration, shifts int, maxDelay time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	if maxDelay > 0 && base >= maxDelay {
		return maxDelay
	}
	d := base
	for i := 0; i < shifts; i++ {
		d *= 2
		if d < 0 || (maxDelay > 0 && d >= maxDelay) {
			return maxDelay
		}
	}
	return d
}
