package market

import "math"

// calculateEMA calculates EMA
func calculateEMA(klines []Kline, period int) float64 {
	if len(klines) < period {
		return 0
	}

	// Calculate SMA as initial EMA
	sum := 0.0
	for i := 0; i < period; i++ {
		sum += klines[i].Close
	}
	ema := sum / float64(period)

	// Calculate EMA
	multiplier := 2.0 / float64(period+1)
	for i := period; i < len(klines); i++ {
		ema = (klines[i].Close-ema)*multiplier + ema
	}

	return ema
}

// calculateMACD calculates MACD
func calculateMACD(klines []Kline) float64 {
	if len(klines) < 26 {
		return 0
	}

	// Calculate 12-period and 26-period EMA
	ema12 := calculateEMA(klines, 12)
	ema26 := calculateEMA(klines, 26)

	// MACD = EMA12 - EMA26
	return ema12 - ema26
}

// calculateRSI calculates RSI
func calculateRSI(klines []Kline, period int) float64 {
	if len(klines) <= period {
		return 0
	}

	gains := 0.0
	losses := 0.0

	// Calculate initial average gain/loss
	for i := 1; i <= period; i++ {
		change := klines[i].Close - klines[i-1].Close
		if change > 0 {
			gains += change
		} else {
			losses += -change
		}
	}

	avgGain := gains / float64(period)
	avgLoss := losses / float64(period)

	// Use Wilder smoothing method to calculate subsequent RSI
	for i := period + 1; i < len(klines); i++ {
		change := klines[i].Close - klines[i-1].Close
		if change > 0 {
			avgGain = (avgGain*float64(period-1) + change) / float64(period)
			avgLoss = (avgLoss * float64(period-1)) / float64(period)
		} else {
			avgGain = (avgGain * float64(period-1)) / float64(period)
			avgLoss = (avgLoss*float64(period-1) + (-change)) / float64(period)
		}
	}

	if avgLoss == 0 {
		return 100
	}

	rs := avgGain / avgLoss
	rsi := 100 - (100 / (1 + rs))

	return rsi
}

// calculateATR calculates ATR
func calculateATR(klines []Kline, period int) float64 {
	if len(klines) <= period {
		return 0
	}

	trs := make([]float64, len(klines))
	for i := 1; i < len(klines); i++ {
		high := klines[i].High
		low := klines[i].Low
		prevClose := klines[i-1].Close

		tr1 := high - low
		tr2 := math.Abs(high - prevClose)
		tr3 := math.Abs(low - prevClose)

		trs[i] = math.Max(tr1, math.Max(tr2, tr3))
	}

	// Calculate initial ATR
	sum := 0.0
	for i := 1; i <= period; i++ {
		sum += trs[i]
	}
	atr := sum / float64(period)

	// Wilder smoothing
	for i := period + 1; i < len(klines); i++ {
		atr = (atr*float64(period-1) + trs[i]) / float64(period)
	}

	return atr
}

// calculateBOLL calculates Bollinger Bands (upper, middle, lower)
// period: typically 20, multiplier: typically 2
func calculateBOLL(klines []Kline, period int, multiplier float64) (upper, middle, lower float64) {
	if len(klines) < period {
		return 0, 0, 0
	}

	// Calculate SMA (middle band)
	sum := 0.0
	for i := len(klines) - period; i < len(klines); i++ {
		sum += klines[i].Close
	}
	sma := sum / float64(period)

	// Calculate standard deviation
	variance := 0.0
	for i := len(klines) - period; i < len(klines); i++ {
		diff := klines[i].Close - sma
		variance += diff * diff
	}
	stdDev := math.Sqrt(variance / float64(period))

	// Calculate bands
	middle = sma
	upper = sma + multiplier*stdDev
	lower = sma - multiplier*stdDev

	return upper, middle, lower
}

// calculateBOLLMiddleMomentum derives the normalized slope and acceleration of
// the Bollinger middle band from its value series. The result is expressed as a
// percentage of the middle band so it is comparable across symbols of different
// price scales. Returned slices are aligned with the input (same length); the
// first slope and the first two acceleration entries are zero because they have
// no predecessor. Callers that need a closed-candle reading should use the
// second-to-last entry when the latest candle may still be forming.
func calculateBOLLMiddleMomentum(middle []float64) (slope, accel []float64) {
	n := len(middle)
	slope = make([]float64, n)
	accel = make([]float64, n)
	if n < 2 {
		return slope, accel
	}
	for i := 1; i < n; i++ {
		prev := middle[i-1]
		if prev == 0 {
			slope[i] = 0
			continue
		}
		slope[i] = (middle[i] - prev) / prev * 100
	}
	for i := 2; i < n; i++ {
		accel[i] = slope[i] - slope[i-1]
	}
	return slope, accel
}

// calculateDonchian calculates Donchian channel (highest high, lowest low) for given period
func calculateDonchian(klines []Kline, period int) (upper, lower float64) {
	if len(klines) == 0 || period <= 0 {
		return 0, 0
	}

	// Use all available klines if period > len(klines)
	start := len(klines) - period
	if start < 0 {
		start = 0
	}

	upper = klines[start].High
	lower = klines[start].Low

	for i := start + 1; i < len(klines); i++ {
		if klines[i].High > upper {
			upper = klines[i].High
		}
		if klines[i].Low < lower {
			lower = klines[i].Low
		}
	}

	return upper, lower
}

// Box period constants (in 1h candles)
const (
	ShortBoxPeriod = 72  // 3 days of 1h candles
	MidBoxPeriod   = 240 // 10 days of 1h candles
	LongBoxPeriod  = 500 // ~21 days of 1h candles
)

// calculateBoxData calculates multi-period box data from klines
func calculateBoxData(klines []Kline, currentPrice float64) *BoxData {
	box := &BoxData{
		CurrentPrice: currentPrice,
	}

	if len(klines) == 0 {
		return box
	}

	box.ShortUpper, box.ShortLower = calculateDonchian(klines, ShortBoxPeriod)
	box.MidUpper, box.MidLower = calculateDonchian(klines, MidBoxPeriod)
	box.LongUpper, box.LongLower = calculateDonchian(klines, LongBoxPeriod)

	return box
}

// ========== Exported indicator calculation functions (for testing) ==========

// ExportCalculateEMA exports calculateEMA for testing
func ExportCalculateEMA(klines []Kline, period int) float64 {
	return calculateEMA(klines, period)
}

// ExportCalculateMACD exports calculateMACD for testing
func ExportCalculateMACD(klines []Kline) float64 {
	return calculateMACD(klines)
}

// ExportCalculateRSI exports calculateRSI for testing
func ExportCalculateRSI(klines []Kline, period int) float64 {
	return calculateRSI(klines, period)
}

// ExportCalculateATR exports calculateATR for testing
func ExportCalculateATR(klines []Kline, period int) float64 {
	return calculateATR(klines, period)
}

// ExportCalculateBOLL exports calculateBOLL for testing
func ExportCalculateBOLL(klines []Kline, period int, multiplier float64) (upper, middle, lower float64) {
	return calculateBOLL(klines, period, multiplier)
}

// ExportCalculateDonchian exports calculateDonchian for testing
func ExportCalculateDonchian(klines []Kline, period int) (float64, float64) {
	return calculateDonchian(klines, period)
}

// ExportCalculateBoxData exports calculateBoxData for testing
func ExportCalculateBoxData(klines []Kline, currentPrice float64) *BoxData {
	return calculateBoxData(klines, currentPrice)
}
