// Package analyzer finds mistakes in ant code that compile but fail at run time.
// A probe is a value from ant.Fields, and everything here follows from that:
// the compiler checks a field's value type, but only a look at the source can tell
// that a pointer came from a probe, or from the right model.
package analyzer

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"slices"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// antPath is the import path of ant.
const antPath = "github.com/skudra-go/ant"

// Analyzer reports field pointers that are not probes, probes of the wrong model,
// and Find keys of the wrong type. A comment //antvet:ignore at the end of a
// report's line, or on a line of its own above it, silences it, for code that is
// wrong on purpose.
var Analyzer = &analysis.Analyzer{
	Name:      "antvet",
	Doc:       "check ant queries for what the compiler cannot see",
	Requires:  []*analysis.Analyzer{inspect.Analyzer},
	Run:       run,
	FactTypes: []analysis.Fact{new(probeFact), new(keyFact)},
}

// keyFact records the primary key a model's Configure method sets, which
// replaces the convention of a field named ID. Field is the path to it, such as
// Identity.Code, and empty when Find cannot take the key, such as a composite
// one, or when the analyzer cannot be sure which key Configure sets.
type keyFact struct {
	Field string
}

// AFact implements analysis.Fact.
func (*keyFact) AFact() {}

func (f *keyFact) String() string { return "key " + f.Field }

// probeFact marks a package variable as a probe, across packages.
type probeFact struct {
	Model string
}

// AFact implements analysis.Fact.
func (*probeFact) AFact() {}

func (f *probeFact) String() string { return "probe of " + f.Model }

// fieldTaking lists the functions whose first argument is a field pointer.
var fieldTaking = map[string]bool{
	"Eq":        true,
	"Ne":        true,
	"Gt":        true,
	"Gte":       true,
	"Lt":        true,
	"Lte":       true,
	"In":        true,
	"NotIn":     true,
	"Between":   true,
	"Like":      true,
	"ILike":     true,
	"IsNull":    true,
	"IsNotNull": true,
	"Past":      true,
	"Future":    true,
	"Today":     true,
	"InSub":     true,
	"NotInSub":  true,
	"Set":       true,
	"SetExpr":   true,
	"Inc":       true,
	"Dec":       true,
	"Col":       true,
	"F":         true,
}

func run(pass *analysis.Pass) (any, error) {
	in := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	locals := findProbes(pass, in)
	findKeys(pass)
	pass = quieted(pass)

	c := &checker{pass, locals}

	in.WithStack(
		[]ast.Node{(*ast.CallExpr)(nil)},
		func(n ast.Node, push bool, stack []ast.Node) bool {
			if !push {
				return true
			}

			call := n.(*ast.CallExpr)
			c.checkCondition(call)
			c.checkWhere(call, stack)
			checkFind(pass, call)

			return true
		},
	)

	return nil, nil
}

// checker holds what the checks share.
type checker struct {
	pass   *analysis.Pass
	locals map[types.Object]string // probes in local variables
}

// findProbes returns the variables that hold probes, and tells other packages of
// those at package level through a fact. A variable holds a probe if all that is
// ever assigned to it is one: from ant.Fields, a probe of another package, or a
// variable that holds one, wherever in the package that comes. A declaration
// with no value does not count, so a package variable may be set in init. A
// variable that is ever given anything else may hold that at any use, as the
// analyzer does not follow the order code runs in, so it is an ordinary one.
func findProbes(pass *analysis.Pass, in *inspector.Inspector) map[types.Object]string {
	assigned := map[types.Object][]ast.Expr{}

	// value is nil where one call gives several names theirs.
	add := func(id *ast.Ident, values []ast.Expr, names, i int) {
		object, ok := pass.TypesInfo.ObjectOf(id).(*types.Var)
		if !ok {
			return
		}

		var value ast.Expr

		if len(values) == names {
			value = values[i]
		}

		assigned[object] = append(assigned[object], value)
	}

	in.Preorder([]ast.Node{(*ast.AssignStmt)(nil), (*ast.ValueSpec)(nil)}, func(n ast.Node) {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if n.Tok != token.ASSIGN && n.Tok != token.DEFINE {
				return
			}

			for i, left := range n.Lhs {
				if id, ok := ast.Unparen(left).(*ast.Ident); ok {
					add(id, n.Rhs, len(n.Lhs), i)
				}
			}
		case *ast.ValueSpec:
			if len(n.Values) == 0 {
				return
			}

			for i, name := range n.Names {
				add(name, n.Values, len(n.Names), i)
			}
		}
	})

	// A copy may come before what it copies is assigned, so this goes round
	// until no more variables turn out to hold probes.
	locals := map[types.Object]string{}

	for found := true; found; {
		found = false

		for object, values := range assigned {
			if _, done := locals[object]; done {
				continue
			}

			if model := heldModel(pass, locals, values); model != "" {
				locals[object] = model
				found = true
			}
		}
	}

	for object, model := range locals {
		if object.Parent() == pass.Pkg.Scope() {
			pass.ExportObjectFact(object, &probeFact{model})
		}
	}

	return locals
}

// heldModel returns the model of the probe that each of values is, or "" if one
// is not, or is not known to be yet.
func heldModel(pass *analysis.Pass, locals map[types.Object]string, values []ast.Expr) string {
	model := ""

	for _, value := range values {
		if value == nil {
			return ""
		}

		model = probeSource(pass, value)
		if object := objectOf(pass, ast.Unparen(value)); model == "" && object != nil {
			model = locals[object]
		}

		if model == "" {
			return ""
		}
	}

	return model
}

// quieted returns pass with its reports dropped on lines that //antvet:ignore
// covers: its own, and the one below it if the comment has a line to itself. One
// at the end of a line of code covers only that line.
func quieted(pass *analysis.Pass) *analysis.Pass {
	ignored := map[string]bool{}

	for _, file := range pass.Files {
		for _, group := range file.Comments {
			for _, comment := range group.List {
				if !strings.HasPrefix(comment.Text, "//antvet:ignore") {
					continue
				}

				at := pass.Fset.Position(comment.Pos())
				ignored[fmt.Sprintf("%s:%d", at.Filename, at.Line)] = true

				if alone(pass, comment.Pos()) {
					ignored[fmt.Sprintf("%s:%d", at.Filename, at.Line+1)] = true
				}
			}
		}
	}

	out := *pass
	report := pass.Report

	out.Report = func(d analysis.Diagnostic) {
		at := pass.Fset.Position(d.Pos)
		if !ignored[fmt.Sprintf("%s:%d", at.Filename, at.Line)] {
			report(d)
		}
	}

	return &out
}

// alone reports whether only spaces come before the comment at pos on its line,
// as the file has it, whatever //line directives say. If the file cannot be
// read, the comment is taken to be alone, as it covers more then.
func alone(pass *analysis.Pass, pos token.Pos) bool {
	at := pass.Fset.PositionFor(pos, false)
	start := at.Offset - at.Column + 1

	if pass.ReadFile == nil {
		return true
	}

	content, err := pass.ReadFile(at.Filename)
	if err != nil || start < 0 || at.Offset > len(content) {
		return true
	}

	return strings.TrimSpace(string(content[start:at.Offset])) == ""
}

// findKeys records the key that each model's Configure method sets.
func findKeys(pass *analysis.Pass) {
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "Configure" || fn.Recv == nil || fn.Body == nil {
				continue
			}

			model := receiverType(pass, fn)
			if model == nil {
				continue
			}

			if field, ok := configuredKey(pass, fn); ok {
				pass.ExportObjectFact(model.Obj(), &keyFact{Field: field})
			}
		}
	}
}

// configuredKey reports whether a Configure method sets the key, and the path to
// it. Only one call, made on every run, says for sure: a key set in a branch, in
// a closure, after a return that may skip it, set twice, or perhaps set by
// whatever the builder is handed to, could be either, so the key is unknown then,
// and checkFind leaves it alone.
func configuredKey(pass *analysis.Pass, fn *ast.FuncDecl) (string, bool) {
	var builder types.Object

	if params := fn.Type.Params.List; len(params) > 0 && len(params[0].Names) > 0 {
		builder = pass.TypesInfo.Defs[params[0].Names[0]]
	}

	// A call runs every time if it heads a statement of the body, or sits in
	// such a statement's chain, as b.Key(&C.Code).NotGenerated() does.
	direct := map[ast.Node]bool{}

	for _, stmt := range fn.Body.List {
		expr, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}

		for e := ast.Unparen(expr.X); ; {
			call, ok := e.(*ast.CallExpr)
			if !ok {
				break
			}

			direct[call] = true

			selector, ok := ast.Unparen(indexed(call.Fun)).(*ast.SelectorExpr)
			if !ok {
				break
			}

			e = ast.Unparen(selector.X)
		}
	}

	// The builder may be used as a receiver, or handed to ant. Anything else,
	// such as alias := b or a helper of one's own, could set the key unseen.
	allowed := map[*ast.Ident]bool{}

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			// A method called on the builder there and then; a method value, as
			// setKey := b.Key, could be called anywhere.
			if selector, ok := ast.Unparen(indexed(n.Fun)).(*ast.SelectorExpr); ok {
				if id, ok := ast.Unparen(selector.X).(*ast.Ident); ok {
					allowed[id] = true
				}
			}

			if isAntFunc(pass, n) {
				for _, a := range n.Args {
					if id, ok := ast.Unparen(a).(*ast.Ident); ok {
						allowed[id] = true
					}
				}
			}
		}

		return true
	})

	var (
		paths   []string
		sure    = true
		escaped bool
		returns bool
	)

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if field, ok := setsKey(pass, n); ok {
				paths = append(paths, field)
				sure = sure && direct[n]
			}
		case *ast.Ident:
			if builder != nil && pass.TypesInfo.Uses[n] == builder && !allowed[n] {
				escaped = true
			}
		}

		return true
	})

	// A return in the body, not in a closure, may come before the key is set.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			returns = true
		}

		return true
	})

	switch {
	case len(paths) == 0 && !escaped:
		return "", false
	case len(paths) == 1 && sure && !escaped && !returns:
		return paths[0], true
	}

	return "", true
}

// isAntFunc reports whether call is to a function or method of ant.
func isAntFunc(pass *analysis.Pass, call *ast.CallExpr) bool {
	var id *ast.Ident

	switch fun := ast.Unparen(indexed(call.Fun)).(type) {
	case *ast.Ident:
		id = fun
	case *ast.SelectorExpr:
		id = fun.Sel
	default:
		return false
	}

	object, ok := pass.TypesInfo.Uses[id].(*types.Func)

	return ok && object.Pkg() != nil && object.Pkg().Path() == antPath
}

// receiverType returns the named type a method is declared on.
func receiverType(pass *analysis.Pass, fn *ast.FuncDecl) *types.Named {
	object, ok := pass.TypesInfo.Defs[fn.Name].(*types.Func)
	if !ok {
		return nil
	}

	recv := object.Type().(*types.Signature).Recv()
	if recv == nil {
		return nil
	}

	t := types.Unalias(recv.Type())
	if pointer, ok := t.(*types.Pointer); ok {
		t = types.Unalias(pointer.Elem())
	}

	named, _ := t.(*types.Named)

	return named
}

// setsKey reports whether call sets a primary key, and of which field, if Find
// can take it.
func setsKey(pass *analysis.Pass, call *ast.CallExpr) (string, bool) {
	var id *ast.Ident

	switch fun := ast.Unparen(indexed(call.Fun)).(type) {
	case *ast.SelectorExpr:
		id = fun.Sel
	case *ast.Ident: // a dot import
		id = fun
	default:
		return "", false
	}

	object, ok := pass.TypesInfo.Uses[id].(*types.Func)
	if !ok || object.Pkg() == nil || object.Pkg().Path() != antPath {
		return "", false
	}

	switch object.Name() {
	case "Key":
		if len(call.Args) == 1 {
			return fieldPath(pass, call.Args[0]), true
		}
	case "GenerateKey":
		if len(call.Args) == 3 {
			return fieldPath(pass, call.Args[1]), true
		}
	case "CompositeKey", "NoKey":
		return "", true
	}

	return "", false
}

// indexed strips the type arguments from a generic function or method value.
func indexed(fun ast.Expr) ast.Expr {
	switch index := ast.Unparen(fun).(type) {
	case *ast.IndexExpr:
		return index.X
	case *ast.IndexListExpr:
		return index.X
	}

	return fun
}

// fieldPath returns the path below the probe of &X.A.B, as A.B, so that a field
// of an embedded struct is not mistaken for a top-level field of the same name.
func fieldPath(pass *analysis.Pass, arg ast.Expr) string {
	unary, ok := ast.Unparen(arg).(*ast.UnaryExpr)
	if !ok {
		return ""
	}

	var names []string

	e := ast.Unparen(unary.X)

	for {
		selector, ok := e.(*ast.SelectorExpr)
		if !ok {
			break
		}

		names = append([]string{selector.Sel.Name}, names...)
		e = ast.Unparen(selector.X)
	}

	// A probe from another package, as in &models.Co.Code, has the package
	// and the probe in front.
	if id, ok := e.(*ast.Ident); ok && len(names) > 0 {
		if _, isPackage := pass.TypesInfo.Uses[id].(*types.PkgName); isPackage {
			names = names[1:]
		}
	}

	return strings.Join(names, ".")
}

// probeSource returns the model of an expression that is a probe, or "".
func probeSource(pass *analysis.Pass, e ast.Expr) string {
	switch e := ast.Unparen(e).(type) {
	case *ast.CallExpr:
		if isAntCall(pass, e, "Fields") {
			if model := modelOf(pass.TypesInfo.TypeOf(e)); model != "" {
				return model
			}
		}
	case *ast.Ident, *ast.SelectorExpr:
		if object := objectOf(pass, e); object != nil {
			fact := new(probeFact)
			if pass.ImportObjectFact(object, fact) {
				return fact.Model
			}
		}
	}

	return ""
}

// modelOf names the model a probe points to. An alias, such as
// type User = fixture.User, names the same model as what it stands for.
func modelOf(t types.Type) string {
	pointer, ok := types.Unalias(t).(*types.Pointer)
	if !ok {
		return ""
	}

	return types.TypeString(unalias(pointer.Elem()), nil)
}

// unalias resolves aliases all the way into a type, its type arguments
// included, so that Row[Key] with type Key = int64 names the same model as
// Row[int64].
func unalias(t types.Type) types.Type {
	switch t := types.Unalias(t).(type) {
	case *types.Pointer:
		return types.NewPointer(unalias(t.Elem()))
	case *types.Slice:
		return types.NewSlice(unalias(t.Elem()))
	case *types.Map:
		return types.NewMap(unalias(t.Key()), unalias(t.Elem()))
	case *types.Named:
		args := t.TypeArgs()
		if args.Len() == 0 {
			return t
		}

		list := make([]types.Type, args.Len())

		for i := range args.Len() {
			list[i] = unalias(args.At(i))
		}

		if out, err := types.Instantiate(nil, t.Origin(), list, false); err == nil {
			return out
		}

		return t
	default:
		return t
	}
}

func objectOf(pass *analysis.Pass, e ast.Expr) types.Object {
	switch e := e.(type) {
	case *ast.Ident:
		return pass.TypesInfo.Uses[e]
	case *ast.SelectorExpr:
		return pass.TypesInfo.Uses[e.Sel]
	}

	return nil
}

func isAntCall(pass *analysis.Pass, call *ast.CallExpr, name string) bool {
	return calleeName(pass, call) == name
}

// calleeName returns the name of an ant package function being called, or "".
func calleeName(pass *analysis.Pass, call *ast.CallExpr) string {
	fun := ast.Unparen(call.Fun)
	switch index := fun.(type) {
	case *ast.IndexExpr:
		fun = index.X
	case *ast.IndexListExpr:
		fun = index.X
	}

	var id *ast.Ident

	switch fun := ast.Unparen(fun).(type) {
	case *ast.SelectorExpr:
		id = fun.Sel
	case *ast.Ident: // a dot import
		id = fun
	default:
		return ""
	}

	object, ok := pass.TypesInfo.Uses[id].(*types.Func)
	if !ok || object.Pkg() == nil || object.Pkg().Path() != antPath {
		return ""
	}

	if object.Type().(*types.Signature).Recv() != nil {
		return ""
	}

	return object.Name()
}

// probeOf follows a pointer like &U.Age to its root, and returns the root's
// variable and whether that is a probe.
func (c *checker) probeOf(arg ast.Expr) (root ast.Expr, model string, isField bool) {
	pass := c.pass

	unary, ok := ast.Unparen(arg).(*ast.UnaryExpr)
	if !ok || unary.Op.String() != "&" {
		return nil, "", false
	}

	e := ast.Unparen(unary.X)

	for {
		selector, ok := e.(*ast.SelectorExpr)
		if !ok {
			break
		}

		object, isPackage := pass.TypesInfo.Uses[identOf(selector.X)].(*types.PkgName)
		if isPackage && object != nil {
			break
		}

		e = ast.Unparen(selector.X)
	}

	object := objectOf(pass, e)
	if object == nil {
		return e, "", true
	}

	if model := c.locals[object]; model != "" {
		return e, model, true
	}

	fact := new(probeFact)
	if pass.ImportObjectFact(object, fact) {
		return e, fact.Model, true
	}

	return e, "", true
}

func identOf(e ast.Expr) *ast.Ident {
	id, _ := e.(*ast.Ident)

	return id
}

// checkCondition reports a field pointer that is not from a probe.
func (c *checker) checkCondition(call *ast.CallExpr) {
	pass := c.pass

	name := calleeName(pass, call)
	if !fieldTaking[name] || len(call.Args) == 0 {
		return
	}
	// A pointer that is not written &x.y here came from elsewhere, and may be fine.
	root, model, isField := c.probeOf(call.Args[0])
	if !isField && root == nil {
		if unary, ok := ast.Unparen(call.Args[0]).(*ast.UnaryExpr); ok && unary.Op.String() == "&" {
			pass.Reportf(
				call.Args[0].Pos(),
				"ant.%s needs a field of a probe from ant.Fields",
				name,
			)
		}

		return
	}

	if model == "" {
		pass.Reportf(
			call.Args[0].Pos(),
			"ant.%s needs a field of a probe from ant.Fields, not of an ordinary variable",
			name,
		)
	}
}

// queryModel returns T of a *Query[T] or *Table[T] receiver, or "".
func queryModel(t types.Type) string {
	if model := queryArg(t); model != nil {
		return types.TypeString(model, nil)
	}

	return ""
}

// queryArg returns T of a *Query[T] or *Table[T], unaliased, or nil, as it is
// when T holds a type parameter, which only an instantiation fills in.
func queryArg(t types.Type) types.Type {
	if t == nil {
		return nil
	}

	t = types.Unalias(t)
	if pointer, ok := t.(*types.Pointer); ok {
		t = types.Unalias(pointer.Elem())
	}

	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != antPath {
		return nil
	}

	if name := named.Obj().
		Name(); (name != "Query" && name != "Table") ||
		named.TypeArgs().Len() != 1 {
		return nil
	}

	if model := unalias(named.TypeArgs().At(0)); !generic(model) {
		return model
	}

	return nil
}

// generic reports whether t holds a type parameter.
func generic(t types.Type) bool {
	switch t := types.Unalias(t).(type) {
	case *types.TypeParam:
		return true
	case *types.Named:
		for arg := range t.TypeArgs().Types() {
			if generic(arg) {
				return true
			}
		}
	case interface{ Elem() types.Type }:
		return generic(t.Elem())
	}

	return false
}

// singleModel lists conditions that name one column of the query's own model.
// Those that compare two columns can refer to an outer query.
var singleModel = map[string]bool{
	"Eq":        true,
	"Ne":        true,
	"Gt":        true,
	"Gte":       true,
	"Lt":        true,
	"Lte":       true,
	"In":        true,
	"NotIn":     true,
	"Between":   true,
	"Like":      true,
	"ILike":     true,
	"IsNull":    true,
	"IsNotNull": true,
	"Past":      true,
	"Future":    true,
	"Today":     true,
	"InSub":     true,
	"NotInSub":  true,
}

// checkWhere reports a condition on a model other than the query's. A subquery's
// conditions may name its outer query's models, so a query is judged only where
// it is plainly not one: when its chain ends in a call that runs it, such as All,
// or when it sits inside an outer query, whose models it may also name.
func (c *checker) checkWhere(call *ast.CallExpr, stack []ast.Node) {
	pass := c.pass

	selector, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Where" {
		return
	}

	model := queryModel(pass.TypesInfo.TypeOf(selector.X))
	if model == "" || joinsLater(stack) {
		return
	}

	allowed := []string{model}

	if !c.runs(stack) {
		outer, ok := c.outerModels(stack)
		if !ok {
			return
		}

		allowed = append(allowed, outer...)
	}

	for _, condition := range c.conditions(call.Args) {
		if !singleModel[calleeName(pass, condition)] || len(condition.Args) == 0 {
			continue
		}

		if _, probe, _ := c.probeOf(condition.Args[0]); probe != "" &&
			!slices.Contains(allowed, probe) {
			pass.Reportf(
				condition.Pos(),
				"condition on %s in a query of %s",
				short(probe),
				short(model),
			)
		}
	}
}

// conditions returns the calls among args, with those that And, Or, and Not
// combine in place of them, since those are on the same query.
func (c *checker) conditions(args []ast.Expr) []*ast.CallExpr {
	var out []*ast.CallExpr

	for _, arg := range args {
		call, ok := ast.Unparen(arg).(*ast.CallExpr)
		if !ok {
			continue
		}

		switch calleeName(c.pass, call) {
		case "And", "Or", "Not":
			out = append(out, c.conditions(call.Args)...)
		default:
			out = append(out, call)
		}
	}

	return out
}

// runs reports whether the chain that the innermost node of stack starts ends in
// a call that runs the query, which leaves ant's builder types: All, Count,
// Update. GroupBy and Join lead to other builders, and the chain goes on.
func (c *checker) runs(stack []ast.Node) bool {
	node := stack[len(stack)-1]

	for i := len(stack) - 2; i >= 1; i-- {
		// Parentheses around a receiver change nothing.
		if paren, ok := stack[i].(*ast.ParenExpr); ok && paren.X == node {
			node = paren

			continue
		}

		selector, ok := stack[i].(*ast.SelectorExpr)
		if !ok || selector.X != node {
			return false
		}

		next, ok := stack[i-1].(*ast.CallExpr)
		if !ok || next.Fun != selector {
			return false
		}

		if !builder(c.pass.TypesInfo.TypeOf(next)) {
			return true
		}

		node = next
		i--
	}

	return false
}

// builder reports whether t is one of ant's types for building a query, such as
// *Query[T] or *Grouped, rather than what running one returns.
func builder(t types.Type) bool {
	if t == nil {
		return false
	}

	t = types.Unalias(t)
	if pointer, ok := t.(*types.Pointer); ok {
		t = types.Unalias(pointer.Elem())
	}

	named, ok := t.(*types.Named)

	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == antPath
}

// outerModels returns the models of the queries that enclose the innermost node
// of stack, and whether they are the whole scope: only queries it sits in the
// arguments of count, as a chain's later calls belong to the same query, and an
// enclosing builder whose model is unknown, such as a grouped query, or a
// function of one's own, makes the scope unknown. So does an outermost query
// that does not run where the test can see it, since it may itself be a
// subquery of something built elsewhere.
func (c *checker) outerModels(stack []ast.Node) ([]string, bool) {
	var (
		out       []string
		outermost = -1
	)

	for i := len(stack) - 2; i >= 0; i-- {
		call, ok := stack[i].(*ast.CallExpr)
		if !ok || !slices.ContainsFunc(call.Args, func(a ast.Expr) bool {
			return ast.Node(a) == stack[i+1]
		}) {
			continue
		}

		if name := calleeName(c.pass, call); isAntFunc(c.pass, call) && name != "" {
			// Has and the Related functions open a scope of the related model,
			// which the analyzer does not follow.
			if strings.HasPrefix(name, "Has") || strings.HasSuffix(name, "Related") {
				return nil, false
			}

			continue // ant.Exists, ant.SubOf, and the like pass a query on
		}

		selector, ok := ast.Unparen(indexed(call.Fun)).(*ast.SelectorExpr)
		if !ok || !isAntFunc(c.pass, call) {
			return nil, false
		}

		model := queryModel(c.pass.TypesInfo.TypeOf(selector.X))
		if model == "" {
			return nil, false
		}

		out = append(out, model)
		outermost = i
	}

	if outermost < 0 || !c.runs(stack[:outermost+1]) {
		return nil, false
	}

	return out, true
}

// joinsLater reports whether the chain goes on to a join, since a condition can
// come before the join that brings its model in. IncludeJoined brings in none
// that conditions can name, as it reads its row under an alias of its own.
func joinsLater(stack []ast.Node) bool {
	for _, n := range stack {
		if selector, ok := n.(*ast.SelectorExpr); ok &&
			strings.Contains(selector.Sel.Name, "Join") &&
			!strings.HasSuffix(selector.Sel.Name, "Joined") {
			return true
		}
	}

	return false
}

func short(name string) string { return name[strings.LastIndex(name, ".")+1:] }

// checkFind reports a Find key of the wrong type, for a model whose key is its ID
// field, or the single field its Configure method makes the key.
func checkFind(pass *analysis.Pass, call *ast.CallExpr) {
	selector, ok := ast.Unparen(indexed(call.Fun)).(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Find" || len(call.Args) != 2 {
		return
	}

	model := queryArg(pass.TypesInfo.TypeOf(selector.X))
	if model == nil {
		return
	}

	name := "ID"

	if named, ok := model.(*types.Named); ok {
		fact := new(keyFact)
		if pass.ImportObjectFact(named.Obj(), fact) {
			name = fact.Field
		}
	}

	if name == "" {
		return
	}

	id := lookupPath(pass, model, name)
	if id == nil {
		return
	}

	key := pass.TypesInfo.TypeOf(call.Args[1])
	if key == nil || compatible(id.Type(), key) {
		return
	}

	pass.Reportf(
		call.Args[1].Pos(),
		"Find key is %s, and %s has a key of type %s",
		types.TypeString(key, nil),
		short(types.TypeString(model, nil)),
		types.TypeString(id.Type(), nil),
	)
}

// lookupPath finds the field at a path such as Identity.Code, or nil.
func lookupPath(pass *analysis.Pass, t types.Type, path string) *types.Var {
	var field *types.Var

	for name := range strings.SplitSeq(path, ".") {
		object, _, _ := types.LookupFieldOrMethod(t, true, pass.Pkg, name)

		v, ok := object.(*types.Var)
		if !ok {
			return nil
		}

		field, t = v, v.Type()
	}

	return field
}

// compatible reports whether Find takes a key of type got for a key field of
// type want, as ant's own check does: the same type, any integer for an integer,
// or a type of the same kind that converts to it.
func compatible(want, got types.Type) bool {
	return types.Identical(want, got) ||
		integer(want) && integer(got) ||
		sameKind(want, got) && types.ConvertibleTo(got, want)
}

func integer(t types.Type) bool {
	basic, ok := t.Underlying().(*types.Basic)

	return ok && basic.Info()&types.IsInteger != 0
}

// sameKind reports whether reflect gives a and b the same Kind: the same basic
// type underneath, or the same sort of type, such as two arrays.
func sameKind(a, b types.Type) bool {
	x, y := a.Underlying(), b.Underlying()

	if bx, ok := x.(*types.Basic); ok {
		by, ok := y.(*types.Basic)

		return ok && bx.Kind() == by.Kind()
	}

	return reflect.TypeOf(x) == reflect.TypeOf(y)
}
