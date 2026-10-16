//go:build dexos_contract

package dexosv1

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// 模拟 CancelAll 回执已返回、下一块尚未撤单；只有块边界后订单才消失。
func TestContractCancelBoundaryWaitsForRemoval(t *testing.T) {
	f, c := newFake(t)
	cat := catalog(t, f, c)
	pending := fixture(t, "orders.json")
	reads := 0
	f.routes["/v1/account/orders"] = func(w http.ResponseWriter, r *http.Request) {
		reads++
		if reads < 3 {
			_, _ = w.Write(pending)
			return
		}
		_, _ = w.Write([]byte(`{"orders":[]}`))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitContractCancelBoundary(ctx, c, cat, AccountRef{Address: mustAddr(t, testAddr)}, 2240412712968); err != nil {
		t.Fatal(err)
	}
	if reads != 3 {
		t.Fatalf("CancelAll 尚未生效便交出账户:读取 %d 次", reads)
	}
}

// 订单持续在挂或读取失败时必须失败，不能把未确认的撤单当作成功。
func TestContractCancelBoundaryRejectsUnconfirmedRemoval(t *testing.T) {
	for _, name := range []string{"pending", "read-error"} {
		t.Run(name, func(t *testing.T) {
			f, c := newFake(t)
			cat := catalog(t, f, c)
			pending := fixture(t, "orders.json")
			f.routes["/v1/account/orders"] = func(w http.ResponseWriter, r *http.Request) {
				if name == "read-error" {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				_, _ = w.Write(pending)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			err := waitContractCancelBoundary(ctx, c, cat, AccountRef{Address: mustAddr(t, testAddr)}, 2240412712968)
			if err == nil {
				t.Fatal("未确认撤单却返回成功")
			}
			if name == "pending" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("等待挂单消失: %v", err)
			}
		})
	}
}

func waitContractCancelBoundary(ctx context.Context, c *Client, cat *Catalog, ref AccountRef, id uint64) error {
	// 远离市价的哨兵单在 CancelAll 之前已挂上；它离开集合才能证明一次性撤单已触发。
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		orders, err := c.Orders(ctx, cat, ref, "")
		if err != nil {
			return err
		}
		found := false
		for _, order := range orders {
			if order.OrderID == id {
				found = true
				break
			}
		}
		if !found {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
