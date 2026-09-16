package dexos

import (
	"encoding/json"
	"errors"
)

// ErrExecutionPending 表示场所明确声明本请求尚未完成折叠，不能当作执行成功。
var ErrExecutionPending = errors.New("dexos: 请求已定序但执行尚未完成")

// AgentRequestIdentity 是本次命令的本地签名身份，不含私钥或可重放的签名。
// 域不等于部署/内核纪元；这些字段不代表服务端已有按身份查询的接口。
type AgentRequestIdentity struct {
	Agent        Address
	Account      uint32
	Nonce        uint64
	NowMs        uint64
	CodecVersion uint32
	Domain       Domain
	CommandHash  string
	SigningHash  string
}

// WriteReceipt 保留实际响应；可选字段为 nil 表示未提供，不填成合法的0/false。
// 聚合统计不是逐项回执，seq/sub 不是 Fill 身份；读取本结构仍必须同时检查 error。
type WriteReceipt struct {
	Status           string          `json:"status"`
	Seq              *uint64         `json:"seq"`
	Sub              *uint16         `json:"sub,omitempty"`
	Folded           *bool           `json:"folded,omitempty"`
	Reason           *string         `json:"reason,omitempty"`
	Submitted        *uint64         `json:"submitted,omitempty"`
	SubmittedCancels *uint64         `json:"submittedCancels,omitempty"`
	Accepted         *uint64         `json:"accepted,omitempty"`
	Canceled         *uint64         `json:"canceled,omitempty"`
	Rejected         *uint64         `json:"rejected,omitempty"`
	BatchStatus      *string         `json:"batchStatus,omitempty"`
	Events           []EventEnvelope `json:"events"`
}

// BatchSubmission 是一次调用已取得的证据，不是持久日志或发送前回调。
// Request=nil 表示此调用未建立签名身份（例如已被会话阻断），不证明前次请求未执行。
// Receipt=nil 表示未取得可解析回执；Request非nil也不等于请求已送达或被接受。
type BatchSubmission struct {
	Request *AgentRequestIdentity
	Receipt *WriteReceipt
}

func (r BatchSubmission) Events() []EventEnvelope {
	if r.Receipt == nil {
		return nil
	}
	return r.Receipt.Events
}

type writeResp struct {
	WriteReceipt
	decoded bool
}

// 完整解析后才留下回执。null、类型错和坏 JSON 不能暴露一个部分解码的伪回执。
func (r *writeResp) UnmarshalJSON(raw []byte) error {
	var parsed *WriteReceipt
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return err
	}
	if parsed == nil {
		return errors.New("dexos: 写回执必须为对象")
	}
	r.WriteReceipt, r.decoded = *parsed, true
	return nil
}

func (r *writeResp) evidence() *WriteReceipt {
	if !r.decoded {
		return nil
	}
	return &r.WriteReceipt
}
