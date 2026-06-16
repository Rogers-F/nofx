package kernel

import (
	"testing"
)

// TestLeverageFallback tests automatic correction when leverage exceeds limit
func TestLeverageFallback(t *testing.T) {
	tests := []struct {
		name            string
		decision        Decision
		accountEquity   float64
		btcEthLeverage  int
		altcoinLeverage int
		wantLeverage    int // Expected leverage after correction
		wantError       bool
	}{
		{
			name: "Altcoin leverage exceeded - auto-correct to limit",
			decision: Decision{
				Symbol:          "SOLUSDT",
				Action:          "open_long",
				Leverage:        20, // Exceeds limit
				PositionSizeUSD: 100,
				StopLoss:        50,
				TakeProfit:      200,
			},
			accountEquity:   100,
			btcEthLeverage:  10,
			altcoinLeverage: 5, // Limit 5x
			wantLeverage:    5, // Should be corrected to 5
			wantError:       false,
		},
		{
			name: "BTC leverage exceeded - auto-correct to limit",
			decision: Decision{
				Symbol:          "BTCUSDT",
				Action:          "open_long",
				Leverage:        20, // Exceeds limit
				PositionSizeUSD: 1000,
				StopLoss:        90000,
				TakeProfit:      110000,
			},
			accountEquity:   100,
			btcEthLeverage:  10, // Limit 10x
			altcoinLeverage: 5,
			wantLeverage:    10, // Should be corrected to 10
			wantError:       false,
		},
		{
			name: "Leverage within limit - no correction",
			decision: Decision{
				Symbol:          "ETHUSDT",
				Action:          "open_short",
				Leverage:        5, // Not exceeded
				PositionSizeUSD: 500,
				StopLoss:        4000,
				TakeProfit:      3000,
			},
			accountEquity:   100,
			btcEthLeverage:  10,
			altcoinLeverage: 5,
			wantLeverage:    5, // Stays unchanged
			wantError:       false,
		},
		{
			name: "Leverage is 0 - should error",
			decision: Decision{
				Symbol:          "SOLUSDT",
				Action:          "open_long",
				Leverage:        0, // Invalid
				PositionSizeUSD: 100,
				StopLoss:        50,
				TakeProfit:      200,
			},
			accountEquity:   100,
			btcEthLeverage:  10,
			altcoinLeverage: 5,
			wantLeverage:    0,
			wantError:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use default position value ratios for testing (10x for BTC/ETH, 1.5x for altcoins)
			err := validateDecision(&tt.decision, tt.accountEquity, tt.btcEthLeverage, tt.altcoinLeverage, 10.0, 1.5, false)

			// Check error status
			if (err != nil) != tt.wantError {
				t.Errorf("validateDecision() error = %v, wantError %v", err, tt.wantError)
				return
			}

			// If shouldn't error, check if leverage was correctly corrected
			if !tt.wantError && tt.decision.Leverage != tt.wantLeverage {
				t.Errorf("Leverage not corrected: got %d, want %d", tt.decision.Leverage, tt.wantLeverage)
			}
		})
	}
}

// contains checks if string contains substring (helper function)
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && stringContains(s, substr)))
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestValidateDecision_OversizeClampVsReject verifies Change B2: an oversize open
// is hard-rejected when clampOversize=false (non-GINA, legacy) and clamped down to
// the per-tier cap when clampOversize=true (GINA), so one oversize size cannot fail
// the whole batch.
func TestValidateDecision_OversizeClampVsReject(t *testing.T) {
	// equity 95, altcoin ratio 0.5 → cap 47.5; size 120 is oversize.
	newAlt := func() *Decision {
		return &Decision{Symbol: "XLMUSDT", Action: "open_long", Leverage: 3, PositionSizeUSD: 120, StopLoss: 0.45, TakeProfit: 0.65}
	}

	// Non-GINA: hard reject (legacy behavior preserved).
	if err := validateDecision(newAlt(), 95, 10, 5, 5.0, 0.5, false); err == nil {
		t.Fatal("non-GINA oversize must be rejected")
	}

	// GINA: clamp to cap 47.5, no error.
	alt := newAlt()
	if err := validateDecision(alt, 95, 10, 5, 5.0, 0.5, true); err != nil {
		t.Fatalf("GINA oversize must clamp, got error: %v", err)
	}
	if alt.PositionSizeUSD != 47.5 {
		t.Fatalf("expected clamp to altcoin cap 47.5, got %v", alt.PositionSizeUSD)
	}

	// BTC/ETH tier uses the major ratio: equity 95, btcEth ratio 5 → cap 475.
	btc := &Decision{Symbol: "BTCUSDT", Action: "open_long", Leverage: 3, PositionSizeUSD: 600, StopLoss: 90000, TakeProfit: 110000}
	if err := validateDecision(btc, 95, 10, 5, 5.0, 0.5, true); err != nil {
		t.Fatalf("BTC oversize must clamp, got error: %v", err)
	}
	if btc.PositionSizeUSD != 475 {
		t.Fatalf("expected BTC clamp to 475 (5x95), got %v", btc.PositionSizeUSD)
	}

	// Pathological cap<min: equity 20, altcoin ratio 0.5 → cap 10 < general min 12.
	// validateDecision still clamps to the cap (returns nil, no batch failure); the
	// execution layer (enforceMinPositionSize) is the per-decision backstop that
	// refuses a sub-min order non-fatally.
	small := newAlt()
	if err := validateDecision(small, 20, 10, 5, 5.0, 0.5, true); err != nil {
		t.Fatalf("GINA cap<min must clamp not fail (execution is the min backstop), got error: %v", err)
	}
	if small.PositionSizeUSD != 10 {
		t.Fatalf("expected clamp to cap 10, got %v", small.PositionSizeUSD)
	}
}
