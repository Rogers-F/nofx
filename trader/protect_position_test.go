package trader

import (
	"errors"
	"testing"
	"time"
)

// fakeProtectTrader is a minimal Trader used to exercise protectOpenPosition's
// stop-loss confirmation / rollback branches. Only the methods that path touches
// carry behavior; the rest are inert stubs.
type fakeProtectTrader struct {
	setSLErr      error
	openOrders    []OpenOrder
	openOrdersErr error

	closeLongCalled  bool
	closeShortCalled bool
	cancelStopCalled bool
}

func (f *fakeProtectTrader) GetBalance() (map[string]interface{}, error) { return nil, nil }
func (f *fakeProtectTrader) GetPositions() ([]map[string]interface{}, error) {
	return nil, nil // empty -> protectOpenPosition falls back to the estimate
}
func (f *fakeProtectTrader) OpenLong(string, float64, int) (map[string]interface{}, error) {
	return nil, nil
}
func (f *fakeProtectTrader) OpenShort(string, float64, int) (map[string]interface{}, error) {
	return nil, nil
}
func (f *fakeProtectTrader) CloseLong(string, float64) (map[string]interface{}, error) {
	f.closeLongCalled = true
	return map[string]interface{}{}, nil
}
func (f *fakeProtectTrader) CloseShort(string, float64) (map[string]interface{}, error) {
	f.closeShortCalled = true
	return map[string]interface{}{}, nil
}
func (f *fakeProtectTrader) SetLeverage(string, int) error          { return nil }
func (f *fakeProtectTrader) SetMarginMode(string, bool) error       { return nil }
func (f *fakeProtectTrader) GetMarketPrice(string) (float64, error) { return 0, nil }
func (f *fakeProtectTrader) SetStopLoss(string, string, float64, float64) error {
	return f.setSLErr
}
func (f *fakeProtectTrader) SetTakeProfit(string, string, float64, float64) error { return nil }
func (f *fakeProtectTrader) CancelStopLossOrders(string) error                    { return nil }
func (f *fakeProtectTrader) CancelTakeProfitOrders(string) error                  { return nil }
func (f *fakeProtectTrader) CancelAllOrders(string) error                         { return nil }
func (f *fakeProtectTrader) CancelStopOrders(string) error {
	f.cancelStopCalled = true
	return nil
}
func (f *fakeProtectTrader) FormatQuantity(string, float64) (string, error) { return "", nil }
func (f *fakeProtectTrader) GetOrderStatus(string, string) (map[string]interface{}, error) {
	return nil, nil
}
func (f *fakeProtectTrader) GetClosedPnL(time.Time, int) ([]ClosedPnLRecord, error) {
	return nil, nil
}
func (f *fakeProtectTrader) GetOpenOrders(string) ([]OpenOrder, error) {
	return f.openOrders, f.openOrdersErr
}

func withFastStopLossRetry(t *testing.T) {
	t.Helper()
	origDelay := stopLossRetryDelay
	stopLossRetryDelay = time.Millisecond
	t.Cleanup(func() { stopLossRetryDelay = origDelay })
}

func TestProtect_SLConfirmedNoRollback(t *testing.T) {
	withFastStopLossRetry(t)
	fake := &fakeProtectTrader{
		setSLErr:   nil,
		openOrders: []OpenOrder{{Type: "STOP_MARKET", PositionSide: "LONG", StopPrice: 99}},
	}
	at := &AutoTrader{trader: fake}

	if err := at.protectOpenPosition("BTCUSDT", "LONG", 1.0, 99, 110); err != nil {
		t.Fatalf("expected nil error when stop-loss confirmed, got %v", err)
	}
	if fake.closeLongCalled {
		t.Fatalf("must not roll back when stop-loss is confirmed")
	}
	if halted, _ := at.protectionHaltState(); halted {
		t.Fatalf("must not halt when stop-loss is confirmed")
	}
}

func TestProtect_SLNilButMissingRollsBack(t *testing.T) {
	withFastStopLossRetry(t)
	// SetStopLoss reports success (nil) but the order is not actually present.
	fake := &fakeProtectTrader{setSLErr: nil, openOrders: nil}
	at := &AutoTrader{trader: fake}

	err := at.protectOpenPosition("BTCUSDT", "LONG", 1.0, 99, 110)
	if err == nil {
		t.Fatalf("expected rollback error when SL reported nil but is absent")
	}
	if !fake.closeLongCalled {
		t.Fatalf("must close (roll back) the unprotected position")
	}
	if !fake.cancelStopCalled {
		t.Fatalf("must cancel residual protective orders during rollback")
	}
	halted, _ := at.protectionHaltState()
	if !halted {
		t.Fatalf("must raise the protection halt after a rollback")
	}
	// Halt blocks subsequent opens.
	if gerr := at.assertOpenAllowed(); gerr == nil {
		t.Fatalf("protection halt must block new opens")
	}
}

func TestProtect_QueryFailTrustsPlacement(t *testing.T) {
	withFastStopLossRetry(t)
	// Placement succeeded but we cannot verify (open-order query fails). On an
	// exchange whose query does not surface stop orders, we must trust the
	// successful placement rather than churn-close the position.
	fake := &fakeProtectTrader{setSLErr: nil, openOrdersErr: errors.New("not supported")}
	at := &AutoTrader{trader: fake}

	if err := at.protectOpenPosition("BTCUSDT", "LONG", 1.0, 99, 110); err != nil {
		t.Fatalf("expected nil (trust placement) when query fails and SL placement succeeded, got %v", err)
	}
	if fake.closeLongCalled {
		t.Fatalf("must not roll back when placement succeeded and query is unavailable")
	}
}

func TestProtect_SLErrorAndAbsentRollsBack(t *testing.T) {
	withFastStopLossRetry(t)
	// Placement errored AND the order is absent -> rollback.
	fake := &fakeProtectTrader{setSLErr: errors.New("rejected"), openOrders: nil}
	at := &AutoTrader{trader: fake}

	if err := at.protectOpenPosition("ETHUSDT", "SHORT", 1.0, 110, 90); err == nil {
		t.Fatalf("expected rollback error when SL errored and is absent")
	}
	if !fake.closeShortCalled {
		t.Fatalf("must close the short position on rollback")
	}
}
