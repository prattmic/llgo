// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements the Check function, which typechecks a package.

package types

import (
	"fmt"
	"go/ast"
	"go/scanner"
	"go/token"
	"sort"
	"strconv"
)

const debug = false

type checker struct {
	fset    *token.FileSet
	errors  scanner.ErrorList
	types   map[ast.Expr]Type
	methods map[*ast.Object]ObjList
}

func (c *checker) errorf(pos token.Pos, format string, args ...interface{}) string {
	msg := fmt.Sprintf(format, args...)
	c.errors.Add(c.fset.Position(pos), msg)
	return msg
}

// collectFields collects struct fields tok = token.STRUCT), interface methods
// (tok = token.INTERFACE), and function arguments/results (tok = token.FUNC).
func (c *checker) collectFields(tok token.Token, list *ast.FieldList, cycleOk bool) (fields ObjList, tags []string, isVariadic bool) {
	if list != nil {
		for _, field := range list.List {
			ftype := field.Type
			if t, ok := ftype.(*ast.Ellipsis); ok {
				ftype = t.Elt
				isVariadic = true
			}
			typ := c.makeType(ftype, cycleOk)
			if isVariadic {
				typ = &Slice{Elt: typ}
			}
			tag := ""
			if field.Tag != nil {
				assert(field.Tag.Kind == token.STRING)
				tag, _ = strconv.Unquote(field.Tag.Value)
			}
			if len(field.Names) > 0 {
				// named fields
				for _, name := range field.Names {
					obj := name.Obj
					obj.Type = typ
					fields = append(fields, obj)
					if tok == token.STRUCT {
						tags = append(tags, tag)
					}
				}
			} else {
				// anonymous field
				switch tok {
				case token.STRUCT:
					tags = append(tags, tag)
					fallthrough
				case token.FUNC:
					obj := ast.NewObj(ast.Var, "")
					obj.Type = typ
					fields = append(fields, obj)
				case token.INTERFACE:
					utyp := Underlying(typ)
					if typ, ok := utyp.(*Interface); ok {
						// TODO(gri) This is not good enough. Check for double declarations!
						fields = append(fields, typ.Methods...)
					} else if _, ok := utyp.(*Bad); !ok {
						// if utyp is Bad, don't complain (the root cause was reported before)
						c.errorf(ftype.Pos(), "interface contains embedded non-interface type")
					}
				default:
					panic("unreachable")
				}
			}
		}
	}
	return
}

// collectMethods collects the method declarations from an AST File and
// returns a mapping from receiver types to their method FuncDecl's.
func (c *checker) collectMethods(file *ast.File) {
	for _, decl := range file.Decls {
		if funcdecl, ok := decl.(*ast.FuncDecl); ok && funcdecl.Recv != nil {
			recvField := funcdecl.Recv.List[0]
			var recv *ast.Ident
			switch typ := recvField.Type.(type) {
			case *ast.StarExpr:
				recv = typ.X.(*ast.Ident)
			case *ast.Ident:
				recv = typ
			default:
				panic("bad receiver type expression")
			}

			// The Obj field of the funcdecl wll be nil, so we'll have to
			// create a new one.
			funcdecl.Name.Obj = ast.NewObj(ast.Fun, funcdecl.Name.String())
			funcdecl.Name.Obj.Decl = funcdecl
			c.methods[recv.Obj] = append(c.methods[recv.Obj], funcdecl.Name.Obj)
		}
	}
}

// decomposeRepeatConsts looks for each instance of a constant list, and
// for constants without a value, associates the preceding constant's
// value and type (if any).
func (c *checker) decomposeRepeatConsts(file *ast.File) {
	for _, decl := range file.Decls {
		gendecl, ok := decl.(*ast.GenDecl)
		if ok && gendecl.Tok == token.CONST {
			var predValueSpec *ast.ValueSpec
			for _, spec := range gendecl.Specs {
				valspec := spec.(*ast.ValueSpec)
				if len(valspec.Values) > 0 {
					predValueSpec = valspec
				} else {
					valspec.Type = predValueSpec.Type
					valspec.Values = predValueSpec.Values
				}
			}
		}
	}
}

// untypedPriority returns an integer priority value that corresponds
// to the given type's position in the sequence:
//     integer, character, floating-point, complex.
func untypedPriority(t Type) int {
	switch t {
	case Int.Underlying:
		return 0
	case Rune.Underlying:
		return 1
	case Float64.Underlying:
		return 2
	case Complex128.Underlying:
		return 3
	}
	panic(fmt.Sprintf("unhandled (%T, %v)", t, t))
}

// checkExpr type checks an expression and returns its type.
//
// TODO get rid of the assignee crap, and keep a context stack.
// We'll also need context for ReturnStmt (in checkStmt).
func (c *checker) checkExpr(x ast.Expr, assignees []*ast.Ident) (typ Type) {
	if typ, ok := c.types[x]; ok {
		return typ
	}
	defer func() {
		if typ != nil {
			c.types[x] = typ
		}
	}()
	// multi-value assignment handled within CallExpr.
	if len(assignees) == 1 && assignees[0] != nil {
		defer func() {
			a := assignees[0]
			if a.Obj.Type == nil {
				a.Obj.Type = typ
			} else {
				// TODO convert rhs type to lhs.
			}
		}()
	}

	switch x := x.(type) {
	case *ast.Ident:
		// type name
		obj := x.Obj
		if obj == nil {
			// unresolved identifier (error has been reported before)
			return &Bad{Msg: "unresolved identifier"}
		}
		if obj.Kind != ast.Var && obj.Kind != ast.Con && obj.Kind != ast.Fun {
			msg := c.errorf(x.Pos(),
				"%s is neither function, variable nor constant",
				x.Name)
			return &Bad{Msg: msg}
		}
		if obj.Kind == ast.Con && obj.Name == "nil" && obj.Decl == nil {
			// TODO check assignee is suitable for taking "nil".
			return &Bad{Msg: "nil typechecking unimplemented"}
		}
		c.checkObj(obj, true)
		return obj.Type.(Type)

	case *ast.BasicLit:
		switch x.Kind {
		case token.INT:
			return Int.Underlying
		case token.FLOAT:
			return Float64.Underlying
		case token.IMAG:
			return Complex128.Underlying
		case token.CHAR:
			return Rune.Underlying
		case token.STRING:
			return String.Underlying
		}

	case *ast.CompositeLit:
		// TODO do this properly.
		return c.makeType(x.Type, true)

	case *ast.BinaryExpr:
		xType := c.checkExpr(x.X, nil)
		yType := c.checkExpr(x.Y, nil)
		_, xUntyped := xType.(*Basic)
		_, yUntyped := yType.(*Basic)
		switch x.Op {
		case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
			// TODO check the operands are comparable.
			// TODO check when to use untyped bool.
			return Bool
		case token.SHL, token.SHR:
			// TODO check right operand is unsigned integer, or untyped
			// constant convertible to unsigned integer.

			// If the left operand of a non-constant shift expression is an
			// untyped constant, the type of the constant is what it would
			// be if the shift expression were replaced by its left operand
			// alone; the type is int if it cannot be determined from thexi
			// context
			// TODO check if rhs is non-const.
			//if xUntyped && !isConst(x.Y) {
			//}
			return Int
		default:
			if xUntyped && yUntyped {
				// Except for shift operations, if the operands of a binary
				// operation are different kinds of untyped constants, the
				// operation and, for non-boolean operations, the result use
				// the kind that appears later in this list:
				//     integer, character, floating-point, complex.
				switch x := untypedPriority(xType) - untypedPriority(yType); {
				case x <= 0:
					return xType
				case x > 0:
					return yType
				}
			} else if xUntyped {
				// Convert x.X's type to x.Y's.
				c.types[x.X] = yType
				return yType
			} else if yUntyped {
				// Convert x.Y's type to x.X's.
				c.types[x.Y] = xType
				return xType
			}
			return xType
		}
		panic("unreachable")

	case *ast.UnaryExpr:
		optype := c.checkExpr(x.X, nil)
		switch x.Op {
		case token.ADD, token.SUB: // +, -
			// TODO allow int, float (complex?)
			return optype
		case token.NOT: // !
			// TODO Make sure it's a bool.
			return optype
		case token.XOR: // ^
			// TODO allow int
			return optype
		case token.MUL: // *
			if ptr, ok := Underlying(optype).(*Pointer); ok {
				return ptr.Base
			}
			msg := c.errorf(x.Pos(), "cannot dereference non-pointer")
			return &Bad{Msg: msg}
		case token.AND: // &
			// TODO check operand is addressable
			return &Pointer{Base: optype}
		case token.ARROW: // <-
			// TODO check channel direction
			if ch, ok := Underlying(optype).(*Chan); ok {
				if len(assignees) > 0 {
					if assignees[0] != nil {
						assignees[0].Obj.Type = ch.Elt
					}
					if len(assignees) == 2 && assignees[1] != nil {
						assignees[1].Obj.Type = Bool
					}
				}
				return ch.Elt
			}
			msg := c.errorf(x.Pos(), "cannot receive from non-channel")
			return &Bad{Msg: msg}
		}
		panic("unreachable")

	case *ast.CallExpr:
		// First check if it's a type conversion.
		if len(x.Args) == 1 && isType(x.Fun) {
			typ := c.makeType(x.Fun, true)
			// TODO check conversion is valid.
			c.checkExpr(x.Args[0], nil)
			return typ
		}

		args := x.Args
		switch x := x.Fun.(type) {
		case *ast.SelectorExpr:
			// check for unsafe functions.
			if ident, ok := x.X.(*ast.Ident); ok && ident.Obj.Data == Unsafe.Data {
				switch x.Sel.Name {
				case "Offsetof":
					if len(args) > 0 {
						if _, ok := args[0].(*ast.SelectorExpr); !ok {
							// TODO format args
							return &Bad{Msg: fmt.Sprintf("invalid expression %s.%s", x.X, x.Sel)}
						}
						// TODO check arg is a struct field selector.
					}
					fallthrough
				case "Alignof":
					fallthrough
				case "Sizeof":
					if len(args) < 1 {
						return &Bad{Msg: fmt.Sprintf("missing argument for %s.%s", x.X, x.Sel)}
					} else if len(args) > 1 {
						return &Bad{Msg: fmt.Sprintf("extra arguments for %s.%s", x.X, x.Sel)}
					}
				default:
					return &Bad{Msg: fmt.Sprintf("undefined: %s.%s", x.X, x.Sel)}
				}
				return Uintptr
			}

		case *ast.Ident:
			// check for builtin functions.
			if x.Obj.Kind == ast.Fun && x.Obj.Decl == nil {
				// TODO check args
				switch x.Name {
				//case "append":
				//case "cap":
				//case "close":
				//case "complex":
				case "copy":
					/*dst := */ c.checkExpr(args[0], nil)
					/*src := */ c.checkExpr(args[1], nil)
					// TODO check src, dst have same elt T and assignable to []T.
					// (or) src is string, dst is assignable to byte[]
					return Int
				case "delete":
					m := c.checkExpr(args[0], nil)
					if _, ok := Underlying(m).(*Map); !ok {
						msg := c.errorf(x.Pos(), "delete must be called with a map type")
						return &Bad{Msg: msg}
					} else {
						k := c.checkExpr(args[1], nil)
						_ = k
						// TODO check key is assignable to map's key type.
						return nil
					}
				case "len":
					return Int
				case "make":
					t := c.makeType(args[0], true)
					// TODO check len/cap args.
					/*
						switch t := Underlying(t).(type) {
						case *Slice:
						case *Map:
						case *Chan:
						}
					*/
					return t
				case "new":
					t := c.makeType(args[0], true)
					return &Pointer{Base: t}
				case "println":
					fallthrough
				case "print":
					// TODO check args are expressions.
					return nil
				case "imag", "real":
					arg := c.checkExpr(args[0], nil)
					switch Underlying(arg) {
					case Complex64:
						return Float32
					case Complex128:
						return Float64
					default:
						msg := c.errorf(x.Pos(), "%s must be called with a complex type", x.Name)
						return &Bad{Msg: msg}
					}
				case "panic":
					// TODO check arg is an expression.
					return nil
				//case "recover":
				default:
					panic(fmt.Sprintf("unhandled builtin function: %s", x.Name))
				}
			} else if x.Obj.Kind == ast.Typ {
				// TODO check type can be converted.
				c.checkObj(x.Obj, false)
				return x.Obj.Type.(Type)
			}
		}

		var fntype *Func
		switch t := c.checkExpr(x.Fun, nil).(type) {
		case *Func:
			fntype = t
		case *Bad:
			return t
		default:
			// TODO
		}

		// TODO check arg types
		for _, arg := range x.Args {
			c.checkExpr(arg, nil)
		}

		for _, obj := range fntype.Results {
			c.checkObj(obj, false)
		}
		if len(assignees) > 1 {
			for i, obj := range fntype.Results {
				if assignees[i] != nil {
					if assignees[i].Obj.Type == nil {
						assignees[i].Obj.Type = obj.Type
					} else {
						// TODO convert rhs type to lhs.
					}
				}
			}
		}
		if len(fntype.Results) == 1 {
			return fntype.Results[0].Type.(Type)
		}
		return nil // nil or multi-value

	case *ast.SelectorExpr:
		if ident, ok := x.X.(*ast.Ident); ok {
			// qualified identifier
			if obj := ident.Obj; obj != nil && obj.Kind == ast.Pkg {
				pkgscope := obj.Data.(*ast.Scope)
				x.Sel.Obj = pkgscope.Lookup(x.Sel.Name)
				return c.checkExpr(x.Sel, nil)
			}
		}

		name := x.Sel.Name
		t := c.checkExpr(x.X, nil)
		if iface, ok := Underlying(t).(*Interface); ok {
			i := sort.Search(len(iface.Methods), func(i int) bool {
				return iface.Methods[i].Name >= name
			})
			if i < len(iface.Methods) && iface.Methods[i].Name == name {
				x.Sel.Obj = iface.Methods[i]
			}
		} else {
			// Do a breadth-first search on the type for a method of field.
			// We don't stop at the first find, but instead we go the full
			// breadth (but not depth) to ensure no there is no ambiguity in
			// the selection.
			curr := []Type{t}
			for x.Sel.Obj == nil && len(curr) > 0 {
				found := 0
				next := make([]Type, 0)
				for _, t := range curr {
					// Selectors automatically dereference pointers to structs.
					// We can safely do this here because pointer types may not
					// be method receivers.
					if p, ok := Underlying(t).(*Pointer); ok {
						if _, ok := Underlying(p.Base).(*Struct); ok {
							t = p.Base
						}
					}

					if n, ok := t.(*Name); ok {
						i := sort.Search(len(n.Methods), func(i int) bool {
							return n.Methods[i].Name >= name
						})
						if i < len(n.Methods) && n.Methods[i].Name == name {
							x.Sel.Obj = n.Methods[i]
							found++
						}
					}

					if t, ok := Underlying(t).(*Struct); ok {
						if i, ok := t.FieldIndices[name]; ok {
							x.Sel.Obj = t.Fields[i]
							found++
						} else {
							// Add embedded types to the next set of types
							// to check.
							for _, field := range t.Fields {
								if field.Name == "" {
									c.checkObj(field, false)
									next = append(next, field.Type.(Type))
								}
							}
						}
					}
				}

				if found > 1 {
					msg := c.errorf(x.Pos(),
						"ambiguous selector %s.%s", x.X, x.Sel)
					return &Bad{Msg: msg}
				}
				curr = next
			}
		}

		if x.Sel.Obj == nil {
			msg := c.errorf(x.Pos(),
				"failed to resolve selector %s.%s", x.X, x.Sel)
			return &Bad{Msg: msg}
		} else {
			c.checkObj(x.Sel.Obj, false)
			return x.Sel.Obj.Type.(Type)
		}

	case *ast.IndexExpr:
		// TODO check index type is suitable for indexing.
		//indexType := c.checkExpr(x.Index, nil)
		c.checkExpr(x.Index, nil)
		containerType := c.checkExpr(x.X, nil)

		switch t := Underlying(containerType).(type) {
		case *Bad:
			return t
		case *Pointer:
			if t, ok := Underlying(t.Base).(*Array); ok {
				// TODO check const integer index
				return t.Elt
			} else {
				msg := c.errorf(x.Pos(),
					"attempted to index a pointer to non-array type")
				return &Bad{Msg: msg}
			}
		case *Array:
			// TODO check const integer index
			return t.Elt
		case *Slice:
			return t.Elt
		case *Map:
			// TODO check key is appropriate
			if len(assignees) > 1 {
				if assignees[0] != nil && assignees[0].Obj.Type == nil {
					assignees[0].Obj.Type = t.Elt
				}
				if assignees[1] != nil && assignees[1].Obj.Type == nil {
					assignees[0].Obj.Type = Bool
				}
			}
			return t.Elt
		case *Name:
			// If we receive a Name here, then we must have a named basic
			// type. The only basic type supporting indexing is string.
			if t.Underlying == String.Underlying {
				return Byte
			}
			msg := c.errorf(x.Pos(), "%s type does not support indexing", t)
			return &Bad{Msg: msg}
		case *Basic:
			if t == String.Underlying {
				return Byte
			}
			msg := c.errorf(x.Pos(), "%s type does not support indexing", t)
			return &Bad{Msg: msg}
		}
		fmt.Println(c.fset.Position(x.Pos()))
		panic(c.errorf(x.Pos(), "unreachable (%T)", containerType))

	case *ast.ParenExpr:
		return c.checkExpr(x.X, assignees)

	case *ast.StarExpr:
		t := c.checkExpr(x.X, nil)
		if t, ok := Underlying(t).(*Pointer); ok {
			return t.Base
		}
		msg := c.errorf(x.Pos(), "cannot dereference non-pointer")
		return &Bad{Msg: msg}

	case *ast.TypeAssertExpr:
		// TODO perform static type assertions.
		//from := c.checkExpr(x.X, nil)
		to := c.makeType(x.Type, true)
		if len(assignees) > 1 {
			if assignees[0] != nil && assignees[0].Obj.Type == nil {
				assignees[0].Obj.Type = to
			}
			if assignees[1] != nil && assignees[1].Obj.Type == nil {
				assignees[1].Obj.Type = Bool
			}
		}
		return to

	case *ast.SliceExpr:
		// TODO check indices are appropriately typed.
		if x.Low != nil {
			c.checkExpr(x.Low, nil)
		}
		if x.High != nil {
			c.checkExpr(x.High, nil)
		}

		lhs := c.checkExpr(x.X, nil)
		switch t := Underlying(lhs).(type) {
		case *Pointer:
			if t, ok := t.Base.(*Array); ok {
				return &Slice{Elt: t.Elt}
			}
		case *Array:
			// TODO check array is addressable.
			return &Slice{Elt: t.Elt}
		case *Slice:
			return lhs
		case *Name:
			if Underlying(t) == Underlying(String) {
				return lhs
			}
		case *Basic:
			if t == String.Underlying {
				return lhs
			}
		}
		msg := c.errorf(x.Pos(), "invalid type for slice expression")
		return &Bad{Msg: msg}

	case *ast.FuncLit:
		t := c.makeType(x.Type, false)
		c.checkStmt(x.Body)
		return t
	}

	panic(fmt.Sprintf("unreachable (%T)", x))
}

// makeType makes a new type for an AST type specification x or returns
// the type referred to by a type name x. If cycleOk is set, a type may
// refer to itself directly or indirectly; otherwise cycles are errors.
func (c *checker) makeType(x ast.Expr, cycleOk bool) (typ Type) {
	if debug {
		fmt.Printf("makeType (cycleOk = %v)\n", cycleOk)
		ast.Print(c.fset, x)
		defer func() {
			fmt.Printf("-> %T %v\n\n", typ, typ)
		}()
	}

	switch t := x.(type) {
	case *ast.BadExpr:
		return &Bad{}

	case *ast.Ident:
		// type name
		obj := t.Obj
		if obj == nil {
			// unresolved identifier (error has been reported before)
			return &Bad{Msg: "unresolved identifier"}
		}
		if obj.Kind != ast.Typ {
			msg := c.errorf(t.Pos(), "%s is not a type", t.Name)
			return &Bad{Msg: msg}
		}
		c.checkObj(obj, cycleOk)
		if !cycleOk && obj.Type.(*Name).Underlying == nil {
			// TODO(gri) Enable this message again once its position
			// is independent of the underlying map implementation.
			// msg := c.errorf(obj.Pos(), "illegal cycle in declaration of %s", obj.Name)
			msg := "illegal cycle"
			return &Bad{Msg: msg}
		}
		return obj.Type.(Type)

	case *ast.ParenExpr:
		return c.makeType(t.X, cycleOk)

	case *ast.SelectorExpr:
		// qualified identifier
		// TODO (gri) eventually, this code belongs to expression
		//            type checking - here for the time being
		if ident, ok := t.X.(*ast.Ident); ok {
			if obj := ident.Obj; obj != nil {
				if obj.Kind != ast.Pkg {
					msg := c.errorf(ident.Pos(), "%s is not a package", obj.Name)
					return &Bad{Msg: msg}
				}
				pkgscope := obj.Data.(*ast.Scope)
				t.Sel.Obj = pkgscope.Lookup(t.Sel.Name)
				return c.makeType(t.Sel, cycleOk)
			}
		}
		// TODO(gri) can this really happen (the parser should have excluded this)?
		msg := c.errorf(t.Pos(), "expected qualified identifier")
		return &Bad{Msg: msg}

	case *ast.StarExpr:
		return &Pointer{Base: c.makeType(t.X, true)}

	case *ast.ArrayType:
		if t.Len != nil {
			// TODO(gri) compute length
			return &Array{Elt: c.makeType(t.Elt, cycleOk)}
		}
		return &Slice{Elt: c.makeType(t.Elt, true)}

	case *ast.StructType:
		fields, tags, _ := c.collectFields(token.STRUCT, t.Fields, cycleOk)
		indices := make(map[string]uint64)
		for i, f := range fields {
			if f.Name == "" {
				typ := f.Type
				if ptr, ok := typ.(*Pointer); ok {
					typ = ptr.Base
				}
				indices[typ.(*Name).Obj.Name] = uint64(i)
			} else {
				indices[f.Name] = uint64(i)
			}
		}
		return &Struct{Fields: fields, Tags: tags, FieldIndices: indices}

	case *ast.FuncType:
		params, _, isVariadic := c.collectFields(token.FUNC, t.Params, true)
		results, _, _ := c.collectFields(token.FUNC, t.Results, true)
		return &Func{Recv: nil, Params: params, Results: results, IsVariadic: isVariadic}

	case *ast.InterfaceType:
		methods, _, _ := c.collectFields(token.INTERFACE, t.Methods, cycleOk)
		methods.Sort()
		return &Interface{Methods: methods}

	case *ast.MapType:
		return &Map{Key: c.makeType(t.Key, true), Elt: c.makeType(t.Value, true)}

	case *ast.ChanType:
		return &Chan{Dir: t.Dir, Elt: c.makeType(t.Value, true)}
	}

	panic(fmt.Sprintf("unreachable (%T)", x))
}

// isType checks if an expression is a type.
func isType(x ast.Expr) bool {
	switch t := x.(type) {
	case *ast.Ident:
		return t.Obj != nil && t.Obj.Kind == ast.Typ
	case *ast.ParenExpr:
		return isType(t.X)
	case *ast.SelectorExpr:
		// qualified identifier
		if ident, ok := t.X.(*ast.Ident); ok {
			if obj := ident.Obj; obj != nil {
				if obj.Kind != ast.Pkg {
					return false
				}
				pkgscope := obj.Data.(*ast.Scope)
				obj := pkgscope.Lookup(t.Sel.Name)
				return obj != nil && obj.Kind == ast.Typ
			}
		}
		return false
	case *ast.StarExpr:
		return isType(t.X)
	case *ast.ArrayType,
		*ast.StructType,
		*ast.FuncType,
		*ast.InterfaceType,
		*ast.MapType,
		*ast.ChanType:
		return true
	}
	return false
}

// checkStmt type checks a statement.
func (c *checker) checkStmt(s ast.Stmt) {
	switch s := s.(type) {
	case *ast.AssignStmt:
		// TODO Each left-hand side operand must be addressable,
		// a map index expression, or the blank identifier. Operands
		// may be parenthesized.
		if len(s.Rhs) == 1 {
			idents := make([]*ast.Ident, len(s.Lhs))
			for i, e := range s.Lhs {
				if ident, ok := e.(*ast.Ident); ok {
					idents[i] = ident
				} else {
					c.checkExpr(e, nil)
				}
			}
			c.checkExpr(s.Rhs[0], idents)
		} else {
			idents := make([]*ast.Ident, 1)
			for i, e := range s.Rhs {
				if ident, ok := s.Lhs[i].(*ast.Ident); ok {
					idents[0] = ident
					c.checkExpr(e, idents)
				} else {
					c.checkExpr(e, nil)
				}
			}
		}

	case *ast.BlockStmt:
		for _, s := range s.List {
			c.checkStmt(s)
		}

	case *ast.ExprStmt:
		c.checkExpr(s.X, nil)

	case *ast.BranchStmt:
		// no-op

	case *ast.DeclStmt:
		// Only a GenDecl/ValueSpec is permissible in a statement.
		decl := s.Decl.(*ast.GenDecl)
		for _, spec := range decl.Specs {
			spec := spec.(*ast.ValueSpec)
			for _, name := range spec.Names {
				c.checkObj(name.Obj, true)
			}
		}
	//case *ast.DeferStmt:
	//case *ast.EmptyStmt:

	case *ast.ForStmt:
		if s.Init != nil {
			c.checkStmt(s.Init)
		}
		// TODO make sure cond expr is some sort of bool.
		c.checkExpr(s.Cond, nil)
		if s.Post != nil {
			c.checkStmt(s.Post)
		}
		c.checkStmt(s.Body)

	//case *ast.IncDecStmt:
	//case *ast.GoStmt:

	case *ast.IfStmt:
		if s.Init != nil {
			c.checkStmt(s.Init)
		}
		// TODO make sure cond expr is some sort of bool.
		c.checkExpr(s.Cond, nil)
		c.checkStmt(s.Body)
		if s.Else != nil {
			c.checkStmt(s.Else)
		}

	case *ast.IncDecStmt:
		// TODO check operand is addressable.
		// TODO check operand type is suitable for inc/dec.
		c.checkExpr(s.X, nil)

	case *ast.LabeledStmt:
		c.checkStmt(s.Stmt)

	case *ast.RangeStmt:
		var k, v Type
		switch x := Underlying(c.checkExpr(s.X, nil)).(type) {
		case *Pointer:
			if x, ok := Underlying(x.Base).(*Array); ok {
				k, v = Int, x.Elt
			} else {
				c.errorf(s.Pos(), "invalid type for range")
				return
			}
		case *Array:
			k, v = Int, x.Elt
		case *Slice:
			k, v = Int, x.Elt
		case *Map:
			k, v = x.Key, x.Elt
		case *Chan:
			k = x.Elt
			if s.Value != nil {
				c.errorf(s.Pos(), "too many variables in range")
				return
			}
		default:
			c.errorf(s.Pos(), "invalid type for range")
			return
		}

		// TODO check key, value are addressable and assignable from range
		// values.
		if ident, ok := s.Key.(*ast.Ident); ok && ident.Obj.Type == nil {
			ident.Obj.Type = k
		} else {
			c.checkExpr(s.Key, nil)
		}
		if s.Value != nil {
			if ident, ok := s.Value.(*ast.Ident); ok && ident.Obj.Type == nil {
				ident.Obj.Type = v
			} else {
				c.checkExpr(s.Value, nil)
			}
		}
		c.checkStmt(s.Body)

	case *ast.ReturnStmt:
		// TODO we need to check the result type(s) against the
		// current function's declared return type(s). We need
		// context for that.
		for _, e := range s.Results {
			c.checkExpr(e, nil)
		}

	//case *ast.SelectStmt:
	//case *ast.SendStmt:

	case *ast.SwitchStmt:
		if s.Init != nil {
			c.checkStmt(s.Init)
		}
		tag := Bool.Underlying // omitted tag == "true"
		if s.Tag != nil {
			tag = c.checkExpr(s.Tag, nil)
		}
		for _, s_ := range s.Body.List {
			cc := s_.(*ast.CaseClause)
			for _, e := range cc.List {
				// TODO check type is comparable to tag.
				t := c.checkExpr(e, nil)
				_, _ = t, tag
			}
			for _, s := range cc.Body {
				c.checkStmt(s)
			}
		}

	//case *ast.TypeSwitchStmt:

	default:
		panic(fmt.Sprintf("unimplemented %T", s))
	}
}

// checkObj type checks an object.
func (c *checker) checkObj(obj *ast.Object, ref bool) {
	if obj.Type != nil {
		// object has already been type checked
		return
	}

	switch obj.Kind {
	case ast.Bad:
		// ignore

	case ast.Con:
		valspec := obj.Decl.(*ast.ValueSpec)
		if valspec.Type != nil {
			obj.Type = c.makeType(valspec.Type, ref)
			for _, name := range valspec.Names {
				name.Obj.Type = obj.Type
			}
		}
		if valspec.Values != nil {
			for i, name := range valspec.Names {
				c.checkExpr(valspec.Values[i], []*ast.Ident{name})
			}
		}

	case ast.Typ:
		typ := &Name{Obj: obj}
		obj.Type = typ // "mark" object so recursion terminates
		typ.Underlying = Underlying(c.makeType(obj.Decl.(*ast.TypeSpec).Type, ref))
		if methobjs := c.methods[obj]; methobjs != nil {
			methobjs.Sort()
			typ.Methods = methobjs
			for _, m := range typ.Methods {
				c.checkObj(m, ref)
			}
		}

	case ast.Var:
		var names []*ast.Ident
		var values []ast.Expr
		var typexpr ast.Expr
		switch x := obj.Decl.(type) {
		case *ast.ValueSpec:
			names = x.Names
			values = x.Values
			typexpr = x.Type
		case *ast.Field:
			names = x.Names
			typexpr = x.Type
		default:
			panic(fmt.Sprintf("unimplemented (%T)", x))
		}
		if names != nil { // nil for anonymous field
			var typ Type
			if typexpr != nil {
				typ = c.makeType(typexpr, ref)
				for _, name := range names {
					if name.Obj != nil {
						name.Obj.Type = typ
					}
				}
			}
			if len(values) == 1 && len(names) > 1 {
				// multi-value assignment
				c.checkExpr(values[0], names)
			} else if len(values) == len(names) {
				for i, name := range names {
					c.checkExpr(values[i], []*ast.Ident{name})
				}
			}
		}

	case ast.Fun:
		fndecl := obj.Decl.(*ast.FuncDecl)
		obj.Type = c.makeType(fndecl.Type, ref)
		fn := obj.Type.(*Func)
		if fndecl.Recv != nil {
			recvField := fndecl.Recv.List[0]
			if len(recvField.Names) > 0 {
				fn.Recv = recvField.Names[0].Obj
				c.checkObj(fn.Recv, ref)
			} else {
				fn.Recv = ast.NewObj(ast.Var, "_")
				fn.Recv.Type = c.makeType(recvField.Type, ref)
			}
		} else {
			// Only check body of non-method functions. We check method
			// bodies later, to avoid references to incomplete types.
			if fndecl.Body != nil {
				c.checkStmt(fndecl.Body)
			}
		}

	default:
		panic("unreachable")
	}
}

// Check typechecks a package.
// It augments the AST by assigning types to all ast.Objects and returns a map
// of types for all expression nodes in statements, and a scanner.ErrorList if
// there are errors.
//
func Check(fset *token.FileSet, pkg *ast.Package) (types map[ast.Expr]Type, err error) {
	var c checker
	c.fset = fset
	c.types = make(map[ast.Expr]Type)
	c.methods = make(map[*ast.Object]ObjList)

	for _, file := range pkg.Files {
		c.decomposeRepeatConsts(file)
		c.collectMethods(file)
	}

	for _, obj := range pkg.Scope.Objects {
		c.checkObj(obj, false)
	}

	for _, methods := range c.methods {
		for _, m := range methods {
			if f := m.Decl.(*ast.FuncDecl); f.Body != nil {
				c.checkStmt(f.Body)
			}
		}
	}

	c.errors.RemoveMultiples()
	return c.types, c.errors.Err()
}

// vim: set ft=go :
