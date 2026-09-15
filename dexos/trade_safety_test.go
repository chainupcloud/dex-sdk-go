package dexos

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBatchPartialSuccessIsNotWholeSuccess(t *testing.T) {
	// 真实内核会跳过失败项，只返回成功 OrderAccepted；夹具按服务端 JSON 契约手写。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"ok","seq":12,"events":[{"kind":"OrderAccepted","data":{"account":42,"market":7,"orderSeq":9,"price":100,"lots":1,"filledLots":0,"resting":true}}]}`)
	}))
	defer srv.Close()
	signer, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	orders := []BatchOrder{{Market: 7, Price: 100, Lots: 1, Side: Buy, TIF: PostOnly}, {Market: 7, Price: 200, Lots: 1, Side: Sell, TIF: PostOnly}}
	ev, err := NewClient(srv.URL, 1).BatchPlace(context.Background(), signer, 42, orders, 3)
	if err == nil {
		t.Fatal("仅一项成功却把整批作为成功")
	}
	if len(ev) != 1 {
		t.Fatal("部分成功证据被丢弃")
	}
}

func TestSignerRejectsInvalidScalar(t *testing.T) {
	for _, key := range []string{"0000000000000000000000000000000000000000000000000000000000000000", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"} {
		if _, err := NewSigner(key); err == nil {
			t.Fatal("零或越界私钥被接受")
		}
	}
}
