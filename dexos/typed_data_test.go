package dexos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTypedDataFieldsMatchSignedTypeStrings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields []TypedField
		want   string
	}{
		{"EIP712Domain", domainFields, domainType},
		{"ApproveAgent", approveAgentFields, approveAgentType},
		{"RevokeAgent", revokeAgentFields, revokeAgentType},
	} {
		if got := encodeType(tc.name, tc.fields); got != tc.want {
			t.Fatalf("%s 字段表与签名类型串漂移:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
}

// hashTypedData 按字段表与 message 通用地重算 EIP-712 哈希,只覆盖本文件用到的类型。
// 它不经过 ApproveAgentHash 的手写字段顺序,所以能抓到 message 值或顺序与哈希不一致。
func hashTypedData(t *testing.T, td TypedData) []byte {
	t.Helper()
	word := func(typ string, v any) []byte {
		switch typ {
		case "address":
			a, err := ParseAddress(v.(string))
			if err != nil {
				t.Fatal(err)
			}
			return wordAddr(a)
		case "string":
			return Keccak256([]byte(v.(string)))
		default:
			n, ok := new(big.Int).SetString(v.(string), 10)
			if !ok {
				t.Fatalf("整数字段须为十进制字符串: %v", v)
			}
			return wordUint(n.Bytes())
		}
	}
	structHash := func(name string, values map[string]any) []byte {
		parts := [][]byte{Keccak256([]byte(encodeType(name, td.Types[name])))}
		for _, f := range td.Types[name] {
			parts = append(parts, word(f.Type, values[f.Name]))
		}
		return Keccak256(parts...)
	}
	domain := map[string]any{
		"name": td.Domain.Name, "version": td.Domain.Version,
		"chainId": new(big.Int).SetUint64(td.Domain.ChainID).String(), "verifyingContract": td.Domain.VerifyingContract,
	}
	return Keccak256([]byte{0x19, 0x01}, structHash("EIP712Domain", domain), structHash(td.PrimaryType, td.Message))
}

func TestAgentTypedDataHashesToSignedHash(t *testing.T) {
	owner, _ := ParseAddress("0x1111111111111111111111111111111111111111")
	agent, _ := ParseAddress("0x2222222222222222222222222222222222222222")
	contract, _ := ParseAddress("0x3333333333333333333333333333333333333333")
	d := Domain{Name: "dex-os", Version: "1", ChainID: 11155111, VerifyingContract: contract}

	approve := d.ApproveAgentTypedData(owner, 7, agent, 1789111997200, 42)
	if got, want := hashTypedData(t, approve), d.ApproveAgentHash(owner, 7, agent, 1789111997200, 42); !bytes.Equal(got, want) {
		t.Fatalf("ApproveAgent 待签结构与签名哈希不一致: %x != %x", got, want)
	}
	revoke := d.RevokeAgentTypedData(owner, 7, agent, 43)
	if got, want := hashTypedData(t, revoke), d.RevokeAgentHash(owner, 7, agent, 43); !bytes.Equal(got, want) {
		t.Fatalf("RevokeAgent 待签结构与签名哈希不一致: %x != %x", got, want)
	}
	// 永不过期是 0,不是省略字段。
	if approve := d.ApproveAgentTypedData(owner, 7, agent, 0, 42); approve.Message["validUntilMs"] != "0" {
		t.Fatalf("永不过期须显式签 0: %v", approve.Message["validUntilMs"])
	}
}

func TestSubmitApproveAgentSendsSignedFieldsVerbatim(t *testing.T) {
	master, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	api, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.String()
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{"status":"ok","seq":9,"sub":0,"events":[{"kind":"AgentApproved","data":{}}]}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, 11155111)

	sig, err := master.SignHashHex(c.Domain.ApproveAgentHash(master.Address(), 7, api.Address(), 0, 42))
	if err != nil {
		t.Fatal(err)
	}
	events, err := c.SubmitApproveAgent(context.Background(), master.Address(), 7, api.Address(), 0, 42, sig)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/agent/approve?wait=fold" || len(events) != 1 || events[0].Kind != "AgentApproved" {
		t.Fatalf("path=%s events=%v", path, events)
	}
	want := map[string]any{"owner": master.Address().Hex(), "master": 7.0, "agent": api.Address().Hex(), "validUntilMs": 0.0, "nonce": 42.0, "signature": sig}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("字段 %s = %v, want %v", k, got[k], v)
		}
	}
}

func TestSubmitApproveAgentSurfacesNonceMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"rejected","reason":"NonceMismatch","seq":9,"sub":0,"events":[]}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, 11155111)
	owner, _ := ParseAddress("0x1111111111111111111111111111111111111111")
	agent, _ := ParseAddress("0x2222222222222222222222222222222222222222")
	_, err := c.SubmitApproveAgent(context.Background(), owner, 7, agent, 0, 42, "0x"+string(bytes.Repeat([]byte("ab"), 65)))
	var rejected *RejectedError
	if !errors.As(err, &rejected) || rejected.Reason != ReasonNonceMismatch {
		t.Fatalf("nonce 被占须原样透出 NonceMismatch: %v", err)
	}
}
