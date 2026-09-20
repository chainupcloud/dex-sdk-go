package dexos

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestStreamDamageIsTerminal(t *testing.T) {
	for _, frame := range []string{`{`, `{"seq":5}`, `{"kind":"Lagged","dropped":3}`, `{"seq":5,"kind":"Fill","data":{}}`} {
		t.Run(frame, func(t *testing.T) {
			up := websocket.Upgrader{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := up.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.WriteMessage(websocket.TextMessage, []byte(frame))
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			st, err := NewClient(srv.URL, 1).Subscribe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-st.Err:
				if err == nil {
					t.Fatal("流损坏未返回原因")
				}
			case <-time.After(150 * time.Millisecond):
				t.Fatal("坏帧/断线后未显性终止，仍可误当完整事件流")
			}
		})
	}
}

func TestCancelClosesSilentStream(t *testing.T) {
	connections := make(chan *websocket.Conn, 1)
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		connections <- conn
		_, _, _ = conn.ReadMessage()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := NewClient(srv.URL, 1).Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conn := <-connections
	defer conn.Close()
	cancel()
	select {
	case <-st.Err:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("ctx 取消未关闭静默连接")
	}
}

// seq 回退不是流损坏:真节点上系统命令事件与订单事件走不同派生路径,seq 每几分钟
// 就小幅回退一次。把它当终止条件,每个 WS 消费者都会周期性断流。
func TestSeqRegressionIsNotTerminal(t *testing.T) {
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for _, f := range []string{
			`{"seq":10,"sub":0,"idx":0,"kind":"OrderAccepted","data":{"orderSeq":1}}`,
			`{"seq":9,"sub":0,"idx":0,"kind":"OracleUpdated","data":{"market":0}}`,
			`{"seq":11,"sub":1,"idx":2,"kind":"Fill","data":{"id":"11-1-2"}}`,
		} {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(f))
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	st, err := NewClient(srv.URL, 1).Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for got < 3 {
		select {
		case ev, ok := <-st.Events:
			if !ok {
				t.Fatalf("seq 回退后流被关闭,只收到 %d 帧", got)
			}
			got++
			if got == 3 && ev.ID() != "11-1-2" {
				t.Errorf("第三帧身份应为 11-1-2,实得 %s", ev.ID())
			}
		case err := <-st.Err:
			t.Fatalf("seq 回退被当成流损坏:%v", err)
		case <-time.After(300 * time.Millisecond):
			t.Fatalf("只收到 %d 帧", got)
		}
	}
}
