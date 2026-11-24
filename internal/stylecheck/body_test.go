package stylecheck

import (
	"go/ast"
	"go/token"
	"strings"
)

type bodyChecker struct {
	v            *visitor
	params       map[string]bool
	scopes       []map[string]bool
	allowErrInit bool
}

func (v *visitor) checkFuncBody(fnPos token.Pos, recv *ast.FieldList, ft *ast.FuncType, body *ast.BlockStmt) {
	b := &bodyChecker{
		v:      v,
		params: collectNames(recv, ft.Params),
		scopes: []map[string]bool{},
	}
	b.walkBlock(body)
	v.checkBodyMetrics(body, fnPos)
}

func collectNames(lists ...*ast.FieldList) map[string]bool {
	names := map[string]bool{}
	for _, list := range lists {
		if list == nil {
			continue
		}
		for _, field := range list.List {
			for _, name := range field.Names {
				names[name.Name] = true
			}
		}
	}
	return names
}

func (b *bodyChecker) walkBlock(block *ast.BlockStmt) {
	if block == nil {
		return
	}
	b.scopes = append(b.scopes, map[string]bool{})
	for _, stmt := range block.List {
		b.walkStmt(stmt)
	}
	b.scopes = b.scopes[:len(b.scopes)-1]
}

func (b *bodyChecker) walkStmt(stmt ast.Stmt) {
	switch s := stmt.(type) {
	case *ast.AssignStmt:
		b.checkAssign(s)
	case *ast.IncDecStmt:
		b.checkIdentWrite(s.X, "reassigns parameter")
	case *ast.DeclStmt:
		b.checkDecl(s)
	case *ast.IfStmt:
		b.walkIf(s)
	case *ast.ForStmt:
		b.walkFor(s)
	case *ast.RangeStmt:
		b.walkRange(s)
	case *ast.BlockStmt:
		b.walkBlock(s)
	case *ast.ExprStmt:
		b.checkExpr(s.X)
	case *ast.ReturnStmt:
		b.walkExprs(s.Results)
	case *ast.SwitchStmt:
		b.walkSwitch(s)
	case *ast.SelectStmt:
		b.walkSelect(s)
	}
}

func (b *bodyChecker) checkAssign(s *ast.AssignStmt) {
	b.walkExprs(s.Rhs)
	for _, lhs := range s.Lhs {
		id, ok := lhs.(*ast.Ident)
		if !ok || id.Name == "_" {
			continue
		}
		if s.Tok == token.DEFINE {
			b.declare(id)
			continue
		}
		b.checkIdentWrite(id, "reassigns parameter")
	}
}

func (b *bodyChecker) checkDecl(s *ast.DeclStmt) {
	gen, isGen := s.Decl.(*ast.GenDecl)
	if !isGen {
		return
	}
	for _, spec := range gen.Specs {
		vs, isValue := spec.(*ast.ValueSpec)
		if !isValue {
			continue
		}
		b.walkExprs(vs.Values)
		for _, name := range vs.Names {
			b.declare(name)
		}
	}
}

func (b *bodyChecker) declare(id *ast.Ident) {
	if id.Name == "_" {
		return
	}
	current := b.scopes[len(b.scopes)-1]
	if current[id.Name] {
		return
	}
	if b.params[id.Name] {
		b.v.errorf(id.Pos(), "BCS011", "shadows parameter %s", id.Name)
	} else if b.inOuterScope(id.Name) && !(id.Name == "err" && b.allowErrInit) {
		b.v.errorf(id.Pos(), "BCS011", "shadows local variable %s", id.Name)
	}
	current[id.Name] = true
}

func (b *bodyChecker) inOuterScope(name string) bool {
	for i := 0; i < len(b.scopes)-1; i++ {
		if b.scopes[i][name] {
			return true
		}
	}
	return false
}

func (b *bodyChecker) checkIdentWrite(expr ast.Expr, msg string) {
	id, ok := expr.(*ast.Ident)
	if ok && b.params[id.Name] {
		b.v.errorf(id.Pos(), "BCS011", "%s %s", msg, id.Name)
	}
}

func (b *bodyChecker) walkIf(s *ast.IfStmt) {
	if len(s.Body.List) == 0 && !b.v.hasCommentInside(s.Body) && !b.v.allowed(s.Pos(), "GO001") {
		b.v.errorf(s.Pos(), "GO001", "empty if body")
	}
	if s.Else != nil && blockTerminates(s.Body) && !b.v.allowed(s.Else.Pos(), "BCS005") {
		b.v.errorf(s.Else.Pos(), "BCS005", "redundant else after terminating branch")
	}
	b.walkInit(s.Init)
	b.checkExpr(s.Cond)
	b.walkBlock(s.Body)
	if s.Else != nil {
		b.walkStmt(s.Else)
	}
}

func blockTerminates(block *ast.BlockStmt) bool {
	if len(block.List) == 0 {
		return false
	}
	switch last := block.List[len(block.List)-1].(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.BranchStmt:
		return last.Tok == token.BREAK || last.Tok == token.CONTINUE
	case *ast.IfStmt:
		elseBlock, ok := last.Else.(*ast.BlockStmt)
		return ok && blockTerminates(last.Body) && blockTerminates(elseBlock)
	default:
		return false
	}
}

func (b *bodyChecker) walkInit(stmt ast.Stmt) {
	if stmt == nil {
		return
	}
	b.scopes = append(b.scopes, map[string]bool{})
	prev := b.allowErrInit
	b.allowErrInit = true
	b.walkStmt(stmt)
	b.allowErrInit = prev
	b.scopes = b.scopes[:len(b.scopes)-1]
}

func (b *bodyChecker) walkFor(s *ast.ForStmt) {
	b.walkInit(s.Init)
	b.checkExpr(s.Cond)
	if s.Post != nil {
		b.walkStmt(s.Post)
	}
	b.walkBlock(s.Body)
}

func (b *bodyChecker) walkRange(s *ast.RangeStmt) {
	b.checkExpr(s.X)
	b.walkBlock(s.Body)
}

func (b *bodyChecker) walkSwitch(s *ast.SwitchStmt) {
	b.walkInit(s.Init)
	b.checkExpr(s.Tag)
	b.walkBlock(s.Body)
}

func (b *bodyChecker) walkSelect(s *ast.SelectStmt) {
	b.walkBlock(s.Body)
}

func (b *bodyChecker) walkExprs(exprs []ast.Expr) {
	for _, expr := range exprs {
		b.checkExpr(expr)
	}
}

func (b *bodyChecker) checkExpr(expr ast.Expr) {
	if expr == nil {
		return
	}
	ast.Inspect(expr, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.CallExpr:
			b.checkCall(e)
		case *ast.FuncLit:
			b.checkFuncLit(e)
			return false
		}
		return true
	})
}

func (b *bodyChecker) checkFuncLit(lit *ast.FuncLit) {
	sub := &bodyChecker{
		v:      b.v,
		params: collectNames(lit.Type.Params),
		scopes: []map[string]bool{},
	}
	sub.walkBlock(lit.Body)
}

func (b *bodyChecker) checkCall(call *ast.CallExpr) {
	if id, isIdent := call.Fun.(*ast.Ident); isIdent && id.Name == "panic" && !b.v.allowed(call.Pos(), "GO001") {
		b.v.errorf(call.Pos(), "GO001", "panic is forbidden outside tests")
		return
	}
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel || b.v.allowExits || b.v.allowed(call.Pos(), "GO001") {
		return
	}
	pkg, isPkg := sel.X.(*ast.Ident)
	if !isPkg {
		return
	}
	name := pkg.Name + "." + sel.Sel.Name
	if name == "os.Exit" || strings.HasPrefix(name, "log.Fatal") {
		b.v.errorf(call.Pos(), "GO001", "%s is forbidden outside cmd/dl and tests", name)
	}
}
