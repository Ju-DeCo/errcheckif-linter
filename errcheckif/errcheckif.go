package errcheckif

import (
	"go/ast"
	"go/token"
	"go/types"
	"golang.org/x/tools/go/ast/astutil"
	"strings"

	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

const doc = `checks that errors returned from functions are checked

The errcheckif checker ensures that whenever a function call returns an error,
that error is checked in a subsequent if statement, returned directly, or used in an if-init statement.`

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

// 缓存 Go 语言中预定义的 error 接口类型
var errorType = types.Universe.Lookup("error").Type().Underlying().(*types.Interface)

func run(pass *analysis.Pass) (interface{}, error) { // pass 对象是分析过程的上下文

	// 获取预先构建好的 inspector 实例
	inspector := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	// 指定 只访问 AST 中的赋值语句节点
	nodeFilter := []ast.Node{(*ast.AssignStmt)(nil)}

	// 遍历 AST 中的 nodeFilter 的指定节点
	inspector.Preorder(nodeFilter, func(node ast.Node) {

		// 跳过测试文件的检测
		pos := node.Pos()
		// pass.Fset 是一个文件集
		file := pass.Fset.File(pos)
		// 获取文件名，以 _test.go 结尾，则直接返回
		if file != nil && strings.HasSuffix(file.Name(), "_test.go") {
			return
		}

		assignStmt, ok := node.(*ast.AssignStmt)
		if !ok {
			return
		}

		// 赋值语句右侧必须是函数调用
		if len(assignStmt.Rhs) != 1 {
			return
		}
		callExpr, ok := assignStmt.Rhs[0].(*ast.CallExpr)
		if !ok {
			return
		}

		// 检查该函数调用是否返回了 error，并获取 error 变量的标识符（*ast.Ident）
		errIdent := findReturnedError(pass, assignStmt, callExpr)
		if errIdent == nil {
			return
		}

		// 返回一个从当前节点 (assignStmt) 到 AST 根节点的路径
		path, _ := astutil.PathEnclosingInterval(findFile(pass, assignStmt), assignStmt.Pos(), assignStmt.End())
		if path == nil {
			return
		}

		// 检查这个赋值是不是一个 if-init 语句的一部分
		if isHandledInIfInit(pass, errIdent, path) {
			return
		}

		// 检查是否在后续的语句中被处理（if 或 return）
		if !isHandledInSubsequentStatement(pass, errIdent, path) {
			pass.Reportf(errIdent.Pos(), "error '%s' is not checked or returned", errIdent.Name)
		}
	})

	return nil, nil
}

// isHandledInIfInit 检测是否是 if-init 模式
func isHandledInIfInit(pass *analysis.Pass, errIdent *ast.Ident, path []ast.Node) bool {
	if len(path) < 2 {
		return false
	}
	// 断言它是一个 if 语句 （*ast.IfStmt）
	ifStmt, ok := path[1].(*ast.IfStmt)
	if !ok || ifStmt.Init != path[0] {
		return false
	}
	return checkCondition(pass, ifStmt.Cond, errIdent)
}

// isHandledInSubsequentStatement 检查错误是否在后续的独立语句中被处理
func isHandledInSubsequentStatement(pass *analysis.Pass, errIdent *ast.Ident, path []ast.Node) bool {
	for i := 1; i < len(path); i++ {
		// 尝试从当前父节点获取语句列表
		stmtList := getStmtList(path[i])
		if stmtList == nil {
			continue
		}

		// 在这个语句列表中，找到我们关心的那个语句（即赋值语句的父语句）
		for stmtIdx, stmt := range stmtList {
			if stmt == path[i-1] {
				for j := stmtIdx + 1; j < len(stmtList); j++ {
					subsequentStmt := stmtList[j]
					if isStmtAValidHandler(pass, subsequentStmt, errIdent) {
						return true
					}
					if isIdentifierReassigned(pass, subsequentStmt, errIdent) {
						return false
					}
				}
				return false
			}
		}
	}
	return false
}

// getStmtList 从一个 AST 节点中提取出其包含的语句列表
// 泛化处理 *ast.BlockStmt, *ast.CaseClause (用于 switch), 和 *ast.CommClause (用于 select)。
func getStmtList(node ast.Node) []ast.Stmt {
	switch n := node.(type) {
	case *ast.BlockStmt:
		return n.List
	case *ast.CaseClause:
		return n.Body
	// 增加对 select 语句中 case 的处理
	case *ast.CommClause:
		return n.Body
	}
	return nil
}

// isStmtAValidHandler 检查一个语句是否有效进行错误处理 (if 或 return)
func isStmtAValidHandler(pass *analysis.Pass, stmt ast.Node, errIdent *ast.Ident) bool {
	// Case 1: 检查是否是 if 语句
	if ifStmt, ok := stmt.(*ast.IfStmt); ok {
		return checkCondition(pass, ifStmt.Cond, errIdent)
	}

	// Case 2: 检查是否是 return 语句
	if returnStmt, ok := stmt.(*ast.ReturnStmt); ok {
		// 遍历 return 语句的所有返回值
		for _, result := range returnStmt.Results {
			// 检查返回的表达式是否就是我们追踪的那个 err 变量
			if retIdent, ok := result.(*ast.Ident); ok {
				if pass.TypesInfo.ObjectOf(retIdent) == pass.TypesInfo.ObjectOf(errIdent) {
					return true
				}
			}
		}
	}

	return false
}

// findReturnedError 查找赋值语句右侧的函数调用是否返回 error，并返回对应的左侧变量
func findReturnedError(pass *analysis.Pass, assign *ast.AssignStmt, call *ast.CallExpr) *ast.Ident {
	// 获取函数调用的类型签名
	sig, ok := pass.TypesInfo.TypeOf(call.Fun).(*types.Signature)
	if !ok {
		return nil
	}
	// 获取返回结果列表
	results := sig.Results()
	if results.Len() == 0 {
		return nil
	}
	for i := 0; i < results.Len(); i++ {
		// types.Implements 检查该返回值的类型是否实现了 error 接口
		if types.Implements(results.At(i).Type(), errorType) {
			if i < len(assign.Lhs) {
				// 如果变量不是 _ (空白标识符)，就返回它
				if ident, ok := assign.Lhs[i].(*ast.Ident); ok && ident.Name != "_" {
					return ident
				}
			}
		}
	}
	return nil
}

// checkCondition 检查 if 条件表达式是否满足给定规则
func checkCondition(pass *analysis.Pass, cond ast.Expr, errIdent *ast.Ident) bool {
	switch c := cond.(type) {
	// 情况1: 二元表达式, 如 err != nil
	case *ast.BinaryExpr:
		// 如果是逻辑或 || (LOR) 逻辑与 && (LAND)，则递归地检查左右两边
		if c.Op == token.LOR || c.Op == token.LAND {
			return checkCondition(pass, c.X, errIdent) || checkCondition(pass, c.Y, errIdent)
		}
		// 如果是 != (NEQ) 或 == (EQL)，检查是不是 err 和 nil 在进行比较
		if c.Op == token.NEQ || c.Op == token.EQL {
			if isIdent(pass, c.X, errIdent) && isNil(pass, c.Y) {
				return true
			}
			if isNil(pass, c.X) && isIdent(pass, c.Y, errIdent) {
				return true
			}
		}
	// 情况2: 函数调用, 如 errors.Is(err, ...)
	case *ast.CallExpr:
		// errors.Is 在 AST 中是一个选择器表达式 (*ast.SelectorExpr)，即 X.Sel
		sel, ok := c.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		// 检查 X 部分是不是 errors
		if pkgIdent, ok := sel.X.(*ast.Ident); !ok || pkgIdent.Name != "errors" {
			return false
		}
		// 检查 Sel 部分是不是 Is 或 As
		if sel.Sel.Name != "Is" && sel.Sel.Name != "As" {
			return false
		}
		// 检查第一个参数是不是我们的 err 变量
		if len(c.Args) > 0 && isIdent(pass, c.Args[0], errIdent) {
			return true
		}
	}
	return false
}

// isIdentifierReassigned 检查 err 变量在被处理前是否被重新赋值
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
		// 检查左侧的变量
		for _, lhs := range assign.Lhs {
			ident, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			// 如果左侧变量的 类型对象 和我们的目标对象是同一个，说明被重新赋值了
			if pass.TypesInfo.ObjectOf(ident) == targetObj {
				reassigned = true
				return false
			}
		}
		return true
	})
	return reassigned
}

// isIdent 确保比较的是同一个变量声明
func isIdent(pass *analysis.Pass, expr ast.Expr, targetIdent *ast.Ident) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && pass.TypesInfo.ObjectOf(ident) == pass.TypesInfo.ObjectOf(targetIdent)
}

// isNil 检查一个表达式是否是预定义的 nil
func isNil(pass *analysis.Pass, expr ast.Expr) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && pass.TypesInfo.ObjectOf(ident) == types.Universe.Lookup("nil")
}

// findFile 根据一个节点的位置找到它所属的 *ast.File
func findFile(pass *analysis.Pass, node ast.Node) *ast.File {
	for _, file := range pass.Files {
		if file.Pos() <= node.Pos() && node.End() <= file.End() {
			return file
		}
	}
	return nil
}
