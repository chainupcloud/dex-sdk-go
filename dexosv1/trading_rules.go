package dexosv1

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

var ErrTradingRulesUnavailable = errors.New("dexosv1: market trading rules unavailable")

type TradingRules struct {
	SubticksPerTick uint32
	Version         uint64
	Pending         bool
	TickSize        string
}

func (m Market) RequireTradingRules() (TradingRules, error) {
	if m.TradingRules == nil {
		return TradingRules{}, ErrTradingRulesUnavailable
	}
	return *m.TradingRules, nil
}

func (w wireMarket) tradingRules(scale Scale) (*TradingRules, error) {
	if !w.rulesPresent && w.SubticksPerTick == nil && w.TradingRulesVersion == nil && w.TickChangePending == nil {
		return nil, nil
	}
	if w.SubticksPerTick == nil || w.TradingRulesVersion == nil || w.TickChangePending == nil || w.TickSize == nil {
		return nil, errors.New("incomplete trading rules")
	}
	if *w.SubticksPerTick == 0 {
		return nil, errors.New("subticksPerTick must be positive")
	}
	version, err := strconv.ParseUint(*w.TradingRulesVersion, 10, 64)
	if err != nil || strconv.FormatUint(version, 10) != *w.TradingRulesVersion {
		return nil, errors.New("invalid tradingRulesVersion")
	}
	step, err := scale.Ticks(*w.TickSize)
	if err != nil || step != *w.SubticksPerTick {
		return nil, fmt.Errorf("tickSize does not match subticksPerTick")
	}
	return &TradingRules{SubticksPerTick: step, Version: version, Pending: *w.TickChangePending, TickSize: *w.TickSize}, nil
}

func (w *wireMarket) UnmarshalJSON(data []byte) error {
	type plain wireMarket
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*w = wireMarket(decoded)
	for _, key := range []string{"subticksPerTick", "tradingRulesVersion", "tickChangePending"} {
		if _, ok := fields[key]; ok {
			w.rulesPresent = true
		}
	}
	return nil
}
