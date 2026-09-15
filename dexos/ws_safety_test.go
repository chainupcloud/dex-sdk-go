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
