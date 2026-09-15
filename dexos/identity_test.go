package dexos

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentGrantsPreservesIdentityAndRejectsMissingFields(t *testing.T) {
	agent, err := ParseAddress("0x2222222222222222222222222222222222222222")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "master", "expired", "validUntilMs", "nextNonce", "agent"} {
		t.Run(bad, func(t *testing.T) {
			grant := map[string]any{"master": 42, "expired": false, "validUntilMs": "0"}
			body := map[string]any{"agent": agent.Hex(), "nextNonce": 17, "grants": []any{grant}}
			delete(grant, bad)
			delete(body, bad)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("身份查询出现写操作")
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer srv.Close()
			got, err := NewClient(srv.URL, 1).AgentGrants(context.Background(), agent)
			if bad != "" {
				if err == nil {
					t.Fatalf("缺 %s 仍接受授权", bad)
				}
				return
			}
			if err != nil || got.Agent != agent || got.NextNonce != 17 || len(got.Grants) != 1 || got.Grants[0].Master != 42 {
				t.Fatalf("授权事实不完整: %+v %v", got, err)
			}
		})
	}
}

func TestCompleteBatchPreservesEventsAndReadWatermark(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/batch" {
			io.WriteString(w, `{"status":"ok","seq":12,"events":[{"kind":"OrderAccepted","data":{"account":42,"market":7,"orderSeq":9,"price":100,"lots":1,"filledLots":0,"resting":true}}]}`)
			return
		}
		if r.Header.Get("x-dexos-min-seq") != "12" {
			t.Error("后续读取未携带原回执水位")
		}
		io.WriteString(w, `{"orders":[{"seq":9}]}`)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, 1)
	signer, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	ev, err := c.BatchPlace(context.Background(), signer, 42, []BatchOrder{{Market: 7, Price: 100, Lots: 1, Side: Buy, TIF: PostOnly}}, 3)
	if err != nil || len(ev) != 1 {
		t.Fatalf("完整成功失败: %v %v", ev, err)
	}
	orders, err := c.Orders(context.Background(), 42, 7)
	if err != nil || len(orders) != 1 {
		t.Fatalf("读取订单失败: %v %v", orders, err)
	}
}

func TestSessionSerializationAndBlockedResync(t *testing.T) {
	s := &Session{nonce: 3}
	for i := uint64(3); i < 6; i++ {
		_, err := s.write(context.Background(), func(n uint64) ([]EventEnvelope, error) {
			if n != i {
				t.Fatalf("nonce=%d want %d", n, i)
			}
			return []EventEnvelope{}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	_, _ = s.write(context.Background(), func(uint64) ([]EventEnvelope, error) { return nil, fmt.Errorf("unknown") })
	if err := s.Resync(context.Background()); err == nil {
		t.Fatal("重读 nonce 清除了未决写入")
	}
}
