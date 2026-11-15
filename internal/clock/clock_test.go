package clock

import (
	"testing"
	"time"
)

func TestFake(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f := NewFake(start)
	if got := f.Now(); !got.Equal(start) {
		t.Fatalf("Now() = %v, want %v", got, start)
	}
	f.Advance(3 * time.Second)
	if got := f.Now(); !got.Equal(start.Add(3 * time.Second)) {
		t.Fatalf("after Advance Now() = %v", got)
	}
	next := start.Add(time.Hour)
	f.Set(next)
	if got := f.Now(); !got.Equal(next) {
		t.Fatalf("after Set Now() = %v", got)
	}
}

func TestReal(t *testing.T) {
	before := time.Now()
	got := (Real{}).Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("Real.Now() = %v outside [%v, %v]", got, before, after)
	}
}
