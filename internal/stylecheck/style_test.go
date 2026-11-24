package stylecheck

import (
	"fmt"
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

const (
	severityViolation = "violation"
	severityWarning   = "warning"
)

type diagnostic struct {
	pos      token.Position
	rule     string
	message  string
	severity string
}

func TestBitsGoStyle(t *testing.T) {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && skipDir(d.Name()) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		for _, diag := range analyzeFile(fset, path, nil) {
			reportDiagnostic(t, diag)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func skipDir(name string) bool {
	switch name {
	case ".git", ".bench", ".ryugi", "testdata":
		return true
	default:
		return false
	}
}

func reportDiagnostic(t *testing.T, d diagnostic) {
	t.Helper()
	if d.severity == severityViolation {
		t.Errorf("%s: %s %s", d.pos, d.rule, d.message)
		return
	}
	t.Logf("warning: %s: %s %s", d.pos, d.rule, d.message)
}

func analyzeFile(fset *token.FileSet, path string, src []byte) []diagnostic {
	slashPath := filepath.ToSlash(path)
	v := &visitor{
		fset:       fset,
		path:       slashPath,
		allowExits: isTestOrMainPath(slashPath),
		comments:   map[*ast.FuncDecl]*ast.CommentGroup{},
		commentLn:  map[int]string{},
		interfaces: map[string]bool{},
		todoRe:     regexp.MustCompile(`TODO\([a-zA-Z0-9_.-]+\): \S.+`),
	}
	v.checkFile(src)
	return v.diags
}

type visitor struct {
	fset        *token.FileSet
	path        string
	allowExits  bool
	comments    map[*ast.FuncDecl]*ast.CommentGroup
	fileComment []*ast.Comment
	commentLn   map[int]string
	interfaces  map[string]bool
	todoRe      *regexp.Regexp
	diags       []diagnostic
}

func (v *visitor) errorf(pos token.Pos, rule, format string, args ...any) {
	v.diags = append(v.diags, diagnostic{
		pos:      v.fset.Position(pos),
		rule:     rule,
		message:  fmt.Sprintf(format, args...),
		severity: severityViolation,
	})
}

func (v *visitor) warnf(pos token.Pos, rule, format string, args ...any) {
	v.diags = append(v.diags, diagnostic{
		pos:      v.fset.Position(pos),
		rule:     rule,
		message:  fmt.Sprintf(format, args...),
		severity: severityWarning,
	})
}

func (v *visitor) checkFile(src []byte) {
	var input any
	if src != nil {
		input = src
	}
	file, err := parser.ParseFile(v.fset, v.path, input, parser.ParseComments)
	if err != nil {
		v.errorf(token.NoPos, "PARSE", "parse %s: %v", v.path, err)
		return
	}
	v.indexComments(file)
	v.checkHeader(file)
	v.indexInterfaces(file)
	v.checkComments(file)
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			v.checkGlobals(d)
		case *ast.FuncDecl:
			v.checkFuncDecl(d)
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		if s, ok := n.(*ast.StructType); ok {
			v.checkStruct(s)
		}
		return true
	})
}

func isTestOrMainPath(path string) bool {
	return strings.HasSuffix(path, "_test.go") || strings.Contains(path, "/cmd/dl/")
}

func (v *visitor) indexComments(file *ast.File) {
	for _, group := range file.Comments {
		for _, c := range group.List {
			v.fileComment = append(v.fileComment, c)
			v.commentLn[v.fset.Position(c.Pos()).Line] = c.Text
		}
	}
}

func (v *visitor) indexInterfaces(file *ast.File) {
	for _, decl := range file.Decls {
		gen, isGen := decl.(*ast.GenDecl)
		if !isGen {
			continue
		}
		for _, spec := range gen.Specs {
			ts, isType := spec.(*ast.TypeSpec)
			if !isType {
				continue
			}
			if _, isIface := ts.Type.(*ast.InterfaceType); isIface {
				v.interfaces[ts.Name.Name] = true
			}
		}
	}
}

func (v *visitor) checkHeader(file *ast.File) {
	switch file.Name.Name {
	case "util", "utils", "common", "base", "helper", "helpers", "misc", "shared":
		v.errorf(file.Name.Pos(), "PKGNAME", "forbidden catch-all package name %q", file.Name.Name)
	}
	for _, spec := range file.Imports {
		if spec.Name == nil {
			continue
		}
		if spec.Name.Name == "." && !v.allowed(spec.Pos(), "IMPORT") {
			v.errorf(spec.Pos(), "IMPORT", "dot import is forbidden")
		}
		if spec.Name.Name == "_" && !v.hasSideEffectReason(spec) {
			v.errorf(spec.Pos(), "IMPORT", "blank import requires a side-effect reason comment")
		}
	}
}

func (v *visitor) hasSideEffectReason(spec *ast.ImportSpec) bool {
	if spec.Doc != nil || spec.Comment != nil || v.allowed(spec.Pos(), "IMPORT") {
		return true
	}
	prev := v.commentLn[v.fset.Position(spec.Pos()).Line-1]
	return strings.TrimSpace(prev) != ""
}

func (v *visitor) allowed(pos token.Pos, rule string) bool {
	line := v.fset.Position(pos).Line
	needle := "bits:allow(" + rule + "):"
	if strings.Contains(v.commentLn[line], needle) || strings.Contains(v.commentLn[line-1], needle) {
		return true
	}
	for fn, doc := range v.comments {
		if doc == nil || v.fset.Position(fn.Pos()).Line > line || v.fset.Position(fn.End()).Line < line {
			continue
		}
		for _, c := range doc.List {
			if strings.Contains(c.Text, needle) {
				return true
			}
		}
	}
	return false
}

func (v *visitor) hasCommentInside(block *ast.BlockStmt) bool {
	for _, c := range v.fileComment {
		if c.Pos() > block.Lbrace && c.End() < block.Rbrace {
			return true
		}
	}
	return false
}

func (v *visitor) checkComments(file *ast.File) {
	for _, group := range file.Comments {
		for _, c := range group.List {
			if strings.Contains(c.Text, "TODO") && !v.todoRe.MatchString(c.Text) {
				v.errorf(c.Pos(), "BCS015", "malformed TODO: %s", strings.TrimSpace(c.Text))
			}
		}
	}
}

func (v *visitor) checkFuncDecl(d *ast.FuncDecl) {
	v.comments[d] = d.Doc
	if d.Name.Name == "init" {
		v.errorf(d.Pos(), "GO008", "init() is forbidden; pass dependencies explicitly")
	}
	v.checkSignature(d.Type, d.Pos())
	if d.Body != nil {
		v.checkFuncBody(d.Pos(), d.Recv, d.Type, d.Body)
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
		v.errorf(vs.Pos(), "BCS011", "mutable package-level var %s", vs.Names[0].Name)
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
	switch pkg.Name + "." + sel.Sel.Name {
	case "errors.New", "fmt.Errorf", "regexp.MustCompile":
		return true
	default:
		return false
	}
}

func (v *visitor) checkStruct(s *ast.StructType) {
	for _, field := range s.Fields.List {
		if isContextType(field.Type) {
			v.errorf(field.Pos(), "GO003", "context.Context stored in struct")
		}
		if len(field.Names) == 0 && !v.isInterfaceEmbedding(field.Type) && !v.allowed(field.Pos(), "GO010") {
			v.errorf(field.Pos(), "GO010", "struct embedding hides behavior; use a named field")
		}
	}
}

func (v *visitor) isInterfaceEmbedding(expr ast.Expr) bool {
	if _, ok := expr.(*ast.InterfaceType); ok {
		return true
	}
	id, ok := expr.(*ast.Ident)
	return ok && v.interfaces[id.Name]
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
		count += max(len(field.Names), 1)
		v.checkParamKind(field, pos, i)
	}
	if count > maxParams && !v.allowed(pos, "BCS004") {
		v.warnf(pos, "BCS004", "parameter count %d > %d", count, maxParams)
	}
}

func (v *visitor) checkParamKind(field *ast.Field, fnPos token.Pos, paramIndex int) {
	if isBoolType(field.Type) && !v.allowed(fnPos, "BCS004") {
		v.errorf(field.Pos(), "BCS004", "naked bool parameter")
	}
	if paramIndex > 0 && isContextType(field.Type) {
		v.errorf(field.Pos(), "GO003", "context.Context must be first parameter")
	}
}

func isBoolType(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == "bool"
}

func (v *visitor) checkBodyMetrics(body *ast.BlockStmt, pos token.Pos) {
	lines := v.fset.Position(body.End()).Line - v.fset.Position(pos).Line
	if lines > warnFuncLines {
		v.warnf(pos, "BCS003", "function spans %d lines (> %d)", lines, warnFuncLines)
	}
	nesting, complexity := walkControl(body, 0, 1)
	if complexity > maxComplexity && !v.allowed(pos, "BCS003") {
		v.errorf(pos, "BCS003", "cyclomatic complexity %d > %d", complexity, maxComplexity)
	}
	if nesting > maxNesting && !v.allowed(pos, "BCS003") {
		v.errorf(pos, "BCS003", "control nesting %d > %d", nesting, maxNesting)
	}
}

type flowMetrics struct {
	maxDepth   int
	complexity int
}

func (m *flowMetrics) merge(d, c int) {
	m.maxDepth = max(m.maxDepth, d)
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
