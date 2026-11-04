package dexosv1

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// KeyFileKind 密钥文件的 kind 字段。
const KeyFileKind = "dex-os-api-wallets"

// KeyFile 网页「API」页一次给多个子账户各建一把 API 钱包时导出的密钥文件(dex-os#335,版本 1)。
// 每把钥匙由主钱包签一份只含一位的 owner 级授权(ApproveAgentScope),只能交易 owner 名下那一个子账户。
//
// 文件里是明文私钥:只放在跑程序的机器上,权限收紧(chmod 600),不进版本库。
type KeyFile struct {
	Owner dexos.Address
	// API 导出时的网页源站(如 https://dev.example),可作网关地址的缺省值。
	API     string
	Wallets []KeyFileWallet
}

// KeyFileWallet 密钥文件里的一把 API 钱包。
type KeyFileWallet struct {
	Subaccount     uint32
	SubaccountName string
	Label          string
	Signer         *dexos.Signer
	// ValidUntil 授权到期时刻;零值 = 不过期。
	ValidUntil time.Time
}

// Account 这把钥匙要交易的账户。
func (w KeyFileWallet) Account(owner dexos.Address) AccountRef {
	return AccountRef{Address: owner, Subaccount: w.Subaccount}
}

type wireKeyFile struct {
	Kind    string `json:"kind"`
	Version int    `json:"version"`
	Owner   string `json:"owner"`
	API     string `json:"api"`
	Wallets []struct {
		Subaccount     int64  `json:"subaccount"`
		SubaccountName string `json:"subaccountName"`
		Label          string `json:"label"`
		Address        string `json:"address"`
		PrivateKey     string `json:"privateKey"`
		ValidUntilMs   int64  `json:"validUntilMs"`
	} `json:"wallets"`
}

// ParseKeyFile 解析并核对密钥文件:kind / version 对得上;owner 是地址;每把私钥推出的地址等于文件里写的地址;
// 子账户编号在 0..63 之内且互不重复(一个子账户一把)。
func ParseKeyFile(b []byte) (*KeyFile, error) {
	var w wireKeyFile
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, fmt.Errorf("dexosv1: 密钥文件不是 JSON:%w", err)
	}
	if w.Kind != KeyFileKind {
		return nil, fmt.Errorf("dexosv1: 密钥文件 kind = %q,应为 %q", w.Kind, KeyFileKind)
	}
	if w.Version != 1 {
		return nil, fmt.Errorf("dexosv1: 密钥文件 version = %d,本 SDK 只认 1", w.Version)
	}
	owner, err := dexos.ParseAddress(w.Owner)
	if err != nil {
		return nil, fmt.Errorf("dexosv1: 密钥文件 owner 不是地址:%w", err)
	}
	if len(w.Wallets) == 0 {
		return nil, errors.New("dexosv1: 密钥文件里一把钥匙都没有")
	}
	f := &KeyFile{Owner: owner, API: strings.TrimRight(w.API, "/")}
	seen := map[int64]bool{}
	for i, x := range w.Wallets {
		if x.Subaccount < 0 || x.Subaccount > 63 {
			return nil, fmt.Errorf("dexosv1: 第 %d 把钥匙的子账户编号 %d 不在 0..63", i, x.Subaccount)
		}
		if seen[x.Subaccount] {
			return nil, fmt.Errorf("dexosv1: 子账户 %d 在密钥文件里重复出现", x.Subaccount)
		}
		seen[x.Subaccount] = true
		s, err := dexos.NewSigner(x.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("dexosv1: 子账户 %d 的私钥:%w", x.Subaccount, err)
		}
		addr, err := dexos.ParseAddress(x.Address)
		if err != nil {
			return nil, fmt.Errorf("dexosv1: 子账户 %d 的地址:%w", x.Subaccount, err)
		}
		if addr != s.Address() {
			return nil, fmt.Errorf("dexosv1: 子账户 %d 写的地址 %s 不是这把私钥的地址 %s", x.Subaccount, addr.Hex(), s.Address().Hex())
		}
		var until time.Time
		if x.ValidUntilMs > 0 {
			until = time.UnixMilli(x.ValidUntilMs)
		}
		f.Wallets = append(f.Wallets, KeyFileWallet{
			Subaccount: uint32(x.Subaccount), SubaccountName: x.SubaccountName, Label: x.Label, Signer: s, ValidUntil: until,
		})
	}
	return f, nil
}
