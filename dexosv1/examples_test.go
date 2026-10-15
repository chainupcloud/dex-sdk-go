package dexosv1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

// 旧 dexos 包里走旧公开端点的入口(已标 Deprecated)。dex-os 在退役那些端点(public-api-v1.md §9),
// 示例照着抄的人会把旧接口带进新接入,所以示例只许用 dexosv1;dexos 里的签名、地址、编码类型照常可用。
var legacyEntryPoints = map[string]bool{
	"Client": true, "NewClient": true, "NewClientFromConfig": true, "Connect": true, "ConnectUnchecked": true,
	"Discover": true, "Session": true, "New": true, "ForAccount": true, "ForVault": true,
}

func TestExamplesUseV1Only(t *testing.T) {
	files, err := filepath.Glob("../examples/*/*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("没找到 examples")
	}
	for _, f := range files {
		fs := token.NewFileSet()
		file, err := parser.ParseFile(fs, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		// dexos 包在本文件里的名字(没有改名导入就是 dexos)
		alias := ""
		for _, imp := range file.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == "github.com/chainupcloud/dex-sdk-go/dexos" {
				alias = "dexos"
				if imp.Name != nil {
					alias = imp.Name.Name
				}
			}
		}
		if alias == "" {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == alias && legacyEntryPoints[sel.Sel.Name] {
				t.Errorf("%s:用了走旧接口的 dexos.%s,改用 dexosv1", fs.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
}
