package dexos

import (
	"errors"
	"testing"
)

// 一把 API 钱包可以被多个账户**各自**授权(主账户与各子账户分别同意)。
// 于是「这次会话代谁」不再总能推出来,而选错的代价是订单落到别的子账户上 ——
// 仓位、保证金、风险全记在别处,**而且没有任何报错**。
//
// 所以 pick 的规则只有一条:能唯一确定就用,不能就报错,**永远不挑默认值**。
func TestPick(t *testing.T) {
	g := func(master uint32, expired bool) Grant {
		return Grant{Master: master, ValidUntilMs: "0", Expired: expired}
	}

	t.Run("唯一授权:不必指明", func(t *testing.T) {
		got, err := pick([]Grant{g(7, false)}, newOpts{})
		if err != nil || got.Master != 7 {
			t.Fatalf("要 7,得到 %v / %v", got, err)
		}
	})

	t.Run("多个授权且未指明:报歧义而不是挑一个", func(t *testing.T) {
		_, err := pick([]Grant{g(2, false), g(7, false)}, newOpts{})
		if !errors.Is(err, ErrAmbiguousAccount) {
			t.Fatalf("要 ErrAmbiguousAccount,得到 %v", err)
		}
		// 报错里要带候选,否则用户不知道该填什么
		if !contains(err.Error(), "2") || !contains(err.Error(), "7") {
			t.Fatalf("报错应列出候选账户,实际:%v", err)
		}
	})

	t.Run("多个授权 + 指明:用指明的那个", func(t *testing.T) {
		got, err := pick([]Grant{g(2, false), g(7, false)}, newOpts{account: 7, hasAccount: true})
		if err != nil || got.Master != 7 {
			t.Fatalf("要 7,得到 %v / %v", got, err)
		}
	})

	// 过期的先剔除再判定:主账户过期、子账户仍有效时,正确行为是直接用子账户,
	// 而不是报歧义、让人去指明一个本来就用不了的账户。
	t.Run("过期的不参与歧义判定", func(t *testing.T) {
		got, err := pick([]Grant{g(2, true), g(7, false)}, newOpts{})
		if err != nil || got.Master != 7 {
			t.Fatalf("要 7,得到 %v / %v", got, err)
		}
	})

	t.Run("全部过期:说清是过期而不是没授权", func(t *testing.T) {
		_, err := pick([]Grant{g(2, true)}, newOpts{})
		if err == nil || errors.Is(err, ErrAgentNotAuthorized) {
			t.Fatalf("要「已过期」,得到 %v", err)
		}
		if !contains(err.Error(), "过期") {
			t.Fatalf("报错要点明过期,实际:%v", err)
		}
	})

	t.Run("一条授权都没有", func(t *testing.T) {
		if _, err := pick(nil, newOpts{}); !errors.Is(err, ErrAgentNotAuthorized) {
			t.Fatalf("要 ErrAgentNotAuthorized,得到 %v", err)
		}
	})

	// 「没授权」与「授权过期」的处置不同:前者去前端授权,后者去续期。
	// 合并成一句「不可用」会让人往错的方向查。
	t.Run("指定的账户未授权 vs 已过期,区分开", func(t *testing.T) {
		_, err := pick([]Grant{g(2, false)}, newOpts{account: 9, hasAccount: true})
		if err == nil || !contains(err.Error(), "没有授权") {
			t.Fatalf("要「没有授权」,实际:%v", err)
		}
		_, err = pick([]Grant{g(9, true)}, newOpts{account: 9, hasAccount: true})
		if err == nil || !contains(err.Error(), "过期") {
			t.Fatalf("要「已过期」,实际:%v", err)
		}
	})
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
