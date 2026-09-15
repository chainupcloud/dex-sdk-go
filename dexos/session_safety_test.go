package dexos

import (
	"context"
	"errors"
	"testing"
)

func TestUnknownWriteStopsBeforeNonceResyncOrNextWrite(t *testing.T) {
	s := &Session{nonce: 7, Client: NewClient("http://example.invalid", 1)}
	// 不应触发 nonce 查询（Agent 故意不提供）；网络结果未知就必须停住。
	calls := 0
	write := func(uint64) ([]EventEnvelope, error) { calls++; return nil, errors.New("connection lost after submit") }
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("未知结果触发 nonce 重取: %v", r)
			}
		}()
		_, _ = s.write(context.Background(), write)
	}()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("未知结果后仍尝试写入: %v", r)
			}
		}()
		_, _ = s.write(context.Background(), write)
	}()
	if calls != 1 {
		t.Fatalf("未知结果后仍再次发单: calls=%d", calls)
	}
	if s.nonce != 7 {
		t.Fatalf("未知结果竟推进 nonce: %d", s.nonce)
	}
}
