package dexosv1

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 写请求发送失败怎么归类(nonceGuard.run):只有连接没建起来(dial 失败)才确定没发出,报 ErrNotSent,nonce 不动、不锁;
// 连上之后的失败都可能已经到了网关,结论未知,照旧锁会话。

// 各写入口(批量下单、撤单、换单、撤全部、断线保护、金库身份、主钱包)都经 Client.post 发送,连接被拒时同样报
// ErrNotSent、不锁、nonce 不动,请求没到网关;连上之后同一个会话照常写。
func TestDialFailureIsNotSent(t *testing.T) {
	ctx := context.Background()
	writes := map[string]func(*Session, *Market) error{
		"批量下单": func(s *Session, btc *Market) error {
			_, err := s.Place(ctx, OrderRequest{Market: btc, Side: dexos.Buy, Price: 65000, Lots: 1})
			return err
		},
		"撤单": func(s *Session, btc *Market) error { _, err := s.Cancel(ctx, btc, 101); return err },
		"换单": func(s *Session, btc *Market) error {
			_, err := s.Replace(ctx, btc, []uint64{101}, []OrderRequest{{Market: btc, Side: dexos.Buy, Price: 65000, Lots: 1}})
			return err
		},
		"撤全部":  func(s *Session, _ *Market) error { _, err := s.CancelAll(ctx); return err },
		"断线保护": func(s *Session, _ *Market) error { _, err := s.ScheduleCancel(ctx, time.Time{}); return err },
	}
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			f, s, cat := newTestSession(t)
			btc, _ := cat.Market("BTC-USD")
			requireNotSent(t, s.Client, func() error { return write(s, btc) }, func() (uint64, *RequestIdentity) { return s.Nonce(), s.Pending() }, 41, f)
			f.answer(t, "/v1/orders/cancel-all", 200, "w_cancel_all.json")
			if _, err := s.CancelAll(ctx); err != nil || s.Nonce() != 42 {
				t.Fatalf("连上之后应照常写: %v, nonce %d", err, s.Nonce())
			}
		})
	}
	t.Run("金库身份", func(t *testing.T) {
		f, c := vaultWriteFake(t, `[{"address":"`+agentAddr+`","label":"mm","validUntil":null,"expired":false}]`)
		s, err := NewSession(ctx, c, signer(t, agentKey), VaultAccount(3))
		if err != nil {
			t.Fatal(err)
		}
		requireNotSent(t, c, func() error { _, err := s.CancelAll(ctx); return err }, func() (uint64, *RequestIdentity) { return s.Nonce(), s.Pending() }, 9, f)
	})
	t.Run("主钱包", func(t *testing.T) {
		f, c := newWriteFake(t)
		cat, err := c.Catalog(ctx)
		if err != nil {
			t.Fatal(err)
		}
		btc, _ := cat.Market("BTC-USD")
		w, err := NewWallet(ctx, c, signer(t, ownerKey), 0)
		if err != nil {
			t.Fatal(err)
		}
		requireNotSent(t, c, func() error { _, err := w.Cancel(ctx, btc, 101); return err }, func() (uint64, *RequestIdentity) { return w.Nonce(), w.Pending() }, w.Nonce(), f)
	})
}

// requireNotSent 把 c 指向一个没人监听的本机地址发一次写:应报 ErrNotSent、不锁、nonce 仍是 nonce、没有未决请求,网关
// 没收到任何写。之后 c 指回原网关。
func requireNotSent(t *testing.T, c *Client, write func() error, state func() (uint64, *RequestIdentity), nonce uint64, f *writeFake) {
	t.Helper()
	gateway := c.BaseURL
	c.BaseURL = "http://" + refusingAddr(t)
	err := write()
	c.BaseURL = gateway
	got, pending := state()
	if !errors.Is(err, ErrNotSent) || errors.Is(err, ErrSessionBlocked) || got != nonce || pending != nil || f.count() != 0 {
		t.Fatalf("连接被拒应报 ErrNotSent、不锁、nonce 不动: %v, nonce %d → %d, 未决 %+v, 网关收到 %d 次写", err, nonce, got, pending, f.count())
	}
}

// refusingAddr 是本机一个没人监听的地址:连过去即被拒。
func refusingAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// 连上之后的失败都可能已经到了网关:TLS 握手失败、连上后被重置、等应答超时、应答读到一半断开,都锁会话、留下未决请求,
// 不算 ErrNotSent。
func TestFailureAfterConnectLocksSession(t *testing.T) {
	const path = "/v1/orders/cancel-all"
	cases := map[string]func(t *testing.T, f *writeFake, c *Client){
		"TLS 握手失败": func(t *testing.T, _ *writeFake, c *Client) {
			// 客户端不认测试证书,握手失败;请求没到处理函数。
			srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("握手失败的请求不该到网关") }))
			t.Cleanup(srv.Close)
			c.BaseURL = srv.URL
		},
		"连上后被重置": func(t *testing.T, f *writeFake, _ *Client) {
			f.routes[path] = func(w http.ResponseWriter, _ *http.Request) {
				if conn := hijack(t, w); conn != nil {
					_ = conn.(*net.TCPConn).SetLinger(0) // 关闭即发 RST
					_ = conn.Close()
				}
			}
		},
		"等应答超时": func(t *testing.T, f *writeFake, c *Client) {
			c.HTTP = &http.Client{Timeout: 100 * time.Millisecond}
			f.routes[path] = func(_ http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body) // 读完请求体,客户端断开时 r.Context() 才会结束
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
			}
		},
		"应答读到一半断开": func(t *testing.T, f *writeFake, _ *Client) {
			f.routes[path] = func(w http.ResponseWriter, _ *http.Request) {
				conn := hijack(t, w)
				if conn == nil {
					return
				}
				_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"status\":"))
				_ = conn.Close()
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f, s, _ := newTestSession(t)
			setup(t, f, s.Client)
			_, err := s.CancelAll(context.Background())
			if !errors.Is(err, ErrSessionBlocked) || errors.Is(err, ErrNotSent) || s.Nonce() != 41 || s.Pending() == nil || s.Pending().Nonce != 41 {
				t.Fatalf("连上之后的失败应锁会话、留下未决请求、不算没发出: %v, nonce %d, 未决 %+v", err, s.Nonce(), s.Pending())
			}
		})
	}
}

// hijack 在处理函数里接管连接(不在测试主协程,失败只记错、返回 nil)。
func hijack(t *testing.T, w http.ResponseWriter) net.Conn {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Errorf("接管连接失败: %v", err)
		return nil
	}
	return conn
}
