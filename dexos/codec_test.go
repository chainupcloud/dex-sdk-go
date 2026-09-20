package dexos

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// 与 Rust 内核编码器的**逐字节对拍**。
//
// 这是 SDK 里唯一不能靠 review 保证的部分:签名承诺的是规范编码的哈希,
// 差一个字节就是「一律被拒且无线索」。金样由 dex-os 侧生成:
//
//	cargo run -q --example sdk_golden > dexos/testdata/golden.json
//
// 内核改了编码而这里没跟上时,这组测试会红 —— 这正是它存在的理由。
// 第一个被它抓出来的 bug 就是漏了 COMMAND_CODEC_VERSION 那个前缀字节。
func TestGoldenCanonicalEncoding(t *testing.T) {
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatalf("读金样失败(先在 dex-os 跑 cargo run --example sdk_golden):%v", err)
	}
	var g struct {
		Vectors []struct {
			Name string `json:"name"`
			Hex  string `json:"hex"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("金样解析失败:%v", err)
	}
	if len(g.Vectors) == 0 {
		t.Fatal("金样为空 —— 空集合恒过,等于没测")
	}

	// 各条金样对应的 Go 侧重算。名字必须与 sdk_golden.rs 里的一致。
	built := map[string][]byte{
		"cancel_order":       EncodeCancelOrder(7, NewOrderID(3, 42)),
		"batch_cancel_empty": EncodeBatchCancel(0, nil),
		"batch_cancel_three": EncodeBatchCancel(4294967295, []OrderID{
			NewOrderID(0, 1),
			NewOrderID(65535, 281474976710655),
			NewOrderID(7, 42),
		}),
		"batch_place_empty": EncodeBatchPlace(1, 0, nil),
		"batch_place_two": EncodeBatchPlace(3, 1700000000000, []BatchOrder{
			{Market: 0, Side: Buy, Price: 49500, Lots: 10, TIF: GTC},
			{Market: 65535, Side: Sell, Price: ^uint32(0), Lots: ^uint64(0), TIF: PostOnly, ReduceOnly: true},
		}),
		"batch_replace_empty_empty": EncodeBatchReplace(0, 0, nil, nil),
		"batch_replace_mixed": EncodeBatchReplace(9, 1700000000123,
			[]OrderID{NewOrderID(0, 1), NewOrderID(0, 2)},
			[]BatchOrder{
				{Market: 0, Side: Buy, Price: 49900, Lots: 5, TIF: GTC},
				{Market: 0, Side: Sell, Price: 50100, Lots: 5, TIF: IOC},
				{Market: 0, Side: Buy, Price: 49800, Lots: 7, TIF: FOK, ReduceOnly: true},
			}),
		"batch_replace_cancels_only": EncodeBatchReplace(2, 5,
			[]OrderID{NewOrderID(1, 8)}, nil),
		"batch_replace_orders_only": EncodeBatchReplace(2, 5, nil,
			[]BatchOrder{{Market: 1, Side: Sell, Price: 100, Lots: 1, TIF: GTC}}),

		// 通用 /agent/exec 打通的那批
		"schedule_cancel":       EncodeScheduleCancel(5, 1700000060000, 1700000000000),
		"schedule_cancel_clear": EncodeScheduleCancel(5, 0, 1),
		"set_leverage":          EncodeSetLeverage(6, 2, 200000),
		"place_order": EncodePlaceOrder(3, 0, Sell, 50100, 7, IOC, true,
			1700000099000, 1700000000000, 9, 250),
		"modify_order": EncodeModifyOrder(3, NewOrderID(0, 11), 49950, 4, GTC, false,
			1700000500000, 1700000000001),
		"cancel_conditional": EncodeCancelConditional(3, NewOrderID(1, 77)),
		"cancel_twap":        EncodeCancelTwap(3, NewOrderID(1, 78)),

		// 白名单里剩下的三条下单类命令
		"place_conditional": EncodePlaceConditional(3, Conditional{
			Market: 1, Side: Sell, Price: 48000, Lots: 12, TIF: IOC, ReduceOnly: true,
			GoodTilMs: 1700000900000, TriggerPrice: 48500, TriggerAbove: false,
		}, 1700000000002),
		"place_twap": EncodePlaceTwap(3, Twap{
			Market: 0, Side: Buy, TotalLots: 1000, Slices: 20, IntervalMs: 30000,
			PriceTolerancePpm: 5000, ReduceOnly: false,
		}, 1700000000003),
		"place_tpsl_pair": EncodePlaceTpslPair(3, TpslPair{
			Market: 0, CloseSide: Sell, Lots: 10, TpTrigger: 52000, TpPrice: 51900,
			SlTrigger: 48000, SlPrice: 0, PositionTpsl: true, // Parent 零值 → NoParent
		}, 1700000000004),
		"place_tpsl_pair_with_parent": EncodePlaceTpslPair(3, TpslPair{
			Market: 2, CloseSide: Buy, Lots: 1, TpTrigger: 100, TpPrice: 101,
			SlTrigger: 200, SlPrice: 199, PositionTpsl: false, Parent: NewOrderID(2, 77),
		}, 5),
	}

	seen := 0
	for _, v := range g.Vectors {
		got, ok := built[v.Name]
		if !ok {
			t.Errorf("金样 %q 在 Go 侧没有对应重算 —— 内核加了命令而 SDK 没跟上", v.Name)
			continue
		}
		seen++
		if want := v.Hex; hex.EncodeToString(got) != want {
			t.Errorf("%s 编码不一致\n  Rust: %s\n  Go  : %s", v.Name, want, hex.EncodeToString(got))
		}
	}
	if seen != len(built) {
		t.Errorf("Go 侧重算了 %d 条,金样只覆盖 %d 条 —— 有重算没有被验证", len(built), seen)
	}
}

// OrderID 打包/解包自洽。内核是 (market << 48) | seq。
func TestOrderIDPacking(t *testing.T) {
	cases := []struct {
		market uint16
		seq    uint64
	}{{0, 0}, {1, 1}, {7, 42}, {65535, 281474976710655}}
	for _, c := range cases {
		id := NewOrderID(c.market, c.seq)
		if id.Market() != c.market || id.Seq() != c.seq {
			t.Errorf("打包往返失败 market=%d seq=%d → market=%d seq=%d",
				c.market, c.seq, id.Market(), id.Seq())
		}
	}
}
