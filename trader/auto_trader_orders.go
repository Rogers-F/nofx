package trader

import (
	"fmt"
	"math"
	"nofx/kernel"
	"nofx/logger"
	"nofx/market"
	"nofx/store"
	"strings"
	"time"
)

const (
	// marginOverheadFactor and takerFeeRate approximate the total funds an
	// exchange reserves when opening a position:
	// totalRequired ≈ positionSize/leverage + positionSize*takerFeeRate + positionSize/leverage*1%
	//              = positionSize * (marginOverheadFactor/leverage + takerFeeRate)
	marginOverheadFactor = 1.01
	takerFeeRate         = 0.001

	// positionSizeSafetyFactor leaves a buffer below the maximum affordable
	// position size so a price move between sizing and execution cannot
	// trigger an insufficient-margin rejection.
	positionSizeSafetyFactor = 0.98
)

// stopLossConfirmRetries bounds how many extra times a freshly opened position
// re-attempts stop-loss placement before it is rolled back. stopLossRetryDelay
// is the pause between confirmation attempts. They are vars (not consts) only so
// tests can shorten the delay.
var (
	stopLossConfirmRetries = 2
	stopLossRetryDelay     = 1500 * time.Millisecond
)

// executeDecisionWithRecord executes AI decision and records detailed information
func (at *AutoTrader) executeDecisionWithRecord(decision *kernel.Decision, actionRecord *store.DecisionAction) error {
	switch decision.Action {
	case "open_long":
		return at.executeOpenLongWithRecord(decision, actionRecord)
	case "open_short":
		return at.executeOpenShortWithRecord(decision, actionRecord)
	case "close_long":
		return at.executeCloseLongWithRecord(decision, actionRecord)
	case "close_short":
		return at.executeCloseShortWithRecord(decision, actionRecord)
	case "hold", "wait":
		// No execution needed, just record
		return nil
	default:
		return fmt.Errorf("unknown action: %s", decision.Action)
	}
}

// executeOpenLongWithRecord executes open long position and records detailed information
func (at *AutoTrader) executeOpenLongWithRecord(decision *kernel.Decision, actionRecord *store.DecisionAction) error {
	logger.Infof("  📈 Open long: %s", decision.Symbol)

	// Refuse new opens while a protection halt is active (a prior open could not
	// be protected by a confirmed stop-loss). Close/maintenance paths are not
	// gated, so existing positions can still be managed.
	if err := at.assertOpenAllowed(); err != nil {
		return err
	}

	// ⚠️ Get current positions for multiple checks
	positions, err := at.trader.GetPositions()
	if err != nil {
		return fmt.Errorf("failed to get positions: %w", err)
	}

	// [CODE ENFORCED] Check max positions limit
	if err := at.enforceMaxPositions(len(positions)); err != nil {
		return err
	}

	// Check if there's already a position in the same symbol and direction
	for _, pos := range positions {
		if pos["symbol"] == decision.Symbol && pos["side"] == "long" {
			return fmt.Errorf("❌ %s already has long position, close it first", decision.Symbol)
		}
	}

	// Get current price
	marketData, err := market.GetWithExchange(decision.Symbol, at.exchange)
	if err != nil {
		return fmt.Errorf("failed to get market data for %s: %w", decision.Symbol, err)
	}

	// Get balance (needed for multiple checks)
	balance, err := at.trader.GetBalance()
	if err != nil {
		return fmt.Errorf("failed to get account balance: %w", err)
	}
	availableBalance := 0.0
	if avail, ok := balance["availableBalance"].(float64); ok {
		availableBalance = avail
	}

	// Get equity for position value ratio check
	equity := 0.0
	if eq, ok := balance["totalEquity"].(float64); ok && eq > 0 {
		equity = eq
	} else if eq, ok := balance["totalWalletBalance"].(float64); ok && eq > 0 {
		equity = eq
	} else {
		equity = availableBalance // Fallback to available balance
	}

	// [CODE ENFORCED] Position Value Ratio Check: position_value <= equity × ratio
	adjustedPositionSize, wasCapped := at.enforcePositionValueRatio(decision.PositionSizeUSD, equity, decision.Symbol)
	if wasCapped {
		decision.PositionSizeUSD = adjustedPositionSize
	}

	// ⚠️ Auto-adjust position size if insufficient margin
	marginFactor := marginOverheadFactor/float64(decision.Leverage) + takerFeeRate
	maxAffordablePositionSize := availableBalance / marginFactor

	actualPositionSize := decision.PositionSizeUSD
	if actualPositionSize > maxAffordablePositionSize {
		adjustedSize := maxAffordablePositionSize * positionSizeSafetyFactor
		logger.Infof("  ⚠️ Position size %.2f exceeds max affordable %.2f, auto-reducing to %.2f",
			actualPositionSize, maxAffordablePositionSize, adjustedSize)
		actualPositionSize = adjustedSize
		decision.PositionSizeUSD = actualPositionSize
	}

	// [CODE ENFORCED] Minimum position size check
	if err := at.enforceMinPositionSize(decision.PositionSizeUSD); err != nil {
		return err
	}

	// Calculate quantity with adjusted position size
	quantity := actualPositionSize / marketData.CurrentPrice
	actionRecord.Quantity = quantity
	actionRecord.Price = marketData.CurrentPrice

	// Set margin mode
	if err := at.trader.SetMarginMode(decision.Symbol, at.config.IsCrossMargin); err != nil {
		logger.Infof("  ⚠️ Failed to set margin mode: %v", err)
		// Continue execution, doesn't affect trading
	}

	// Open position
	order, err := at.trader.OpenLong(decision.Symbol, quantity, decision.Leverage)
	if err != nil {
		return fmt.Errorf("failed to open long position for %s: %w", decision.Symbol, err)
	}

	// Record order ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	logger.Infof("  ✓ Position opened successfully, order ID: %v, quantity: %.4f", order["orderId"], quantity)

	// Record order to database and poll for confirmation
	at.recordAndConfirmOrder(order, decision.Symbol, "open_long", quantity, marketData.CurrentPrice, decision.Leverage, 0)

	// Record position opening time
	posKey := decision.Symbol + "_long"
	at.positionFirstSeenTime[posKey] = time.Now().UnixMilli()

	// Set stop loss (mandatory) and take profit. If the stop-loss cannot be
	// confirmed on the exchange, the position is rolled back and an error is
	// returned so it can never be left open without protection.
	if err := at.protectOpenPosition(decision.Symbol, "LONG", quantity, decision.StopLoss, decision.TakeProfit); err != nil {
		return err
	}

	return nil
}

// executeOpenShortWithRecord executes open short position and records detailed information
func (at *AutoTrader) executeOpenShortWithRecord(decision *kernel.Decision, actionRecord *store.DecisionAction) error {
	logger.Infof("  📉 Open short: %s", decision.Symbol)

	// Refuse new opens while a protection halt is active (a prior open could not
	// be protected by a confirmed stop-loss). Close/maintenance paths are not
	// gated, so existing positions can still be managed.
	if err := at.assertOpenAllowed(); err != nil {
		return err
	}

	// ⚠️ Get current positions for multiple checks
	positions, err := at.trader.GetPositions()
	if err != nil {
		return fmt.Errorf("failed to get positions: %w", err)
	}

	// [CODE ENFORCED] Check max positions limit
	if err := at.enforceMaxPositions(len(positions)); err != nil {
		return err
	}

	// Check if there's already a position in the same symbol and direction
	for _, pos := range positions {
		if pos["symbol"] == decision.Symbol && pos["side"] == "short" {
			return fmt.Errorf("❌ %s already has short position, close it first", decision.Symbol)
		}
	}

	// Get current price
	marketData, err := market.GetWithExchange(decision.Symbol, at.exchange)
	if err != nil {
		return fmt.Errorf("failed to get market data for %s: %w", decision.Symbol, err)
	}

	// Get balance (needed for multiple checks)
	balance, err := at.trader.GetBalance()
	if err != nil {
		return fmt.Errorf("failed to get account balance: %w", err)
	}
	availableBalance := 0.0
	if avail, ok := balance["availableBalance"].(float64); ok {
		availableBalance = avail
	}

	// Get equity for position value ratio check
	equity := 0.0
	if eq, ok := balance["totalEquity"].(float64); ok && eq > 0 {
		equity = eq
	} else if eq, ok := balance["totalWalletBalance"].(float64); ok && eq > 0 {
		equity = eq
	} else {
		equity = availableBalance // Fallback to available balance
	}

	// [CODE ENFORCED] Position Value Ratio Check: position_value <= equity × ratio
	adjustedPositionSize, wasCapped := at.enforcePositionValueRatio(decision.PositionSizeUSD, equity, decision.Symbol)
	if wasCapped {
		decision.PositionSizeUSD = adjustedPositionSize
	}

	// ⚠️ Auto-adjust position size if insufficient margin
	marginFactor := marginOverheadFactor/float64(decision.Leverage) + takerFeeRate
	maxAffordablePositionSize := availableBalance / marginFactor

	actualPositionSize := decision.PositionSizeUSD
	if actualPositionSize > maxAffordablePositionSize {
		adjustedSize := maxAffordablePositionSize * positionSizeSafetyFactor
		logger.Infof("  ⚠️ Position size %.2f exceeds max affordable %.2f, auto-reducing to %.2f",
			actualPositionSize, maxAffordablePositionSize, adjustedSize)
		actualPositionSize = adjustedSize
		decision.PositionSizeUSD = actualPositionSize
	}

	// [CODE ENFORCED] Minimum position size check
	if err := at.enforceMinPositionSize(decision.PositionSizeUSD); err != nil {
		return err
	}

	// Calculate quantity with adjusted position size
	quantity := actualPositionSize / marketData.CurrentPrice
	actionRecord.Quantity = quantity
	actionRecord.Price = marketData.CurrentPrice

	// Set margin mode
	if err := at.trader.SetMarginMode(decision.Symbol, at.config.IsCrossMargin); err != nil {
		logger.Infof("  ⚠️ Failed to set margin mode: %v", err)
		// Continue execution, doesn't affect trading
	}

	// Open position
	order, err := at.trader.OpenShort(decision.Symbol, quantity, decision.Leverage)
	if err != nil {
		return fmt.Errorf("failed to open short position for %s: %w", decision.Symbol, err)
	}

	// Record order ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	logger.Infof("  ✓ Position opened successfully, order ID: %v, quantity: %.4f", order["orderId"], quantity)

	// Record order to database and poll for confirmation
	at.recordAndConfirmOrder(order, decision.Symbol, "open_short", quantity, marketData.CurrentPrice, decision.Leverage, 0)

	// Record position opening time
	posKey := decision.Symbol + "_short"
	at.positionFirstSeenTime[posKey] = time.Now().UnixMilli()

	// Set stop loss (mandatory) and take profit. If the stop-loss cannot be
	// confirmed on the exchange, the position is rolled back and an error is
	// returned so it can never be left open without protection.
	if err := at.protectOpenPosition(decision.Symbol, "SHORT", quantity, decision.StopLoss, decision.TakeProfit); err != nil {
		return err
	}

	return nil
}

// executeCloseLongWithRecord executes close long position and records detailed information
func (at *AutoTrader) executeCloseLongWithRecord(decision *kernel.Decision, actionRecord *store.DecisionAction) error {
	logger.Infof("  🔄 Close long: %s", decision.Symbol)

	// Get current price
	marketData, err := market.GetWithExchange(decision.Symbol, at.exchange)
	if err != nil {
		return fmt.Errorf("failed to get market data for %s: %w", decision.Symbol, err)
	}
	actionRecord.Price = marketData.CurrentPrice

	// Normalize symbol for database lookup
	normalizedSymbol := market.Normalize(decision.Symbol)

	// Get entry price and quantity - prioritize local database for accurate quantity
	var entryPrice float64
	var quantity float64

	// First try to get from local database (more accurate for quantity)
	if at.store != nil {
		if openPos, err := at.store.Position().GetOpenPositionBySymbol(at.id, normalizedSymbol, "LONG"); err == nil && openPos != nil {
			quantity = openPos.Quantity
			entryPrice = openPos.EntryPrice
			logger.Infof("  📊 Using local position data: qty=%.8f, entry=%.2f", quantity, entryPrice)
		}
	}

	// Fallback to exchange API if local data not found
	if quantity == 0 {
		positions, err := at.trader.GetPositions()
		if err == nil {
			for _, pos := range positions {
				if pos["symbol"] == decision.Symbol && pos["side"] == "long" {
					if ep, ok := pos["entryPrice"].(float64); ok {
						entryPrice = ep
					}
					if amt, ok := pos["positionAmt"].(float64); ok && amt > 0 {
						quantity = amt
					}
					break
				}
			}
		}
		logger.Infof("  📊 Using exchange position data: qty=%.8f, entry=%.2f", quantity, entryPrice)
	}

	// Close position
	order, err := at.trader.CloseLong(decision.Symbol, 0) // 0 = close all
	if err != nil {
		return fmt.Errorf("failed to close long position for %s: %w", decision.Symbol, err)
	}

	// Record order ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	// Record order to database and poll for confirmation
	at.recordAndConfirmOrder(order, decision.Symbol, "close_long", quantity, marketData.CurrentPrice, 0, entryPrice)

	logger.Infof("  ✓ Position closed successfully")
	return nil
}

// executeCloseShortWithRecord executes close short position and records detailed information
func (at *AutoTrader) executeCloseShortWithRecord(decision *kernel.Decision, actionRecord *store.DecisionAction) error {
	logger.Infof("  🔄 Close short: %s", decision.Symbol)

	// Get current price
	marketData, err := market.GetWithExchange(decision.Symbol, at.exchange)
	if err != nil {
		return fmt.Errorf("failed to get market data for %s: %w", decision.Symbol, err)
	}
	actionRecord.Price = marketData.CurrentPrice

	// Normalize symbol for database lookup
	normalizedSymbol := market.Normalize(decision.Symbol)

	// Get entry price and quantity - prioritize local database for accurate quantity
	var entryPrice float64
	var quantity float64

	// First try to get from local database (more accurate for quantity)
	if at.store != nil {
		if openPos, err := at.store.Position().GetOpenPositionBySymbol(at.id, normalizedSymbol, "SHORT"); err == nil && openPos != nil {
			quantity = openPos.Quantity
			entryPrice = openPos.EntryPrice
			logger.Infof("  📊 Using local position data: qty=%.8f, entry=%.2f", quantity, entryPrice)
		}
	}

	// Fallback to exchange API if local data not found
	if quantity == 0 {
		positions, err := at.trader.GetPositions()
		if err == nil {
			for _, pos := range positions {
				if pos["symbol"] == decision.Symbol && pos["side"] == "short" {
					if ep, ok := pos["entryPrice"].(float64); ok {
						entryPrice = ep
					}
					if amt, ok := pos["positionAmt"].(float64); ok {
						quantity = -amt // positionAmt is negative for short
					}
					break
				}
			}
		}
		logger.Infof("  📊 Using exchange position data: qty=%.8f, entry=%.2f", quantity, entryPrice)
	}

	// Close position
	order, err := at.trader.CloseShort(decision.Symbol, 0) // 0 = close all
	if err != nil {
		return fmt.Errorf("failed to close short position for %s: %w", decision.Symbol, err)
	}

	// Record order ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	// Record order to database and poll for confirmation
	at.recordAndConfirmOrder(order, decision.Symbol, "close_short", quantity, marketData.CurrentPrice, 0, entryPrice)

	logger.Infof("  ✓ Position closed successfully")
	return nil
}

// ============================================================================
// Stop-loss protection: guarantee a freshly opened position is never left
// "naked" (open with no confirmed stop-loss). Either the stop-loss is
// confirmed on the exchange, or the position is rolled back (closed), residual
// protective orders are cancelled, and the trader is halted from opening new
// positions until an operator clears it.
// ============================================================================

// protectOpenPosition places the stop-loss (mandatory) and take-profit for a
// just-opened position. positionSide is "LONG" or "SHORT". fallbackQty is the
// pre-trade estimated size, used only if the live position size cannot be read.
//
// Behavior:
//   - The stop-loss is ALWAYS confirmed against the exchange's open orders, even
//     when SetStopLoss returns nil: a nil error does not guarantee the protective
//     order is actually live. If it cannot be confirmed after short retries, the
//     position is rolled back and an error is returned.
//   - A take-profit failure never rolls back the position: the confirmed
//     stop-loss already bounds the downside. It is retried once and otherwise
//     left in a degraded (logged) state.
func (at *AutoTrader) protectOpenPosition(symbol, positionSide string, fallbackQty, stopPrice, takeProfitPrice float64) error {
	// Prefer the actual on-exchange position size over the pre-trade estimate.
	// If it cannot be read (transient error / not yet visible), fall back to the
	// estimate so behavior matches the previous implementation.
	qty := at.actualPositionQty(symbol, positionSide)
	if qty <= 0 {
		qty = fallbackQty
	}

	// Initial placement. The return value is informative only — confirmation
	// against the exchange's open orders below is authoritative.
	slErr := at.trader.SetStopLoss(symbol, positionSide, qty, stopPrice)
	if slErr != nil {
		at.logWarnf("  ⚠ Stop-loss placement returned error (will verify/retry): %v", slErr)
	}

	// Confirm the stop-loss actually exists on the exchange. A nil error from the
	// placement call is NOT trusted: brokers can accept-then-drop, and an error
	// may still have landed. Re-check (allowing for propagation), then re-place
	// and re-check up to a bounded number of times before rolling back.
	confirmed, qErr := at.stopLossConfirmed(symbol, positionSide, stopPrice)
	for attempt := 1; attempt <= stopLossConfirmRetries && !confirmed; attempt++ {
		time.Sleep(stopLossRetryDelay)
		// The order may simply have been propagating; re-check before re-placing
		// to avoid stacking duplicates.
		if c, _ := at.stopLossConfirmed(symbol, positionSide, stopPrice); c {
			confirmed = true
			break
		}
		if rerr := at.trader.SetStopLoss(symbol, positionSide, qty, stopPrice); rerr != nil {
			at.logWarnf("  ⚠ Stop-loss retry %d/%d failed: %v", attempt, stopLossConfirmRetries, rerr)
		}
		confirmed, qErr = at.stopLossConfirmed(symbol, positionSide, stopPrice)
	}

	if !confirmed {
		// Roll back only on positive evidence the stop-loss is missing: either the
		// open-order query succeeded and the stop-loss is genuinely absent, or the
		// placement itself errored. If the query failed (qErr) AND placement
		// reported success, we cannot prove it is missing — trust the placement
		// rather than churn-close positions on exchanges whose open-order query
		// does not surface stop orders.
		if qErr != nil && slErr == nil {
			at.logWarnf("  ⚠ Could not verify stop-loss for %s %s (open-order query failed: %v); trusting successful placement. Verify this exchange surfaces stop orders before relying on auto-rollback.", symbol, positionSide, qErr)
		} else {
			reason := fmt.Sprintf("stop-loss could not be confirmed for %s %s after %d retries", symbol, positionSide, stopLossConfirmRetries)
			at.rollbackUnprotectedPosition(symbol, positionSide, reason)
			return fmt.Errorf("rolled back %s %s open: %s", symbol, positionSide, reason)
		}
	}

	// Take-profit is best-effort: the stop-loss already caps the downside, so a
	// take-profit failure degrades the position but never triggers a rollback.
	if err := at.trader.SetTakeProfit(symbol, positionSide, qty, takeProfitPrice); err != nil {
		logger.Infof("  ⚠ Failed to set take profit (position remains protected by stop-loss): %v", err)
		if rerr := at.trader.SetTakeProfit(symbol, positionSide, qty, takeProfitPrice); rerr != nil {
			at.logWarnf("  ⚠ Take-profit retry failed; leaving %s %s in degraded state (stop-loss active): %v", symbol, positionSide, rerr)
		}
	}

	return nil
}

// actualPositionQty returns the absolute live position size for symbol/side as
// reported by the exchange, or 0 if it cannot be determined. positionSide is
// "LONG" or "SHORT".
func (at *AutoTrader) actualPositionQty(symbol, positionSide string) float64 {
	positions, err := at.trader.GetPositions()
	if err != nil {
		at.logWarnf("  ⚠ Failed to read live position size for %s: %v", symbol, err)
		return 0
	}
	wantSide := "long"
	if strings.EqualFold(positionSide, "SHORT") {
		wantSide = "short"
	}
	for _, pos := range positions {
		if sym, _ := pos["symbol"].(string); sym != symbol {
			continue
		}
		if side, _ := pos["side"].(string); side != wantSide {
			continue
		}
		if amt, ok := pos["positionAmt"].(float64); ok {
			if amt < 0 {
				amt = -amt // short positions report a negative amount
			}
			return amt
		}
	}
	return 0
}

// stopLossConfirmTolerance is the relative tolerance allowed between a live
// stop order's trigger price and the requested stop price when confirming that
// OUR stop-loss (not a stale one) is in place.
const stopLossConfirmTolerance = 0.005 // 0.5%

// stopLossConfirmed reports whether a stop-loss order matching the given
// symbol/side AND trigger price is currently present on the exchange. The second
// return value is non-nil when the open orders could not be queried — callers
// use it to distinguish "the stop-loss is genuinely absent" (queryErr == nil,
// confirmed == false) from "we could not verify" (queryErr != nil), so a
// position is not churn-closed merely because an exchange's open-order query is
// unavailable. Matching the trigger price avoids mistaking a stale, uncancelled
// stop order for the one just placed. A non-positive stopPrice skips the price
// match (type/side only).
func (at *AutoTrader) stopLossConfirmed(symbol, positionSide string, stopPrice float64) (confirmed bool, queryErr error) {
	orders, err := at.trader.GetOpenOrders(symbol)
	if err != nil {
		at.logWarnf("  ⚠ Failed to query open orders while confirming stop-loss for %s: %v", symbol, err)
		return false, err
	}
	want := strings.ToUpper(positionSide)
	for _, o := range orders {
		ot := strings.ToUpper(o.Type)
		// A stop-loss is a STOP-type order; exclude take-profit orders, whose
		// type contains TAKE_PROFIT (and may also contain STOP on some venues).
		if !strings.Contains(ot, "STOP") || strings.Contains(ot, "TAKE_PROFIT") {
			continue
		}
		ps := strings.ToUpper(o.PositionSide)
		// Accept a matching hedge-mode side, or one-way mode where the position
		// side is reported as BOTH or left empty.
		if ps != want && ps != "BOTH" && ps != "" {
			continue
		}
		// Verify the trigger price matches what we requested, so a stale stop is
		// not mistaken for the current one.
		if stopPrice > 0 && o.StopPrice > 0 {
			if math.Abs(o.StopPrice-stopPrice)/stopPrice > stopLossConfirmTolerance {
				continue
			}
		}
		return true, nil
	}
	return false, nil
}

// rollbackUnprotectedPosition closes a position that could not be protected by a
// confirmed stop-loss, cancels any residual protective orders for the symbol,
// and raises a persistent protection halt that blocks new opens. positionSide
// is "LONG" or "SHORT".
func (at *AutoTrader) rollbackUnprotectedPosition(symbol, positionSide, reason string) {
	at.logErrorf("  🚨 NAKED POSITION: %s %s has no confirmed stop-loss; closing immediately. Reason: %s", symbol, positionSide, reason)

	var err error
	if strings.EqualFold(positionSide, "LONG") {
		_, err = at.trader.CloseLong(symbol, 0) // 0 = close all
	} else {
		_, err = at.trader.CloseShort(symbol, 0) // 0 = close all
	}
	if err != nil {
		at.logErrorf("  🚨 CRITICAL: failed to close unprotected %s %s during rollback: %v — MANUAL INTERVENTION REQUIRED", symbol, positionSide, err)
	} else {
		at.logInfof("  ✓ Unprotected %s %s closed (rollback)", symbol, positionSide)
	}

	// Best-effort: remove any residual protective (stop-loss/take-profit) orders
	// left behind. CancelStopOrders targets only protective orders, so unrelated
	// limit orders for the symbol are not disturbed.
	if cerr := at.trader.CancelStopOrders(symbol); cerr != nil {
		at.logWarnf("  ⚠ Failed to cancel residual protective orders for %s after rollback: %v", symbol, cerr)
	}

	at.raiseProtectionHalt(reason)
}

// raiseProtectionHalt sets the persistent open-block. It is safe for concurrent
// access. Close and maintenance actions are unaffected.
func (at *AutoTrader) raiseProtectionHalt(reason string) {
	at.protectionHaltMu.Lock()
	at.protectionHalt = true
	at.protectionHaltReason = reason
	at.protectionHaltMu.Unlock()
	at.logErrorf("  🔒 Open trading halted: new positions are blocked until cleared. Reason: %s", reason)
}

// protectionHaltState reports whether the persistent open-block is active and
// why. It is safe for concurrent access.
func (at *AutoTrader) protectionHaltState() (bool, string) {
	at.protectionHaltMu.RLock()
	defer at.protectionHaltMu.RUnlock()
	return at.protectionHalt, at.protectionHaltReason
}

// assertOpenAllowed returns an error when new positions are blocked by an active
// protection halt. Close and maintenance paths do not call this, so existing
// positions can still be managed while opens are halted.
func (at *AutoTrader) assertOpenAllowed() error {
	if halted, reason := at.protectionHaltState(); halted {
		return fmt.Errorf("open blocked: trading halted after a stop-loss protection failure: %s", reason)
	}
	return nil
}
