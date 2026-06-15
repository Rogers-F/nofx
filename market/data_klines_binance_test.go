package market

import "testing"

func TestParseBinanceKlines_Valid(t *testing.T) {
	body := []byte(`[
		[1000, "10.0", "12.0", "9.0", "11.0", "100.0", 1059, "1100.0", 5, "60.0", "660.0", "0"],
		[1060, "11.0", "13.0", "10.5", "12.5", "120.0", 1119, "1500.0", 7, "70.0", "875.0", "0"],
		[1120, "12.5", "12.9", "11.8", "12.0", "90.0", 1179, "1080.0", 4, "40.0", "480.0", "0"]
	]`)
	klines, err := parseBinanceKlines(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(klines) != 3 {
		t.Fatalf("got %d klines, want 3", len(klines))
	}
	// current_price comes from the last close.
	if klines[len(klines)-1].Close != 12.0 {
		t.Fatalf("last close = %v, want 12.0", klines[2].Close)
	}
	if klines[0].OpenTime != 1000 || klines[2].OpenTime != 1120 {
		t.Fatalf("openTimes wrong: %d..%d", klines[0].OpenTime, klines[2].OpenTime)
	}
	if klines[0].CloseTime != 1059 {
		t.Fatalf("closeTime wrong: %d", klines[0].CloseTime)
	}
	// Best-effort enrichment fields.
	if klines[0].Trades != 5 || klines[1].QuoteVolume != 1500.0 {
		t.Fatalf("enrichment wrong: trades=%d qv=%v", klines[0].Trades, klines[1].QuoteVolume)
	}
}

func TestParseBinanceKlines_SortsAscending(t *testing.T) {
	// Rows arrive out of order; the parser must sort ascending by OpenTime so the
	// last element is the most recent candle (the source of current_price).
	body := []byte(`[
		[3000, "12.0", "12.0", "12.0", "12.0", "1.0", 3059, "12.0", 1, "0", "0", "0"],
		[1000, "10.0", "10.0", "10.0", "10.0", "1.0", 1059, "10.0", 1, "0", "0", "0"],
		[2000, "11.0", "11.0", "11.0", "11.0", "1.0", 2059, "11.0", 1, "0", "0", "0"]
	]`)
	klines, err := parseBinanceKlines(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if klines[0].OpenTime != 1000 || klines[1].OpenTime != 2000 || klines[2].OpenTime != 3000 {
		t.Fatalf("not sorted ascending: %d,%d,%d", klines[0].OpenTime, klines[1].OpenTime, klines[2].OpenTime)
	}
	if klines[len(klines)-1].Close != 12.0 {
		t.Fatalf("last close = %v, want 12.0 (openTime 3000)", klines[2].Close)
	}
}

// TestParseBinanceKlines_Rejects covers the fail-closed paths: any malformed row,
// impossible OHLC relationship, or non-finite/zero price rejects the whole series
// so the GINA caller skips the symbol rather than trading on suspect data.
func TestParseBinanceKlines_Rejects(t *testing.T) {
	cases := map[string]string{
		"empty array":      `[]`,
		"malformed json":   `not json`,
		"short row":        `[[1000, "1", "1", "1", "1", "1"]]`,
		"openTime string":  `[["x", "1", "1", "1", "1", "1", 1059]]`,
		"closeTime string": `[[1000, "1", "1", "1", "1", "1", "y"]]`,
		"price not string": `[[1000, 1.0, "1", "1", "1", "1", 1059]]`,
		"zero close":       `[[1000, "1", "1", "1", "0", "1", 1059]]`,
		"negative volume":  `[[1000, "1", "1", "1", "1", "-1", 1059]]`,
		"nan price":        `[[1000, "1", "1", "1", "NaN", "1", 1059]]`,
		"inf high":         `[[1000, "1", "Inf", "1", "1", "1", 1059]]`,
		"high below close": `[[1000, "10", "9", "8", "11", "1", 1059]]`,
		"low above open":   `[[1000, "10", "13", "11", "12", "1", 1059]]`,
		"closeTime<=open":  `[[1000, "1", "1", "1", "1", "1", 1000]]`,
		"dup openTime":     `[[1000,"1","1","1","1","1",1059],[1000,"1","1","1","1","1",1059]]`,
	}
	for name, body := range cases {
		if _, err := parseBinanceKlines([]byte(body)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}
