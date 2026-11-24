package stylecheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	maxNesting    = 3
	maxComplexity = 10
	maxParams     = 4
	warnFuncLines = 48
)

type visitor struct {
	fset     *token.FileSet
	comments map[*ast.FuncDecl]*ast.CommentGroup
	todoRe   *regexp.Regexp
	t        *testing.T
}

func TestBitsGoStyle(t *testing.T) {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	v := &visitor{
		fset:     fset,
		comments: map[*ast.FuncDecl]*ast.CommentGroup{},
		todoRe:   regexp.MustCompile(`TODO\([a-zA-Z0-9_.-]+\): \S.+`),
		t:        t,
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".bench") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		v.checkFile(path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (v *visitor) checkFile(path string) {
	file, err := parser.ParseFile(v.fset, path, nil, parser.ParseComments)
	if err != nil {
		v.t.Errorf("parse %s: %v", path, err)
		return
	}
	v.checkComments(file)
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			v.checkGlobals(d)
		case *ast.FuncDecl:
			v.comments[d] = d.Doc
			v.checkSignature(d.Type, d.Pos())
			if d.Body != nil {
				v.checkBody(d.Body, d.Pos())
			}
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.StructType:
			v.checkStruct(node)
		case *ast.FuncLit:
			v.checkSignature(node.Type, node.Pos())
		}
		return true
	})
}

func (v *visitor) allowed(pos token.Pos, rule string) bool {
	line := v.fset.Position(pos).Line
	for fn, doc := range v.comments {
		if doc == nil || v.fset.Position(fn.Pos()).Line > line || v.fset.Position(fn.End()).Line < line {
			continue
		}
		for _, c := range doc.List {
			if strings.Contains(c.Text, "bits:allow("+rule+"):") {
				return true
			}
		}
	}
	return false
}

func (v *visitor) checkComments(file *ast.File) {
	for _, group := range file.Comments {
		for _, c := range group.List {
			if strings.Contains(c.Text, "TODO") && !v.todoRe.MatchString(c.Text) {
				v.t.Errorf("%s: BCS015 malformed TODO: %s", v.fset.Position(c.Pos()), strings.TrimSpace(c.Text))
			}
		}
	}
}

func (v *visitor) checkGlobals(d *ast.GenDecl) {
	if d.Tok != token.VAR {
		return
	}
	for _, spec := range d.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		if isAllowedGlobal(vs) {
			continue
		}
		v.t.Errorf("%s: BCS011/GO008 mutable package-level var %s", v.fset.Position(vs.Pos()), vs.Names[0].Name)
	}
}

func isAllowedGlobal(vs *ast.ValueSpec) bool {
	if len(vs.Names) == 1 && vs.Names[0].Name == "_" {
		return true
	}
	if len(vs.Values) != 1 {
		return false
	}
	call, ok := vs.Values[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	isErrorsNew := pkg.Name == "errors" && sel.Sel.Name == "New"
	isFmtErrorf := pkg.Name == "fmt" && sel.Sel.Name == "Errorf"
	return isErrorsNew || isFmtErrorf
}

func (v *visitor) checkStruct(s *ast.StructType) {
	for _, field := range s.Fields.List {
		if isContextType(field.Type) {
			v.t.Errorf("%s: GO003 context.Context stored in struct", v.fset.Position(field.Pos()))
		}
	}
}

func isContextType(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Context" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "context"
}

func (v *visitor) checkSignature(ft *ast.FuncType, pos token.Pos) {
	if ft.Params == nil {
		return
	}
	count := 0
	for i, field := range ft.Params.List {
		count += maxInt(len(field.Names), 1)
		v.checkParamKind(field, pos, i)
	}
	if count > maxParams && !v.allowed(pos, "BCS004") {
		v.t.Errorf("%s: BCS004 too many parameters (%d > %d)", v.fset.Position(pos), count, maxParams)
	}
}

func (v *visitor) checkParamKind(field *ast.Field, fnPos token.Pos, paramIndex int) {
	if isBoolType(field.Type) && !v.allowed(fnPos, "BCS004") {
		v.t.Errorf("%s: BCS004 naked bool parameter", v.fset.Position(field.Pos()))
	}
	if paramIndex > 0 && isContextType(field.Type) {
		v.t.Errorf("%s: GO003 context.Context must be first parameter", v.fset.Position(field.Pos()))
	}
}

func isBoolType(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == "bool"
}

func (v *visitor) checkBody(body *ast.BlockStmt, pos token.Pos) {
	lines := v.fset.Position(body.End()).Line - v.fset.Position(pos).Line
	if lines > warnFuncLines {
		v.t.Logf("warning: %s: BCS003 function spans %d lines (> %d)", v.fset.Position(pos), lines, warnFuncLines)
	}
	nesting, complexity := walkControl(body, 0, 1)
	if complexity > maxComplexity && !v.allowed(pos, "BCS003") {
		v.t.Errorf("%s: BCS003 cyclomatic complexity %d > %d", v.fset.Position(pos), complexity, maxComplexity)
	}
	if nesting > maxNesting && !v.allowed(pos, "BCS003") {
		v.t.Errorf("%s: BCS003 control nesting %d > %d", v.fset.Position(pos), nesting, maxNesting)
	}
}

type flowMetrics struct {
	maxDepth   int
	complexity int
}

func (m *flowMetrics) merge(d, c int) {
	m.maxDepth = maxInt(m.maxDepth, d)
	m.complexity += c
}

func walkControl(root ast.Node, depth, complexity int) (int, int) {
	m := &flowMetrics{maxDepth: depth, complexity: complexity}
	ast.Inspect(root, func(n ast.Node) bool {
		return m.visit(n, depth)
	})
	return m.maxDepth, m.complexity
}

func (m *flowMetrics) visit(n ast.Node, depth int) bool {
	if n == nil {
		return true
	}
	switch node := n.(type) {
	case *ast.FuncLit:
		return false
	case *ast.IfStmt:
		m.walkIf(node, depth)
	case *ast.ForStmt:
		m.merge(walkControl(node.Body, depth+1, 1))
	case *ast.RangeStmt:
		m.merge(walkControl(node.Body, depth+1, 1))
	case *ast.SwitchStmt:
		m.merge(walkControl(node.Body, depth, 0))
	case *ast.TypeSwitchStmt:
		m.merge(walkControl(node.Body, depth, 0))
	case *ast.SelectStmt:
		m.merge(walkControl(node.Body, depth+1, 0))
	case *ast.CaseClause:
		m.merge(walkList(node.Body, depth+1, 0))
	case *ast.CommClause:
		m.merge(walkList(node.Body, depth+1, 1))
	case *ast.BinaryExpr:
		m.walkBinary(node)
	default:
		return true
	}
	return false
}

func (m *flowMetrics) walkIf(node *ast.IfStmt, depth int) {
	m.complexity++
	m.merge(walkControl(node.Body, depth+1, 0))
	_, c := walkControl(node.Cond, depth, 0)
	m.complexity += c
	if node.Else != nil {
		m.merge(walkControl(node.Else, depth+1, 0))
	}
}

func (m *flowMetrics) walkBinary(node *ast.BinaryExpr) {
	if node.Op == token.LAND || node.Op == token.LOR {
		m.complexity++
	}
}

func walkList(stmts []ast.Stmt, depth, complexity int) (int, int) {
	m := &flowMetrics{maxDepth: depth, complexity: complexity}
	for _, s := range stmts {
		m.merge(walkControl(s, depth, 0))
	}
	return m.maxDepth, m.complexity
}

func maxInt(a, b int) int {
	return int(max(a, b))
}
