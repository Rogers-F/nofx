package trader

import "testing"

// TestExistingPositionSide covers the Change-F reverse-open guard core: it reports
// the side of an open position on a symbol so the caller can refuse BOTH a duplicate
// same-side open and a REVERSE open. A reverse open is dangerous because every
// exchange's OpenLong/OpenShort first calls CancelAllOrders(symbol), which would
// strip an existing opposite position's stop-loss/take-profit and leave it naked.
func TestExistingPositionSide(t *testing.T) {
	positions := []map[string]interface{}{
		{"symbol": "BTCUSDT", "side": "long"},
		{"symbol": "ETHUSDT", "side": "short"},
		{"symbol": "SOLUSDT", "side": ""},  // unknown side → treated as flat
		{"symbol": "ADAUSDT", "side": nil}, // non-string side → treated as flat
	}

	cases := []struct {
		name   string
		symbol string
		want   string
	}{
		{"long held → open_short would reverse (sees long)", "BTCUSDT", "long"},
		{"short held → open_long would reverse (sees short)", "ETHUSDT", "short"},
		{"empty side ignored", "SOLUSDT", ""},
		{"nil side ignored", "ADAUSDT", ""},
		{"flat symbol", "XRPUSDT", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := existingPositionSide(positions, c.symbol); got != c.want {
				t.Fatalf("existingPositionSide(%s) = %q, want %q", c.symbol, got, c.want)
			}
		})
	}
}
