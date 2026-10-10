package dexos

import (
	"context"
	"testing"
)

// /risk 带出视图所在的执行纪元与位置(asOfSeq,与 /book 的 seq、/equity 的 upto_seq 同一条轴):
// 调用方据此读纪元、给历史读接口定上界,不必再发不带游标的历史查询(dex-os #28)。
func TestRiskCarriesEpochAndAsOfSeq(t *testing.T) {
	c, _ := serveOnce(t, "/risk/7", 200, `{"exists":true,"accountId":7,"collateral":"100","balances":[],
		"nc":"100","imr":"0","mmr":"0","withdrawable":"100","nextNonce":3,"positions":[],
		"epoch":"k1-1","asOfSeq":4096}`)
	r, err := c.Risk(context.Background(), 7)
	if err != nil {
		t.Fatalf("读风险视图失败: %v", err)
	}
	if r.Epoch != "k1-1" || r.AsOfSeq == nil || *r.AsOfSeq != 4096 {
		t.Fatalf("纪元与视图位置没有解析出来: epoch=%q asOfSeq=%v", r.Epoch, r.AsOfSeq)
	}
}

// 老网关没有这两个字段:保持空值与 nil,不补默认值 —— 0 是合法位置,补了就分不清「没给」与「在 0」。
func TestRiskWithoutEpochLeavesThemUnset(t *testing.T) {
	c, _ := serveOnce(t, "/risk/7", 200, `{"exists":true,"accountId":7,"collateral":"100","balances":[],
		"nc":"100","imr":"0","mmr":"0","withdrawable":"100","nextNonce":3,"positions":[]}`)
	r, err := c.Risk(context.Background(), 7)
	if err != nil {
		t.Fatalf("读风险视图失败: %v", err)
	}
	if r.Epoch != "" || r.AsOfSeq != nil {
		t.Fatalf("缺字段时应当保持未设置: epoch=%q asOfSeq=%v", r.Epoch, r.AsOfSeq)
	}
}

// /risk 的 groups[] 是各抵押 token 的风险组(D24),与余额、仓位、asOfSeq 同一次取视图(dex-os #29):
// 调用方据此判账户是否只有一个抵押组、取该组的 nc,不必另读 /equity(dex-os #28)。
func TestRiskCarriesRiskGroups(t *testing.T) {
	c, _ := serveOnce(t, "/risk/7", 200, `{"exists":true,"accountId":7,"collateral":"100","balances":[],
		"nc":"100","imr":"0","mmr":"0","withdrawable":"100","nextNonce":3,"positions":[],"epoch":"k1-1","asOfSeq":4096,
		"groups":[{"token":0,"nc":"100","imr":"7","mmr":"3","openOrderMargin":"2","withdrawable":"91"},
		          {"token":5,"nc":"-4","imr":"0","mmr":"0","openOrderMargin":"0","withdrawable":"0"}]}`)
	r, err := c.Risk(context.Background(), 7)
	if err != nil {
		t.Fatalf("读风险视图失败: %v", err)
	}
	want := []RiskGroup{
		{Token: 0, NC: "100", IMR: "7", MMR: "3", OpenOrderMargin: "2", Withdrawable: "91"},
		{Token: 5, NC: "-4", IMR: "0", MMR: "0", OpenOrderMargin: "0", Withdrawable: "0"},
	}
	if len(r.Groups) != len(want) {
		t.Fatalf("风险组数=%d,期望 %d: %+v", len(r.Groups), len(want), r.Groups)
	}
	for i := range want {
		if r.Groups[i] != want[i] {
			t.Fatalf("风险组 %d 解析不对: got=%+v want=%+v", i, r.Groups[i], want[i])
		}
	}
}

// 老网关不给 groups:保持 nil,不补一个空组 —— 空组读起来是「这个账户没有风险」。
func TestRiskWithoutGroupsLeavesThemNil(t *testing.T) {
	c, _ := serveOnce(t, "/risk/7", 200, `{"exists":true,"accountId":7,"collateral":"100","balances":[],
		"nc":"100","imr":"0","mmr":"0","withdrawable":"100","nextNonce":3,"positions":[]}`)
	r, err := c.Risk(context.Background(), 7)
	if err != nil {
		t.Fatalf("读风险视图失败: %v", err)
	}
	if r.Groups != nil {
		t.Fatalf("缺 groups 时应当保持 nil: %+v", r.Groups)
	}
}
