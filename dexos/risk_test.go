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
