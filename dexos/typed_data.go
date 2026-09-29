package dexos

import (
	"strconv"
	"strings"
)

// TypedData 钱包可签的 EIP-712 结构,即 eth_signTypedData_v4 收的 JSON。
//
// 给「主账号密钥不在本进程」的接入用:服务端生成 API 钱包,把授权结构交给浏览器钱包签,
// 拿回签名后用 SubmitApproveAgent 提交。它与 ApproveAgentHash 同源 —— 字段、顺序、
// 类型串任何一处漂移,钱包签出来的东西服务端都验不过,而报错只有 401。
//
// message 里的整数一律是十进制字符串:uint64 超出 JS 安全整数范围时按数字传会被静默截断,
// 签出的就是另一个值。钱包与 EIP-712 实现都接受字符串形式的整数。
type TypedData struct {
	Types       map[string][]TypedField `json:"types"`
	PrimaryType string                  `json:"primaryType"`
	Domain      TypedDomain             `json:"domain"`
	Message     map[string]any          `json:"message"`
}

type TypedField struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// TypedDomain 与 Domain 一一对应。chainId 是数字:钱包拿它与当前网络比对,不一致即拒签。
type TypedDomain struct {
	Name              string `json:"name"`
	Version           string `json:"version"`
	ChainID           uint64 `json:"chainId"`
	VerifyingContract string `json:"verifyingContract"`
}

// 字段表与 eip712.go 的类型串逐字对应,typed_data_test.go 核对两者不漂移。
var (
	domainFields = []TypedField{
		{Name: "name", Type: "string"}, {Name: "version", Type: "string"},
		{Name: "chainId", Type: "uint256"}, {Name: "verifyingContract", Type: "address"},
	}
	approveAgentFields = []TypedField{
		{Name: "owner", Type: "address"}, {Name: "master", Type: "uint32"}, {Name: "agent", Type: "address"},
		{Name: "validUntilMs", Type: "uint64"}, {Name: "nonce", Type: "uint64"},
	}
	revokeAgentFields = []TypedField{
		{Name: "owner", Type: "address"}, {Name: "master", Type: "uint32"}, {Name: "agent", Type: "address"},
		{Name: "nonce", Type: "uint64"},
	}
)

// encodeType 按 EIP-712 规则把字段表拼成类型串,如 ApproveAgent(address owner,...)。
func encodeType(name string, fields []TypedField) string {
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = f.Type + " " + f.Name
	}
	return name + "(" + strings.Join(parts, ",") + ")"
}

func (d Domain) typedData(primary string, fields []TypedField, message map[string]any) TypedData {
	return TypedData{
		Types:       map[string][]TypedField{"EIP712Domain": domainFields, primary: fields},
		PrimaryType: primary,
		Domain:      TypedDomain{Name: d.Name, Version: d.Version, ChainID: d.ChainID, VerifyingContract: d.VerifyingContract.Hex()},
		Message:     message,
	}
}

// ApproveAgentTypedData 「授权 API 钱包」的钱包待签结构;签名结果交给 SubmitApproveAgent。
//
// validUntilMs 为 0 表示永不过期。nonce 取 master 账户的计数器(NextNonce),且必须恰好是
// 下一个值:签完到提交之间,主账号若做了别的签名操作,这份签名就作废。
func (d Domain) ApproveAgentTypedData(owner Address, master uint32, agent Address, validUntilMs, nonce uint64) TypedData {
	return d.typedData("ApproveAgent", approveAgentFields, map[string]any{
		"owner":        owner.Hex(),
		"master":       strconv.FormatUint(uint64(master), 10),
		"agent":        agent.Hex(),
		"validUntilMs": strconv.FormatUint(validUntilMs, 10),
		"nonce":        strconv.FormatUint(nonce, 10),
	})
}

// RevokeAgentTypedData 「撤销 API 钱包」的钱包待签结构;签名结果交给 SubmitRevokeAgent。
func (d Domain) RevokeAgentTypedData(owner Address, master uint32, agent Address, nonce uint64) TypedData {
	return d.typedData("RevokeAgent", revokeAgentFields, map[string]any{
		"owner":  owner.Hex(),
		"master": strconv.FormatUint(uint64(master), 10),
		"agent":  agent.Hex(),
		"nonce":  strconv.FormatUint(nonce, 10),
	})
}
