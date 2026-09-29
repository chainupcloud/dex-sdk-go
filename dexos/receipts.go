package dexos

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// 按原请求身份查权威回执(dex-os #3,GET /receipts/{requestId})。
//
// 写请求结果未知时(超时、断线、folded:false),**唯一**正确的恢复是拿本地原请求的身份去问:
// 不能重读 nonce 猜,也不能换 nonce 重发 —— 前者分不清「没执行」与「执行了但回执丢了」,
// 后者会把同一笔意图执行两次。请求身份 = keccak256(签名者 20 字节 ‖ EIP-712 摘要 32 字节),
// 由签名承诺推导,本地发送前就能算出([AgentRequestIdentity.RequestID])。

// ReceiptStatus 回执结论。六种都是**答案**,不是错误。
type ReceiptStatus string

const (
	// ReceiptExecuted 已执行(nonce 已消耗);Items/Events 是它的逐项结局与事件。
	ReceiptExecuted ReceiptStatus = "executed"
	// ReceiptRejected 这一次投递被拒。NonceConsumed=true 是终局业务拒绝;NonceConsumed=false
	// 表示只在认证层被拒、nonce 未动 —— 代理签名没有过期窗口,同一份签名以后仍可能被定序,
	// 这**不是**终局。
	ReceiptRejected ReceiptStatus = "rejected"
	// ReceiptPending 已定序、结论尚未持久 —— 稍后再问,不能重发。
	ReceiptPending ReceiptStatus = "pending"
	// ReceiptNotFound 截至 AsOfSeq 这个身份从未定序。依据是完整历史,或(Basis="agent_nonce")
	// 查询带了 commandHash、截至 AsOfSeq 代理的下一个 nonce(NextNonce)还没走到查询的 nonce(dex-os #17)。
	// **不是终局**:代理签名没有过期窗口,nonce 被消耗之前同一请求随时可能被定序;
	// 只有同一 nonce 已被别的请求用掉(ReceiptConflict)才证明它不会再执行。
	ReceiptNotFound ReceiptStatus = "not_found"
	// ReceiptConflict 查询带的 nonce 已被**另一个**请求用掉(ConflictingRequest)。
	ReceiptConflict ReceiptStatus = "conflict"
	// ReceiptHistoryUnavailable 本副本历史有缺口,证明不了有没有 —— 不是 not_found。
	ReceiptHistoryUnavailable ReceiptStatus = "history_unavailable"
)

// RequestReceipt 一次回执查询的结论。可空字段 nil = 该结论不带这一项。
type RequestReceipt struct {
	Status             ReceiptStatus `json:"status"`
	RequestID          string        `json:"requestId"`
	Epoch              string        `json:"epoch"`
	Seq                *uint64       `json:"seq"`
	Sub                *uint16       `json:"sub"`
	NonceConsumed      *bool         `json:"nonceConsumed"`
	Events             []Event       `json:"events"`
	Items              []BatchItem   `json:"items"`
	Reason             *string       `json:"reason"`
	Source             *string       `json:"source"`
	Signer             *string       `json:"signer"`
	NonceScope         *string       `json:"nonceScope"`
	Nonce              *uint64       `json:"nonce"`
	AsOfSeq            *uint64       `json:"asOfSeq"`
	Basis              *string       `json:"basis"`
	NextNonce          *uint64       `json:"nextNonce"`
	ConflictingRequest *string       `json:"conflictingRequest"`
	Gaps               []Gap         `json:"gaps"`
}

// ReceiptQuery 可选的 nonce 冲突检查:Scope 形如 `agent:0x…` / `account:<id>` / `owner:0x…`,
// 与 Nonce 成对给出;查不到终局时服务端会先看这个 nonce 是否已被别的请求用掉。
//
// CommandHash(0x + 64 位十六进制,= keccak256(规范命令编码))只配 agent scope + Nonce:历史有缺口时,
// 服务端凭它重算请求身份,再以代理 nonce 计数证明这个请求从未执行(dex-os #17)。
type ReceiptQuery struct {
	Scope       string
	Nonce       *uint64
	CommandHash string
}

// Receipt 按请求身份查回执。存储故障、请求错误、结论与状态码对不上都是 error。
func (c *Client) Receipt(ctx context.Context, requestID string, q ReceiptQuery) (*RequestReceipt, error) {
	if _, err := decodeHash32(requestID); err != nil {
		return nil, fmt.Errorf("dexos: 请求身份不合法: %w", err)
	}
	if (q.Scope == "") != (q.Nonce == nil) {
		return nil, errors.New("dexos: 回执查询的 scope 与 nonce 必须成对")
	}
	if q.CommandHash != "" {
		if !strings.HasPrefix(q.Scope, "agent:") {
			return nil, errors.New("dexos: commandHash 只能配 agent scope 与 nonce")
		}
		if _, err := decodeHash32(q.CommandHash); err != nil {
			return nil, fmt.Errorf("dexos: commandHash 不合法: %w", err)
		}
	}
	path := "/receipts/" + requestID
	if q.Scope != "" {
		values := url.Values{"scope": {q.Scope}, "nonce": {strconv.FormatUint(*q.Nonce, 10)}}
		if q.CommandHash != "" {
			values.Set("commandHash", q.CommandHash)
		}
		path += "?" + values.Encode()
	}
	status, raw, err := c.roundTrip(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var r RequestReceipt
	if json.Unmarshal(raw, &r) != nil {
		return nil, classifyAPIError(&APIError{Status: status, Body: string(raw)})
	}
	allowed := map[int][]ReceiptStatus{
		http.StatusOK:                 {ReceiptExecuted, ReceiptRejected, ReceiptPending},
		http.StatusNotFound:           {ReceiptNotFound},
		http.StatusConflict:           {ReceiptConflict},
		http.StatusServiceUnavailable: {ReceiptHistoryUnavailable},
	}
	ok := false
	for _, s := range allowed[status] {
		ok = ok || s == r.Status
	}
	if !ok {
		if status >= 200 && status < 300 {
			return nil, fmt.Errorf("dexos: 回执结论 %q 不在契约内", r.Status)
		}
		return nil, classifyAPIError(&APIError{Status: status, Body: string(raw)})
	}
	if r.Epoch == "" || (r.RequestID != "" && !strings.EqualFold(r.RequestID, requestID)) {
		return nil, fmt.Errorf("dexos: 回执缺纪元或请求身份不符(问 %s,答 %q)", requestID, r.RequestID)
	}
	switch r.Status {
	case ReceiptExecuted, ReceiptRejected:
		if r.Seq == nil || r.Sub == nil || r.NonceConsumed == nil {
			return nil, errors.New("dexos: 终局回执缺 seq/sub/nonceConsumed")
		}
	case ReceiptPending:
		if r.Seq == nil || r.Sub == nil {
			return nil, errors.New("dexos: pending 回执缺 seq/sub")
		}
	case ReceiptNotFound:
		if r.AsOfSeq == nil {
			return nil, errors.New("dexos: not_found 回执缺 asOfSeq")
		}
	}
	return &r, nil
}

// ReceiptOf 按本地原请求身份查回执(agent scope + 原 nonce + 命令哈希),并核对回执声称的签名者与 nonce。
//
// epoch 是原请求**发送前**取得的节点纪元(来自发送前某次历史调用的 Epoch),必填:节点换了纪元后,
// 旧纪元里执行过的请求在新纪元会显示成 not_found,不核纪元就会把「已执行」读成「没执行」;
// 发送后才取纪元同样挡不住这一条。
func (c *Client) ReceiptOf(ctx context.Context, id AgentRequestIdentity, epoch string) (*RequestReceipt, error) {
	if epoch == "" {
		return nil, errors.New("dexos: 按原请求查回执必须给出原请求所在的纪元")
	}
	requestID, err := id.RequestID()
	if err != nil {
		return nil, err
	}
	if _, err := decodeHash32(id.CommandHash); err != nil {
		return nil, fmt.Errorf("dexos: 原请求 CommandHash 不合法: %w", err)
	}
	nonce := id.Nonce
	r, err := c.Receipt(ctx, requestID, ReceiptQuery{Scope: "agent:" + id.Agent.Hex(), Nonce: &nonce, CommandHash: id.CommandHash})
	if err != nil {
		return nil, err
	}
	if r.Epoch != epoch {
		return nil, fmt.Errorf("%w: 回执纪元 %s 不是原请求所在的 %s", ErrHistoryEpochMismatch, r.Epoch, epoch)
	}
	if r.Signer != nil && !strings.EqualFold(*r.Signer, id.Agent.Hex()) {
		return nil, fmt.Errorf("dexos: 回执签名者 %s 不是原请求的代理 %s", *r.Signer, id.Agent.Hex())
	}
	if r.Nonce != nil && *r.Nonce != id.Nonce {
		return nil, fmt.Errorf("dexos: 回执 nonce %d 不是原请求的 %d", *r.Nonce, id.Nonce)
	}
	return r, nil
}

// CheckReplace 用与写回执同一套逐项判据核对一次 /replace 的权威回执(见 checkBatchOutcome):
// nil = 全部生效;*BatchOutcomeError = 终局部分成功;其余错误(含 ErrIncompleteBatch)= 证据不可信。
//
// 回执必须回显这个请求的身份(requestId,给了 nonce 也要相符):两笔逐项结果一模一样的换单
// 不能互相冒充。executed 且 nonce 已消耗是批次结局;整批被业务拒绝(rejected 且 nonceConsumed=true)
// 带逐项结果时同样是逐项结局(*BatchOutcomeError,Whole 带整批拒因),不带逐项结果时报错,
// 由调用方按拒绝处理。
func (r *RequestReceipt) CheckReplace(req ReplaceRequest) error {
	want, err := req.Identity.RequestID()
	if err != nil {
		return err
	}
	if !strings.EqualFold(r.RequestID, want) || (r.Nonce != nil && *r.Nonce != req.Identity.Nonce) {
		return fmt.Errorf("dexos: 回执身份 %q 不是这个请求的 %s", r.RequestID, want)
	}
	whole := r.Status == ReceiptRejected && len(r.Items) > 0
	if (r.Status != ReceiptExecuted && !whole) || r.NonceConsumed == nil || !*r.NonceConsumed {
		return fmt.Errorf("dexos: 回执结论 %q 不是已执行的批次结局", r.Status)
	}
	cancels := make([]OrderID, len(req.Cancels))
	for i, seq := range req.Cancels {
		cancels[i] = NewOrderID(req.Market, seq)
	}
	events := make([]EventEnvelope, len(r.Events))
	for i, e := range r.Events {
		events[i] = EventEnvelope{Kind: e.Kind, Data: e.Data}
	}
	receipt := WriteReceipt{Items: r.Items, Events: events}
	if whole {
		re := &RejectedError{Reason: "Rejected"}
		if r.Reason != nil && *r.Reason != "" {
			re.Reason = *r.Reason
		}
		if r.Seq != nil {
			re.Seq = *r.Seq
		}
		if r.Sub != nil {
			re.Sub = *r.Sub
		}
		return checkWholeRejected(receipt, re, req.Identity.Account, req.Orders, cancels)
	}
	return checkBatchOutcome(receipt, req.Identity.Account, req.Orders, cancels)
}

// RequestID 服务端对这次请求的身份:keccak256(代理地址 ‖ SigningHash)。
// 与 dex-os crates/kernel/src/engine/authn.rs `SignedRequest::request_id` 同一定义。
func (id AgentRequestIdentity) RequestID() (string, error) {
	digest, err := decodeHash32(id.SigningHash)
	if err != nil {
		return "", fmt.Errorf("dexos: SigningHash 不合法: %w", err)
	}
	return "0x" + hex.EncodeToString(Keccak256(id.Agent[:], digest)), nil
}

func decodeHash32(s string) ([]byte, error) {
	h, ok := strings.CutPrefix(s, "0x")
	if !ok || len(h) != 64 {
		return nil, errors.New("应为 0x + 64 位十六进制")
	}
	return hex.DecodeString(h)
}
