package market

import (
	"math"
	"testing"
)

func TestCalculateBOLLMiddleMomentum_Normalized(t *testing.T) {
	// Middle band rising then accelerating.
	middle := []float64{100, 101, 103} // +1%, then +~1.98%
	slope, accel := calculateBOLLMiddleMomentum(middle)

	if len(slope) != 3 || len(accel) != 3 {
		t.Fatalf("expected aligned slices of len 3, got slope=%d accel=%d", len(slope), len(accel))
	}
	// First entries have no predecessor.
	if slope[0] != 0 || accel[0] != 0 || accel[1] != 0 {
		t.Fatalf("leading entries must be zero: slope0=%v accel0=%v accel1=%v", slope[0], accel[0], accel[1])
	}
	wantSlope1 := (101.0 - 100.0) / 100.0 * 100 // 1.0
	if math.Abs(slope[1]-wantSlope1) > 1e-9 {
		t.Fatalf("slope[1]=%v want %v", slope[1], wantSlope1)
	}
	wantSlope2 := (103.0 - 101.0) / 101.0 * 100 // ~1.9802
	if math.Abs(slope[2]-wantSlope2) > 1e-9 {
		t.Fatalf("slope[2]=%v want %v", slope[2], wantSlope2)
	}
	if accel[2] <= 0 {
		t.Fatalf("acceleration should be positive when slope increases, got %v", accel[2])
	}
}

func TestCalculateBOLLMiddleMomentum_ZeroAndShort(t *testing.T) {
	if s, a := calculateBOLLMiddleMomentum(nil); len(s) != 0 || len(a) != 0 {
		t.Fatalf("nil input should give empty slices")
	}
	if s, a := calculateBOLLMiddleMomentum([]float64{42}); len(s) != 1 || len(a) != 1 || s[0] != 0 || a[0] != 0 {
		t.Fatalf("single element should give zeroed slices")
	}
	// Zero previous value must not divide by zero.
	slope, _ := calculateBOLLMiddleMomentum([]float64{0, 5})
	if slope[1] != 0 {
		t.Fatalf("zero predecessor must yield zero slope, got %v", slope[1])
	}
}
