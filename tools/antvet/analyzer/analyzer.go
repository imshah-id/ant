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

// fields says which arguments of a call are field pointers: count of them from
// the one at, or all from there on if count is 0.
type fields struct {
	at, count int
}

func (f fields) of(args []ast.Expr) []ast.Expr {
	if f.at >= len(args) {
		return nil
	}

	if f.count == 0 {
		return args[f.at:]
	}

	return args[f.at:min(f.at+f.count, len(args))]
}

var first = fields{0, 1}

// fieldFuncs lists ant's functions that take field pointers, and where.
var fieldFuncs = map[string]fields{
	"Eq":           first,
	"Ne":           first,
	"Gt":           first,
	"Gte":          first,
	"Lt":           first,
	"Lte":          first,
	"In":           first,
	"NotIn":        first,
	"Between":      first,
	"Like":         first,
	"ILike":        first,
	"IsNull":       first,
	"IsNotNull":    first,
	"Past":         first,
	"Future":       first,
	"Today":        first,
	"InSub":        first,
	"NotInSub":     first,
	"AnyOf":        {1, 0},
	"AllOf":        {1, 0},
	"NoneOf":       {1, 0},
	"EqCol":        {0, 2},
	"Set":          first,
	"SetExpr":      first,
	"Inc":          first,
	"Dec":          first,
	"Col":          first,
	"F":            first,
	"Into":         {0, 2},
	"IntoExpr":     first,
	"SubOf":        {1, 1},
	"SumOf":        {2, 1},
	"AvgOf":        {2, 1},
	"MinOf":        {2, 1},
	"MaxOf":        {2, 1},
	"Pluck":        {2, 1},
	"Sum":          first,
	"Avg":          first,
	"Min":          first,
	"Max":          first,
	"CountOf":      first,
	"Lower":        first,
	"Upper":        first,
	"Length":       first,
	"Collate":      first,
	"PartitionBy":  first,
	"OnConflict":   first,
	"Then":         {1, 1},
	"Has":          first,
	"HasNone":      first,
	"HasOne":       first,
	"CountRelated": first,
	"SumRelated":   {0, 2},
	"AvgRelated":   {0, 2},
	"MinRelated":   {0, 2},
	"MaxRelated":   {0, 2},
	"Related":      {2, 1},
	"RelatedOne":   {2, 1},
	"Link":         {3, 1},
	"Unlink":       {3, 1},
	"SyncLinks":    {3, 1},
	"Toggle":       {3, 1},
	"IsDirty":      {1, 1},
	"Original":     {1, 1},
}

// fieldMethods lists the methods of ant's query builders whose first argument
// is a field pointer, and how a field of another model is judged there: by the
// query's scope, as conditions are, since a subquery may order or group by its
// outer query's columns, or by the query's own models alone, for what it reads
// into its entities or the relations it loads.
var fieldMethods = map[string]judging{
	"OrderBy":       byScope,
	"OrderByDesc":   byScope,
	"ThenBy":        byScope,
	"ThenByDesc":    byScope,
	"GroupBy":       byScope,
	"Column":        byOwn,
	"Omit":          byOwn,
	"With":          byOwn,
	"Include":       byOwn,
	"IncludeOne":    byOwn,
	"IncludeJoined": byOwn,
	"Join":          byOwn,
	"JoinMany":      byOwn,
	"LeftJoin":      byOwn,
	"LeftJoinMany":  byOwn,
	"JoinUsing":     probeOnly,
}

type judging int

const (
	byScope judging = iota + 1
	byOwn
	probeOnly // JoinUsing reads only the column's name, which both sides have
)

// updates lists the methods that write their assignments to the query's rows.
var updates = map[string]bool{
	"Update":          true,
	"UpdateReturning": true,
	"UpdateSQL":       true,
}

// aggregates lists the functions that run a query, ctx and the query first,
// and read a field of its model.
var aggregates = map[string]bool{
	"SumOf": true,
	"AvgOf": true,
	"MinOf": true,
	"MaxOf": true,
	"Pluck": true,
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
			c.checkProbes(call)
			c.checkScope(call, stack)
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
// analyzer does not follow the order code runs in, so it is an ordinary one. So
// is one whose value the analyzer cannot see: a parameter or a receiver, which
// the caller gives, a range variable, or one whose address is taken, through
// which anything may be stored.
func findProbes(pass *analysis.Pass, in *inspector.Inspector) map[types.Object]string {
	assigned := map[types.Object][]ast.Expr{}

	// A nil value is one the analyzer cannot see.
	add := func(id *ast.Ident, value ast.Expr) {
		if object, ok := pass.TypesInfo.ObjectOf(id).(*types.Var); ok {
			assigned[object] = append(assigned[object], value)
		}
	}

	unseen := func(list *ast.FieldList) {
		if list == nil {
			return
		}

		for _, field := range list.List {
			for _, name := range field.Names {
				add(name, nil)
			}
		}
	}

	kinds := []ast.Node{
		(*ast.AssignStmt)(nil),
		(*ast.ValueSpec)(nil),
		(*ast.FuncDecl)(nil),
		(*ast.FuncType)(nil),
		(*ast.RangeStmt)(nil),
		(*ast.UnaryExpr)(nil),
	}

	in.Preorder(kinds, func(n ast.Node) {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if n.Tok != token.ASSIGN && n.Tok != token.DEFINE {
				return
			}

			for i, left := range n.Lhs {
				if id, ok := ast.Unparen(left).(*ast.Ident); ok {
					add(id, valueAt(n.Rhs, len(n.Lhs), i))
				}
			}
		case *ast.ValueSpec:
			if len(n.Values) == 0 {
				return
			}

			for i, name := range n.Names {
				add(name, valueAt(n.Values, len(n.Names), i))
			}
		case *ast.FuncDecl:
			unseen(n.Recv)
		case *ast.FuncType:
			unseen(n.Params)
		case *ast.RangeStmt:
			for _, e := range []ast.Expr{n.Key, n.Value} {
				if id, ok := ast.Unparen(e).(*ast.Ident); ok {
					add(id, nil)
				}
			}
		case *ast.UnaryExpr:
			if id, ok := ast.Unparen(n.X).(*ast.Ident); ok && n.Op == token.AND {
				add(id, nil)
			}
		}
	})

	// A copy may come before what it copies is assigned, and copies may go round
	// a cycle, as in q := p, then p = q. So every assigned variable starts out
	// as a candidate, and those given anything but a probe or a candidate drop
	// out, until none do.
	candidate := map[types.Object]bool{}

	for object := range assigned {
		candidate[object] = true
	}

	for dropped := true; dropped; {
		dropped = false

		for object := range candidate {
			for _, value := range assigned[object] {
				if value == nil ||
					probeSource(pass, value) == "" &&
						!candidate[objectOf(pass, ast.Unparen(value))] {
					delete(candidate, object)

					dropped = true

					break
				}
			}
		}
	}

	// A candidate holds the probe that reaches it through copies, from
	// ant.Fields or another package. One that no probe reaches only copies the
	// others, and holds nil.
	locals := map[types.Object]string{}

	for found := true; found; {
		found = false

		for object := range candidate {
			if _, done := locals[object]; done {
				continue
			}

			for _, value := range assigned[object] {
				model := probeSource(pass, value)
				if model == "" {
					model = locals[objectOf(pass, ast.Unparen(value))]
				}

				if model != "" {
					locals[object] = model
					found = true

					break
				}
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

// valueAt returns the value of the i-th of names assigned from values, or nil
// where one call gives them all.
func valueAt(values []ast.Expr, names, i int) ast.Expr {
	if len(values) != names {
		return nil
	}

	return values[i]
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
			if model == nil || !configures(pass.TypesInfo.Defs[fn.Name]) {
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

	// A return or a goto in the body, not in a closure, may come before the key
	// is set, and skip it.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			returns = true
		case *ast.BranchStmt:
			returns = returns || n.Tok == token.GOTO
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

// configures reports whether a method named Configure is one ant calls: it
// finds the method by reflection and calls it only with one argument, a
// *ModelBuilder. One that takes anything else is never called, so it sets no
// key.
func configures(method types.Object) bool {
	fn, ok := method.(*types.Func)
	if !ok {
		return false
	}

	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Params().Len() != 1 || sig.Variadic() {
		return false
	}

	param := types.Unalias(sig.Params().At(0).Type())
	if _, ok := param.(*types.Pointer); !ok {
		return false
	}

	named := antType(param)

	return named != nil && named.Obj().Name() == "ModelBuilder"
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
		if _, _, args, ok := methodCall(pass, call); ok && len(args) == 1 {
			return fieldPath(pass, args[0]), true
		}

		return "", true // a key, but not one the analyzer can read
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

// unalias resolves aliases all the way into a type, its type arguments and the
// types it is made of included, so that Row[[4]Key] with type Key = int64 is
// written as Row[[4]int64]. Models are compared with types.Identical, which sees
// through aliases itself, so this is for the names that facts and reports give.
func unalias(t types.Type) types.Type {
	switch t := types.Unalias(t).(type) {
	case *types.Pointer:
		return types.NewPointer(unalias(t.Elem()))
	case *types.Slice:
		return types.NewSlice(unalias(t.Elem()))
	case *types.Array:
		return types.NewArray(unalias(t.Elem()), t.Len())
	case *types.Chan:
		return types.NewChan(t.Dir(), unalias(t.Elem()))
	case *types.Map:
		return types.NewMap(unalias(t.Key()), unalias(t.Elem()))
	case *types.Signature:
		return types.NewSignatureType(
			nil,
			nil,
			nil,
			unaliasTuple(t.Params()),
			unaliasTuple(t.Results()),
			t.Variadic(),
		)
	case *types.Struct:
		fields := make([]*types.Var, t.NumFields())
		tags := make([]string, t.NumFields())

		for i := range t.NumFields() {
			f := t.Field(i)
			fields[i] = types.NewField(f.Pos(), f.Pkg(), f.Name(), unalias(f.Type()), f.Embedded())
			tags[i] = t.Tag(i)
		}

		return types.NewStruct(fields, tags)
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

func unaliasTuple(tuple *types.Tuple) *types.Tuple {
	vars := make([]*types.Var, tuple.Len())

	for i := range tuple.Len() {
		v := tuple.At(i)
		vars[i] = types.NewParam(v.Pos(), v.Pkg(), v.Name(), unalias(v.Type()))
	}

	return types.NewTuple(vars...)
}

// name writes a model as reports show it: aliases resolved, packages left out.
func name(t types.Type) string {
	return types.TypeString(unalias(t), func(*types.Package) string { return "" })
}

// names writes the models of a query, such as Post and User for a join.
func names(models []types.Type) string {
	out := make([]string, len(models))

	for i, model := range models {
		out[i] = name(model)
	}

	return strings.Join(out, " and ")
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

// probeOf follows a pointer like &U.Age to the variable it is a field of, and
// returns that, the model of the probe it holds, or "" if it holds none, and
// whether the analyzer can tell. It cannot for a field behind a pointer that is
// itself held in a field, as in &holder.U.Age, or behind what a call returns, as
// in &probe[User]().Age: either may be a probe.
func (c *checker) probeOf(arg ast.Expr) (root ast.Expr, model string, known bool) {
	pass := c.pass

	unary, ok := ast.Unparen(arg).(*ast.UnaryExpr)
	if !ok || unary.Op != token.AND {
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

		if inner, ok := e.(*ast.SelectorExpr); ok && pass.TypesInfo.Selections[inner] != nil &&
			pointee(pass.TypesInfo.TypeOf(inner)) != nil {
			return nil, "", false
		}
	}

	if _, ok := e.(*ast.CallExpr); ok {
		return nil, "", false
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

// pointee returns what a pointer of type t points to, or nil if t is no pointer.
func pointee(t types.Type) types.Type {
	if t == nil {
		return nil
	}

	if pointer, ok := t.Underlying().(*types.Pointer); ok {
		return pointer.Elem()
	}

	return nil
}

func identOf(e ast.Expr) *ast.Ident {
	id, _ := e.(*ast.Ident)

	return id
}

// checkProbes reports field pointers that are not from a probe, among the
// arguments of ant's functions and its builders' methods that take them.
func (c *checker) checkProbes(call *ast.CallExpr) {
	label, args := c.fieldArgs(call)

	for _, arg := range args {
		if _, model, known := c.probeOf(arg); known && model == "" {
			c.pass.Reportf(
				arg.Pos(),
				"%s needs a field of a probe from ant.Fields, not of an ordinary variable",
				label,
			)
		}
	}
}

// fieldArgs returns how reports name call, and those of its arguments that are
// field pointers.
func (c *checker) fieldArgs(call *ast.CallExpr) (string, []ast.Expr) {
	if name := calleeName(c.pass, call); name != "" {
		if f, ok := fieldFuncs[name]; ok {
			return "ant." + name, f.of(call.Args)
		}

		return "", nil
	}

	selector, ok := ast.Unparen(indexed(call.Fun)).(*ast.SelectorExpr)
	if !ok || fieldMethods[selector.Sel.Name] == 0 || !isAntFunc(c.pass, call) {
		return "", nil
	}

	_, recvType, args, ok := methodCall(c.pass, call)
	if !ok || !builder(recvType) {
		return "", nil
	}

	return selector.Sel.Name, first.of(args)
}

// antType returns the named type of ant that t is, or points to, or nil.
func antType(t types.Type) *types.Named {
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

	return named
}

// queryModels returns the models a query reads: T of a *Query[T] or *Table[T],
// and each model of a join. It returns nil for anything else, and when a model
// holds a type parameter, which only an instantiation fills in.
func queryModels(t types.Type) []types.Type {
	named := antType(t)
	if named == nil {
		return nil
	}

	switch named.Obj().Name() {
	case "Query", "Table", "Join2", "Join3", "Join4":
	default:
		return nil
	}

	var out []types.Type

	for arg := range named.TypeArgs().Types() {
		if generic(arg) {
			return nil
		}

		out = append(out, arg)
	}

	return out
}

// queryArg returns T of a *Query[T] or *Table[T], or nil.
func queryArg(t types.Type) types.Type {
	named := antType(t)
	if named == nil || named.Obj().Name() != "Query" && named.Obj().Name() != "Table" {
		return nil
	}

	if models := queryModels(t); len(models) == 1 {
		return models[0]
	}

	return nil
}

// generic reports whether t holds a type parameter anywhere.
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
	case *types.Map:
		return generic(t.Key()) || generic(t.Elem())
	case *types.Signature:
		return generic(t.Params()) || generic(t.Results())
	case *types.Tuple:
		for v := range t.Variables() {
			if generic(v.Type()) {
				return true
			}
		}
	case *types.Struct:
		for f := range t.Fields() {
			if generic(f.Type()) {
				return true
			}
		}
	case *types.Interface:
		for m := range t.ExplicitMethods() {
			if generic(m.Type()) {
				return true
			}
		}

		for e := range t.EmbeddedTypes() {
			if generic(e) {
				return true
			}
		}
	case *types.Union:
		for i := range t.Len() {
			if generic(t.Term(i).Type()) {
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
	"AnyOf":     true,
	"AllOf":     true,
	"NoneOf":    true,
}

// checkScope reports fields of models a query does not read: in its conditions,
// its ordering and grouping, the columns it reads, the relations it loads, the
// assignments of its updates, and the field an aggregate such as ant.SumOf reads
// of it. A subquery may name its outer query's models in its conditions and its
// ordering, so a query is judged there only where it is plainly not one: when
// its chain ends in a call that runs it, such as All, or when it sits inside an
// outer query, whose models it may also name.
func (c *checker) checkScope(call *ast.CallExpr, stack []ast.Node) {
	pass := c.pass

	if name := calleeName(pass, call); aggregates[name] && len(call.Args) == 3 {
		if own := queryModels(pass.TypesInfo.TypeOf(call.Args[1])); own != nil {
			c.judge(call.Args[2:], own, own, "ant."+name)
		}

		return
	}

	selector, ok := ast.Unparen(indexed(call.Fun)).(*ast.SelectorExpr)
	if !ok || !isAntFunc(pass, call) {
		return
	}

	_, recvType, args, ok := methodCall(pass, call)
	if !ok {
		return
	}

	own := queryModels(recvType)
	if own == nil {
		return
	}

	method := selector.Sel.Name

	switch {
	case method == "Where":
		allowed, ok := c.scope(stack, own)
		if !ok {
			return
		}

		for _, condition := range c.conditions(args) {
			if name := calleeName(pass, condition); singleModel[name] {
				c.judge(fieldFuncs[name].of(condition.Args), allowed, own, "")
			}
		}
	case fieldMethods[method] == byScope:
		if allowed, ok := c.scope(stack, own); ok {
			c.judge(first.of(args), allowed, own, method)
		}
	case fieldMethods[method] == byOwn:
		c.judge(first.of(args), own, own, method)
	case updates[method] && len(args) > 0:
		for _, arg := range args[1:] {
			assignment, ok := ast.Unparen(arg).(*ast.CallExpr)
			if !ok {
				continue
			}

			if name := calleeName(pass, assignment); fieldFuncs[name] == first {
				c.judge(first.of(assignment.Args), own, own, "ant."+name)
			}
		}
	}
}

// judge reports each of fields whose probe's model is none of allowed. what
// names the call that takes the field, or is "" for a condition.
func (c *checker) judge(fields []ast.Expr, allowed, own []types.Type, what string) {
	for _, field := range fields {
		root, probe, _ := c.probeOf(field)
		if probe == "" {
			continue
		}

		model := pointee(c.pass.TypesInfo.TypeOf(root))
		if model == nil || slices.ContainsFunc(allowed, func(t types.Type) bool {
			return types.Identical(t, model)
		}) {
			continue
		}

		if what == "" {
			c.pass.Reportf(field.Pos(), "condition on %s in a query of %s", name(model), names(own))
		} else {
			c.pass.Reportf(
				field.Pos(),
				"%s on a field of %s in a query of %s",
				what,
				name(model),
				names(own),
			)
		}
	}
}

// scope returns the models that a query, whose call ends stack, may name where
// a subquery may name its outer query's: its own, and those of the queries it is
// a subquery of. It returns false where the analyzer cannot tell.
func (c *checker) scope(stack []ast.Node, own []types.Type) ([]types.Type, bool) {
	if c.joinsLater(stack) {
		return nil, false
	}

	if c.runs(stack) {
		return own, true
	}

	outer, ok := c.outerModels(stack)
	if !ok {
		return nil, false
	}

	return append(slices.Clone(own), outer...), true
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
// Update, or a function such as ant.SumOf. GroupBy and Join lead to other
// builders, and the chain goes on, as it does through a function such as
// ant.Project or ant.Merge that makes the query part of what it builds. A method
// given its type arguments, as OrderBy[string] is, is followed past them. The
// subquery of a lateral join, one in an outer query's arguments, and one handed
// to a function of one's own do not run there.
func (c *checker) runs(stack []ast.Node) bool {
	node := stack[len(stack)-1]

	for i := len(stack) - 2; i >= 0; i-- {
		switch parent := stack[i].(type) {
		case *ast.ParenExpr:
			node = parent
		case *ast.SelectorExpr:
			call, at := calledAt(stack, i)
			if call == nil || ast.Node(parent.X) != node {
				return false
			}

			if !builder(c.pass.TypesInfo.TypeOf(call)) {
				return true
			}

			node, i = call, at
		case *ast.CallExpr:
			at := argIndex(parent, node)

			// A method expression of ant takes the query it runs or builds on
			// first, as a chain's next call does.
			if at == 0 && methodExpr(c.pass, parent) && isAntFunc(c.pass, parent) {
				if !builder(c.pass.TypesInfo.TypeOf(parent)) {
					return true
				}

				node = parent

				continue
			}

			// Only a function given the query itself runs it or builds on it: one
			// given a condition made of it, as ant.Exists makes, uses a subquery.
			name := calleeName(c.pass, parent)

			if name == "" || at < 0 || !isQuery(c.pass.TypesInfo.TypeOf(parent.Args[at])) ||
				lateral(name) && at != 0 {
				return false
			}

			if !builder(c.pass.TypesInfo.TypeOf(parent)) {
				return true
			}

			node = parent
		default:
			return false
		}
	}

	return false
}

// calledAt returns the call of the method that stack[i] selects, past the type
// arguments it may be given, and where the call is in stack, or nil.
func calledAt(stack []ast.Node, i int) (*ast.CallExpr, int) {
	fun, j := stack[i], i-1

	if j >= 0 {
		switch index := stack[j].(type) {
		case *ast.IndexExpr:
			if ast.Node(index.X) == fun {
				fun, j = index, j-1
			}
		case *ast.IndexListExpr:
			if ast.Node(index.X) == fun {
				fun, j = index, j-1
			}
		}
	}

	if j < 0 {
		return nil, 0
	}

	call, ok := stack[j].(*ast.CallExpr)
	if !ok || ast.Node(call.Fun) != fun {
		return nil, 0
	}

	return call, j
}

func isArg(call *ast.CallExpr, node ast.Node) bool { return argIndex(call, node) >= 0 }

// methodCall returns the receiver of a method call, the type the method belongs
// to, and the arguments the method takes, written as recv.M(args) or as a method
// expression, T.M(recv, args), which takes the receiver first. ok is false for
// any other call.
func methodCall(
	pass *analysis.Pass,
	call *ast.CallExpr,
) (recv ast.Expr, recvType types.Type, args []ast.Expr, ok bool) {
	selector, isSelector := ast.Unparen(indexed(call.Fun)).(*ast.SelectorExpr)
	if !isSelector {
		return nil, nil, nil, false
	}

	switch selection := pass.TypesInfo.Selections[selector]; {
	case selection == nil, selection.Kind() == types.FieldVal:
		return nil, nil, nil, false // a function of a package, as ant.Eq is, or a field
	case selection.Kind() == types.MethodExpr:
		if len(call.Args) == 0 {
			return nil, nil, nil, false
		}

		return call.Args[0], receiverOf(selection), call.Args[1:], true
	default:
		return selector.X, receiverOf(selection), call.Args, true
	}
}

// receiverOf returns the type a selected method belongs to: the receiver's own,
// or, for a method it gets from an embedded field, as a type of one's own that
// embeds an ant query or table does, that field's type.
func receiverOf(selection *types.Selection) types.Type {
	t, index := selection.Recv(), selection.Index()

	for _, i := range index[:len(index)-1] {
		under := types.Unalias(t)
		if pointer, ok := under.(*types.Pointer); ok {
			under = types.Unalias(pointer.Elem())
		}

		fields, ok := under.Underlying().(*types.Struct)
		if !ok {
			return nil
		}

		t = fields.Field(i).Type()
	}

	return t
}

// methodExpr reports whether call is written as a method expression.
func methodExpr(pass *analysis.Pass, call *ast.CallExpr) bool {
	selector, ok := ast.Unparen(indexed(call.Fun)).(*ast.SelectorExpr)
	if !ok {
		return false
	}

	selection := pass.TypesInfo.Selections[selector]

	return selection != nil && selection.Kind() == types.MethodExpr
}

// argIndex returns which of call's arguments node is, or -1.
func argIndex(call *ast.CallExpr, node ast.Node) int {
	return slices.IndexFunc(call.Args, func(a ast.Expr) bool { return ast.Node(a) == node })
}

// lateral reports whether an ant function is a lateral join, whose subquery may
// name the query it joins.
func lateral(name string) bool { return strings.HasSuffix(name, "Lateral") }

// joins reports whether a method or function of ant joins a model in.
// IncludeJoined does not: it reads its row under an alias of its own, which
// conditions cannot name.
func joins(name string) bool {
	return strings.Contains(name, "Join") && !strings.HasSuffix(name, "Joined")
}

// isQuery reports whether t is a query, which a function may run or build on,
// rather than a condition or an expression made of one. A grouped query is one,
// as ant.Project takes it.
func isQuery(t types.Type) bool {
	named := antType(t)
	if named == nil {
		return false
	}

	switch named.Obj().Name() {
	case "Query", "Table", "Join2", "Join3", "Join4", "Grouped", "Projection":
		return true
	}

	return false
}

// builder reports whether t is one of ant's types for building a query, such as
// *Query[T] or *Grouped, rather than what running one returns.
func builder(t types.Type) bool { return antType(t) != nil }

// outerModels returns the models of the queries that enclose the innermost node
// of stack, and whether they are the whole scope: only queries it sits in the
// arguments of count, as a chain's later calls belong to the same query, and so
// does the query a lateral join takes its subquery to. An enclosing builder
// whose models are unknown, such as a grouped query, or a function of one's own,
// makes the scope unknown. So does an outermost query that does not run where
// the analyzer can see it, since it may itself be a subquery of something built
// elsewhere. The walk ends at a statement: what is held in a variable, or built
// in a statement of its own, is used where the analyzer does not follow it. Only
// what a function literal returns, as an Include shape does, goes on to the call
// the literal is given to, since that is the call's query.
func (c *checker) outerModels(stack []ast.Node) ([]types.Type, bool) {
	var (
		out       []types.Type
		outermost = -1
	)

walk:
	for i := len(stack) - 2; i >= 0; i-- {
		switch stack[i].(type) {
		case *ast.ReturnStmt:
			lit := returnedFrom(stack, i)
			if lit < 0 {
				break walk
			}

			// An include's shape is a query ant runs apart, for the related
			// model alone, so nothing outside the shape is in its scope.
			if c.shapes(stack, lit) {
				return out, true
			}

			i = lit

			continue
		case ast.Stmt:
			break walk
		}

		call, ok := stack[i].(*ast.CallExpr)
		if !ok || !isArg(call, stack[i+1]) {
			continue
		}

		// The receiver of a method expression goes on along its chain, as the
		// receiver of a call does, and is not a subquery of it.
		if argIndex(call, stack[i+1]) == 0 && methodExpr(c.pass, call) {
			continue
		}

		if name := calleeName(c.pass, call); name != "" {
			switch at := argIndex(call, stack[i+1]); {
			// Has and the Related functions open a scope of the related model,
			// which the analyzer does not follow.
			case strings.HasPrefix(name, "Has") || strings.HasSuffix(name, "Related"):
				return nil, false
			// A subquery in a join's conditions may name either side.
			case joins(name) && at > 1:
				return nil, false
			case lateral(name) && at == 1:
				models := queryModels(c.pass.TypesInfo.TypeOf(call.Args[0]))
				if models == nil {
					return nil, false
				}

				out = append(out, models...)
				outermost = i
			}

			continue // ant.Exists, ant.SubOf, and the like pass a query on
		}

		// A method expression takes its receiver first, so where an argument
		// sits says nothing the analyzer follows.
		if !isAntFunc(c.pass, call) || methodExpr(c.pass, call) {
			return nil, false
		}

		_, recvType, _, ok := methodCall(c.pass, call)
		if !ok {
			return nil, false
		}

		models := queryModels(recvType)
		if models == nil {
			return nil, false
		}

		out = append(out, models...)
		outermost = i
	}

	if outermost < 0 || !c.runs(stack[:outermost+1]) {
		return nil, false
	}

	return out, true
}

// shapes reports whether the function literal at stack[lit] is the shape given to
// Include or IncludeOne, which builds the query of a related model.
func (c *checker) shapes(stack []ast.Node, lit int) bool {
	if lit == 0 {
		return false
	}

	call, ok := stack[lit-1].(*ast.CallExpr)
	if !ok || argIndex(call, stack[lit]) < 1 || !isAntFunc(c.pass, call) {
		return false
	}

	selector, ok := ast.Unparen(indexed(call.Fun)).(*ast.SelectorExpr)

	return ok && (selector.Sel.Name == "Include" || selector.Sel.Name == "IncludeOne")
}

// returnedFrom returns where in stack the function literal is whose return
// statement is stack[i], or -1 if that returns from a declared function.
func returnedFrom(stack []ast.Node, i int) int {
	for j := i - 1; j >= 0; j-- {
		if _, ok := stack[j].(*ast.FuncLit); ok {
			return j
		}

		// Past the blocks, branches, and loops the return sits in.
		if _, ok := stack[j].(ast.Stmt); !ok {
			return -1
		}
	}

	return -1
}

// joinsLater reports whether the query goes on to a join, by a method of its
// chain or of an outer one, or as the query a join function such as ant.JoinSub
// takes first, since a condition can come before the join that brings its model
// in.
func (c *checker) joinsLater(stack []ast.Node) bool {
	for i, n := range stack {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if joins(n.Sel.Name) {
				return true
			}
		case *ast.CallExpr:
			if i+1 < len(stack) && len(n.Args) > 0 && ast.Node(n.Args[0]) == stack[i+1] &&
				joins(calleeName(c.pass, n)) {
				return true
			}
		}
	}

	return false
}

// checkFind reports a Find key of the wrong type, for a model whose key is its ID
// field, or the single field its Configure method makes the key.
func checkFind(pass *analysis.Pass, call *ast.CallExpr) {
	selector, ok := ast.Unparen(indexed(call.Fun)).(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Find" {
		return
	}

	_, recvType, args, ok := methodCall(pass, call)
	if !ok || len(args) != 2 {
		return
	}

	model := queryArg(recvType)
	if model == nil {
		return
	}

	// An alias of a model has the key of the model it stands for.
	model = types.Unalias(model)
	path := "ID"

	// ant finds Configure by reflection, so one a model gets from a struct it
	// embeds runs too, and sets a key the analyzer does not follow.
	set := types.NewMethodSet(types.NewPointer(model))
	if sel := set.Lookup(
		nil,
		"Configure",
	); sel != nil && len(sel.Index()) > 1 &&
		configures(sel.Obj()) {
		return
	}

	if named, ok := model.(*types.Named); ok {
		fact := new(keyFact)
		if pass.ImportObjectFact(named.Obj(), fact) {
			path = fact.Field
		}
	}

	if path == "" {
		return
	}

	id := lookupPath(pass, model, path)
	if id == nil {
		return
	}

	// ant checks the key by Find's type argument, given or inferred, which the
	// argument converts to, as an untyped constant does.
	key := pass.TypesInfo.TypeOf(args[1])

	if instance, ok := pass.TypesInfo.Instances[selector.Sel]; ok && instance.TypeArgs.Len() == 1 {
		key = instance.TypeArgs.At(0)
	}

	if key == nil || compatible(id.Type(), key) {
		return
	}

	pass.Reportf(
		args[1].Pos(),
		"Find key is %s, and %s has a key of type %s",
		types.TypeString(key, nil),
		name(model),
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
