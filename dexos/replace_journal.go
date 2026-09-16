package dexos

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ReplaceRequest 是发送前已固定的原请求材料，不含私钥或可重放签名。
// Identity.NowMs/Nonce/摘要与后续发送完全一致；输入索引由 Cancels/Orders 保留。
// 业务 ClientID、部署纪元和执行围栏由调用方另行关联，不能从价量推断。
type ReplaceRequest struct {
	Identity AgentRequestIdentity
	Market   uint16
	Cancels  []uint64
	Orders   []BatchOrder
}

// ReplaceJournal 在同一 Session nonce 锁内提供发送前/收到结果后的持久化边界。
// BeforeSend 必须先提交原请求与逐项关联，AfterReceive 必须保留错误及已有证据。
// 两者任何失败（包括提交结果未知）均锁定会话；不得在回调中重入本 Session。
// SDK 不实现存储、跨进程锁、服务端回执查询或自动重发。
type ReplaceJournal interface {
	BeforeSend(context.Context, ReplaceRequest) error
	AfterReceive(context.Context, BatchSubmission, error) error
}

// ReplaceWithJournal 先落原请求、再单次 /replace?wait=fold，最后记录结果。
// nonce 只在场所完整成功且结果记账成功后推进；缺 journal 是本地错误，不发请求。
func (s *Session) ReplaceWithJournal(ctx context.Context, market uint16, cancels []uint64, orders []BatchOrder, journal ReplaceJournal) (BatchSubmission, error) {
	if journal == nil {
		return BatchSubmission{}, errors.New("dexos: 发送前请求日志不可用")
	}
	var result BatchSubmission
	_, err := s.write(ctx, func(n uint64) ([]EventEnvelope, error) {
		var sendErr error
		guarded := sessionReplaceJournal{session: s, target: journal, client: s.Client, until: s.ValidUntilMs}
		result, sendErr = s.Client.batchReplaceWithJournal(ctx, s.Agent, s.Account, market, cancels, orders, n, guarded)
		return result.Events(), sendErr
	})
	return result, err
}

type sessionReplaceJournal struct {
	session *Session
	target  ReplaceJournal
	client  *Client
	until   uint64
}

func (j sessionReplaceJournal) BeforeSend(ctx context.Context, request ReplaceRequest) error {
	if err := j.target.BeforeSend(ctx, request); err != nil {
		return err
	}
	if j.session.Client != j.client || j.session.Account != request.Identity.Account || j.session.Agent == nil || j.session.Agent.Address() != request.Identity.Agent || j.session.ValidUntilMs != j.until {
		return errors.New("dexos: 记账期间会话身份或授权变化，未发送")
	}
	if j.until != 0 && uint64(time.Now().UnixMilli()) >= j.until {
		return ErrAgentExpired
	}
	return nil
}

func (j sessionReplaceJournal) AfterReceive(ctx context.Context, result BatchSubmission, err error) error {
	return j.target.AfterReceive(ctx, result, err)
}

// 日志只能取得独立的证据副本，不能改写 SDK 的成功判据或返回给调用方的结果。
func copySubmission(result BatchSubmission) (BatchSubmission, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return BatchSubmission{}, err
	}
	var copied BatchSubmission
	err = json.Unmarshal(raw, &copied)
	return copied, err
}
