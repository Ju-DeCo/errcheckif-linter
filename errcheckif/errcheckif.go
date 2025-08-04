package errcheckif

import (
	"go/ast"
	"go/token"
	"go/types"
	"golang.org/x/tools/go/ast/astutil"

	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

const doc = `checks that errors returned from functions are checked

The errcheckif checker ensures that whenever a function call returns an error,
that error is checked in a subsequent if statement using "err != nil", "err == nil",
"errors.Is", or "errors.As".`

// 通过 register.Plugin 将 linter 构造函数注册到 golangci-lint 的插件系统中
func init() {
	// "errcheckif" 是在 .golangci.yml 中使用的 linter 名称
	register.Plugin("errcheckif", New)
}

// ErrCheckIfPlugin 用来保存从 .golangci.yml 传来的配置
type ErrCheckIfPlugin struct{}

// New 是 linter 的构造函数，golangci-lint 会调用它
func New(settings any) (register.LinterPlugin, error) {
	// 如果 linter 需要从 .golangci.yml 中读取配置，可以在这里解码。
	// 例如: `register.DecodeSettings[MySettings](settings)`
	// 因为我们没有配置，所以直接返回实例。
	return &ErrCheckIfPlugin{}, nil
}

// BuildAnalyzers 返回该插件提供的所有 analysis.Analyzer 实例
func (p *ErrCheckIfPlugin) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	return []*analysis.Analyzer{
		{
			Name:     "errcheckif",
			Doc:      doc,
			Requires: []*analysis.Analyzer{inspect.Analyzer},
			Run:      run, // 核心检查逻辑的函数
		},
	}, nil
}

// GetLoadMode 告诉 golangci-lint 如何加载代码
func (p *ErrCheckIfPlugin) GetLoadMode() string {
	// 因为我们需要检查变量是否为 `error` 类型，以及 `nil` 的定义，
	// 所以必须使用 `LoadModeTypesInfo` 来获取完整的类型信息。
	// 如果 linter 只做语法检查（如检查注释格式），可以使用更快的 `LoadModeSyntax`。
	return register.LoadModeTypesInfo
}

var errorType = types.Universe.Lookup("error").Type().Underlying().(*types.Interface)

func run(pass *analysis.Pass) (interface{}, error) {
	inspector := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	nodeFilter := []ast.Node{(*ast.AssignStmt)(nil)}

	inspector.Preorder(nodeFilter, func(node ast.Node) {
		assignStmt, ok := node.(*ast.AssignStmt)
		if !ok {
			return
		}

		if len(assignStmt.Rhs) != 1 {
			return
		}
		callExpr, ok := assignStmt.Rhs[0].(*ast.CallExpr)
		if !ok {
			return
		}

		errIdent := findReturnedError(pass, assignStmt, callExpr)
		if errIdent == nil {
			return
		}

		path, _ := astutil.PathEnclosingInterval(findFile(pass, assignStmt), assignStmt.Pos(), assignStmt.End())
		if path == nil {
			return
		}

		if isHandledInIfInit(pass, errIdent, path) {
			return
		}

		if !isHandledInSubsequentStatement(pass, errIdent, path) {
			pass.Reportf(errIdent.Pos(), "error '%s' is not checked or returned", errIdent.Name)
		}
	})

	return nil, nil
}

func isHandledInIfInit(pass *analysis.Pass, errIdent *ast.Ident, path []ast.Node) bool {
	if len(path) < 2 {
		return false
	}
	ifStmt, ok := path[1].(*ast.IfStmt)
	if !ok || ifStmt.Init != path[0] {
		return false
	}
	return checkCondition(pass, ifStmt.Cond, errIdent)
}

func isHandledInSubsequentStatement(pass *analysis.Pass, errIdent *ast.Ident, path []ast.Node) bool {
	for i, node := range path {
		if block, ok := node.(*ast.BlockStmt); ok {
			if i > 0 {
				for stmtIdx, stmt := range block.List {
					if stmt == path[i-1] {
						for j := stmtIdx + 1; j < len(block.List); j++ {
							subsequentStmt := block.List[j]
							if isStmtAValidHandler(pass, subsequentStmt, errIdent) {
								return true
							}
							if isIdentifierReassigned(pass, subsequentStmt, errIdent) {
								return false
							}
						}
						break
					}
				}
			}
		}
	}
	return false
}

// isStmtAValidHandler 检查一个语句是否是有效的错误处理器 (if 或 return)。
func isStmtAValidHandler(pass *analysis.Pass, stmt ast.Node, errIdent *ast.Ident) bool {
	// Case 1: 检查是否是 if 语句
	if ifStmt, ok := stmt.(*ast.IfStmt); ok {
		return checkCondition(pass, ifStmt.Cond, errIdent)
	}

	// Case 2: 检查是否是 return 语句
	if returnStmt, ok := stmt.(*ast.ReturnStmt); ok {
		for _, result := range returnStmt.Results {
			if retIdent, ok := result.(*ast.Ident); ok {
				if pass.TypesInfo.ObjectOf(retIdent) == pass.TypesInfo.ObjectOf(errIdent) {
					return true
				}
			}
		}
	}

	return false
}

func findReturnedError(pass *analysis.Pass, assign *ast.AssignStmt, call *ast.CallExpr) *ast.Ident {
	sig, ok := pass.TypesInfo.TypeOf(call.Fun).(*types.Signature)
	if !ok {
		return nil
	}
	results := sig.Results()
	if results.Len() == 0 {
		return nil
	}
	for i := 0; i < results.Len(); i++ {
		if types.Implements(results.At(i).Type(), errorType) {
			if i < len(assign.Lhs) {
				if ident, ok := assign.Lhs[i].(*ast.Ident); ok && ident.Name != "_" {
					return ident
				}
			}
		}
	}
	return nil
}

func checkCondition(pass *analysis.Pass, cond ast.Expr, errIdent *ast.Ident) bool {
	switch c := cond.(type) {
	case *ast.BinaryExpr:
		if c.Op == token.LOR {
			return checkCondition(pass, c.X, errIdent) || checkCondition(pass, c.Y, errIdent)
		}
		if c.Op == token.NEQ || c.Op == token.EQL {
			if isIdent(pass, c.X, errIdent) && isNil(pass, c.Y) {
				return true
			}
			if isNil(pass, c.X) && isIdent(pass, c.Y, errIdent) {
				return true
			}
		}
	case *ast.CallExpr:
		sel, ok := c.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		if pkgIdent, ok := sel.X.(*ast.Ident); !ok || pkgIdent.Name != "errors" {
			return false
		}
		if sel.Sel.Name != "Is" && sel.Sel.Name != "As" {
			return false
		}
		if len(c.Args) > 0 && isIdent(pass, c.Args[0], errIdent) {
			return true
		}
	}
	return false
}

func isIdentifierReassigned(pass *analysis.Pass, stmt ast.Node, errIdent *ast.Ident) bool {
	targetObj := pass.TypesInfo.ObjectOf(errIdent)
	if targetObj == nil {
		return false
	}
	reassigned := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range assign.Lhs {
			ident, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			if pass.TypesInfo.ObjectOf(ident) == targetObj {
				reassigned = true
				return false
			}
		}
		return true
	})
	return reassigned
}

func isIdent(pass *analysis.Pass, expr ast.Expr, targetIdent *ast.Ident) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && pass.TypesInfo.ObjectOf(ident) == pass.TypesInfo.ObjectOf(targetIdent)
}

func isNil(pass *analysis.Pass, expr ast.Expr) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && pass.TypesInfo.ObjectOf(ident) == types.Universe.Lookup("nil")
}

func findFile(pass *analysis.Pass, node ast.Node) *ast.File {
	for _, file := range pass.Files {
		if file.Pos() <= node.Pos() && node.End() <= file.End() {
			return file
		}
	}
	return nil
}
