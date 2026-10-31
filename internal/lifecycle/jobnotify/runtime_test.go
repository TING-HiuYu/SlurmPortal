package jobnotify

import "testing"

func TestActiveRuntimeSecondsSharedByHookAndNormalizer(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		start, end, suspend, pre int64
		want                     int64
		valid                    bool
	}{
		{"unallocated", 0, 1000, 0, 0, 0, true},
		{"exactly60", 1000, 1060, 0, 0, 60, true},
		{"exactly61", 1000, 1061, 0, 0, 61, true},
		{"paused60", 1000, 2000, 1950, 10, 60, true},
		{"paused61", 1000, 2000, 1950, 11, 61, true},
		{"noAccumulationAcrossAttempts", 2000, 2060, 0, 0, 60, true},
		{"negativeStart", -1, 1000, 0, 0, 0, false},
		{"endBeforeStart", 1000, 999, 0, 0, 0, false},
		{"suspendBeforeStart", 1000, 1061, 999, 0, 0, false},
		{"suspendAfterEnd", 1000, 1061, 1062, 0, 0, false},
		{"preExceedsElapsed", 1000, 2000, 1500, 501, 0, false},
		{"negativeSuspend", 1000, 1061, -1, 0, 0, false},
		{"negativePre", 1000, 1061, 0, -1, 0, false},
		{"unsafeIntegerDuration", 1, 1<<53 + 1, 0, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, valid := activeRuntimeSeconds(tc.start, tc.end, tc.suspend, tc.pre)
			if value != tc.want || valid != tc.valid {
				t.Fatalf("duration=%d valid=%t, expected duration=%d valid=%t", value, valid, tc.want, tc.valid)
			}
		})
	}
}
