package dexos

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Config 连接参数,由网关 `/config` 给出。
//
// 它存在的理由是**这些值填错的后果都很隐蔽**:
//
//   - chainID 错 → 每笔签名 401,而盘口/K 线(只读)全部正常,
//     看起来像「只有下单坏了」。
//   - 入金地址过期(运营方重新部署过合约)→ 钱打到没人监听的合约,
//     链上扣了、账户里没有。
//
// 所以不要把它们写死在配置文件里,让客户端每次启动**问一次**。
// 运营方换了链、换了合约,你这边自动跟上。
type Config struct {
	ChainID      uint64      `json:"chainId"`
	Domain       DomainInfo  `json:"domain"`
	Deposit      DepositInfo `json:"deposit"`
	CodecVer     uint32      `json:"codecVer"`
	SnapshotVer  uint32      `json:"snapshotVer"`
	FinalityMode string      `json:"finalityMode"`
}

type DomainInfo struct {
	Name              string `json:"name"`
	Version           string `json:"version"`
	VerifyingContract string `json:"verifyingContract"`
}

type DepositInfo struct {
	// SystemGateway 入金要 transfer 到的地址。
	//
	// **可能为空** —— 网关不认识它(那是宿主/中继层概念),只有在部署信息被注入
	// 时才给得出。空值意味着「这个节点没告诉你」,不是「不需要入金地址」,
	// 此时应当向运营方索要,而不是猜一个。
	SystemGateway string      `json:"systemGateway"`
	Tokens        []TokenInfo `json:"tokens"`
}

type TokenInfo struct {
	Token            uint32 `json:"token"`
	ERC20            string `json:"erc20"`
	ExtraWeiDecimals uint8  `json:"extraWeiDecimals"`
	Finalized        bool   `json:"finalized"`
}

// Discover 从网关取连接参数。接入方**只需要知道一个 URL**。
//
//	cfg, err := dexos.Discover(ctx, "http://node.example:17807")
//	c := dexos.NewClientFromConfig("http://node.example:17807", cfg)
func Discover(ctx context.Context, baseURL string) (*Config, error) {
	c := &Client{BaseURL: baseURL, HTTP: &http.Client{Timeout: 15 * time.Second}}
	var cfg Config
	if err := c.do(ctx, http.MethodGet, "/config", nil, &cfg); err != nil {
		return nil, err
	}
	if cfg.ChainID == 0 {
		// 给了 0 比给错更危险:签出来的东西一律被拒,而错误信息只会说 401。
		return nil, fmt.Errorf("dexos: /config 未给出 chainId —— 节点版本过旧?")
	}
	return &cfg, nil
}

// NewClientFromConfig 用发现到的参数建客户端。
//
// verifyingContract 目前恒为零地址(域里保留该字段是为了与 EVM 侧的 EIP-712
// 习惯一致),真值仍以 /config 为准而不是这里写死 —— 将来它变成真合约地址时,
// 老客户端不会因为"自己记得是零地址"而全线签名失败。
func NewClientFromConfig(baseURL string, cfg *Config) *Client {
	vc, _ := ParseAddress(cfg.Domain.VerifyingContract)
	return &Client{
		BaseURL: baseURL,
		Domain:  Domain{ChainID: cfg.ChainID, VerifyingContract: vc},
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Connect = Discover + NewClientFromConfig,并核对 codec 指纹。
//
// 指纹核对不是洁癖:codec 版本决定**规范编码**,而 agent 代执行签的是编码后的
// 哈希。版本对不上时每一笔都会被拒,错误信息却只有 401 —— 在这里当场失败,
// 比在生产里查半天好。
func Connect(ctx context.Context, baseURL string, wantCodecVer uint32) (*Client, *Config, error) {
	cfg, err := Discover(ctx, baseURL)
	if err != nil {
		return nil, nil, err
	}
	if wantCodecVer != 0 && cfg.CodecVer != wantCodecVer {
		return nil, nil, fmt.Errorf(
			"dexos: codec 版本不匹配(节点 v%d,本 SDK 期望 v%d)—— 规范编码可能已变,"+
				"继续下去每笔签名都会被拒", cfg.CodecVer, wantCodecVer)
	}
	return NewClientFromConfig(baseURL, cfg), cfg, nil
}
