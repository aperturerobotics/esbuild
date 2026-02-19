// Package cjsexports detects CommonJS named exports from JavaScript source code.
//
// It parses JavaScript using esbuild's internal parser and walks the AST to
// find CJS export patterns such as exports.foo, module.exports = {...}, and
// Object.defineProperty(exports, ...).
package cjsexports

import (
	"regexp"
	"sort"
	"strings"

	"github.com/aperturerobotics/esbuild/internal/ast"
	"github.com/aperturerobotics/esbuild/internal/helpers"
	"github.com/aperturerobotics/esbuild/internal/js_ast"
	"github.com/aperturerobotics/esbuild/internal/js_parser"
	"github.com/aperturerobotics/esbuild/internal/logger"
)

// Result contains the detected CJS exports from a module.
type Result struct {
	// Exports are the named export identifiers found.
	Exports []string
	// Reexports are module paths being re-exported via require().
	Reexports []string
}

// Options configures CJS export detection.
type Options struct {
	// NodeEnv is the value of process.env.NODE_ENV for conditional branch evaluation.
	// Common values: "production", "development". Empty means no evaluation.
	NodeEnv string
	// CallMode analyzes function return exports (for module.exports = function(){...}).
	CallMode bool
}

// Parse analyzes JavaScript source code and returns detected CJS exports.
func Parse(source string, filename string, opts Options) (*Result, error) {
	log := logger.NewDeferLog(logger.DeferLogAll, logger.LevelSilent, nil)
	src := logger.Source{
		Contents:       source,
		IdentifierName: filename,
		KeyPath:        logger.Path{Text: filename},
	}

	tree, ok := js_parser.Parse(log, src, js_parser.Options{})
	if !ok {
		msgs := log.Done()
		if len(msgs) > 0 {
			return nil, &ParseError{Messages: msgs}
		}
		return nil, &ParseError{}
	}
	log.Done()

	w := &walker{
		tree:      &tree,
		opts:      opts,
		exports:   make(map[string]struct{}),
		reexports: make(map[string]struct{}),
		// Track variable assignments: identifier ref -> what it holds
		varRequire:              make(map[ast.Ref]string),    // var x = require("mod") -> ref(x) -> "mod"
		varExports:              make(map[ast.Ref]struct{}),  // var e = exports -> ref(e) is alias of exports
		varModExports:           make(map[ast.Ref]struct{}),  // var m = module.exports -> ref(m) is alias of module.exports
		varObject:               make(map[ast.Ref]*objInfo),  // var o = { ... } -> ref(o) -> object info
		varFunc:                 make(map[ast.Ref]*funcInfo), // function f() or var f = function/arrow -> ref(f) -> func info
		nodeEnvAliases:          make(map[ast.Ref]struct{}),  // variables holding process.env.NODE_ENV value
		moduleExportsOverridden: false,
	}

	w.analyze()

	// Check for annotation pattern: 0 && (module.exports = {...})
	// esbuild's parser constant-folds this away, so we need a text scan.
	w.scanAnnotationPattern(source, filename)

	result := &Result{
		Exports:   w.sortedExports(),
		Reexports: w.sortedReexports(),
	}
	return result, nil
}

// ParseError is returned when parsing fails.
type ParseError struct {
	Messages logger.SortableMsgs
}

// Error implements the error interface.
func (e *ParseError) Error() string {
	if len(e.Messages) > 0 {
		return e.Messages[0].Data.Text
	}
	return "parse error"
}

// objInfo tracks object literal properties assigned to a variable.
type objInfo struct {
	props   map[string]struct{}
	spreads []string // require() paths spread into this object
}

// funcInfo tracks function bodies for call-mode analysis.
type funcInfo struct {
	body []js_ast.Stmt
}

// walker walks the AST to detect CJS exports.
type walker struct {
	tree      *js_ast.AST
	opts      Options
	exports   map[string]struct{}
	reexports map[string]struct{}

	// Variable tracking maps
	varRequire     map[ast.Ref]string    // ref -> require path
	varExports     map[ast.Ref]struct{}  // refs that alias `exports`
	varModExports  map[ast.Ref]struct{}  // refs that alias `module.exports`
	varObject      map[ast.Ref]*objInfo  // refs -> object literal info
	varFunc        map[ast.Ref]*funcInfo // refs -> function body info
	nodeEnvAliases map[ast.Ref]struct{}  // refs that hold process.env.NODE_ENV

	// When module.exports = something is encountered, prior exports.X assignments
	// are invalidated.
	moduleExportsOverridden bool
}

// analyze runs the full analysis pass.
func (w *walker) analyze() {
	// First pass: collect variable declarations and their initializers.
	for _, part := range w.tree.Parts {
		w.collectVarDecls(part.Stmts)
	}

	// Second pass: walk statements for export patterns.
	for _, part := range w.tree.Parts {
		w.walkStmts(part.Stmts)
	}
}

// collectVarDecls scans for variable declarations to track aliases.
func (w *walker) collectVarDecls(stmts []js_ast.Stmt) {
	for _, stmt := range stmts {
		switch s := stmt.Data.(type) {
		case *js_ast.SLocal:
			for _, decl := range s.Decls {
				w.collectDecl(decl)
			}
		case *js_ast.SBlock:
			w.collectVarDecls(s.Stmts)
		case *js_ast.SIf:
			w.collectVarDeclsFromStmt(s.Yes)
			if s.NoOrNil.Data != nil {
				w.collectVarDeclsFromStmt(s.NoOrNil)
			}
		case *js_ast.SExpr:
			// Handle IIFE: (function(){...})() or (() => {...})()
			w.collectVarDeclsFromExpr(s.Value)
		}
	}
}

// collectVarDeclsFromStmt unwraps a single statement for var decl collection.
func (w *walker) collectVarDeclsFromStmt(stmt js_ast.Stmt) {
	switch s := stmt.Data.(type) {
	case *js_ast.SBlock:
		w.collectVarDecls(s.Stmts)
	case *js_ast.SLocal:
		for _, decl := range s.Decls {
			w.collectDecl(decl)
		}
	case *js_ast.SExpr:
		w.collectVarDeclsFromExpr(s.Value)
	}
}

// collectVarDeclsFromExpr handles IIFE expressions for var decl collection.
func (w *walker) collectVarDeclsFromExpr(expr js_ast.Expr) {
	switch e := expr.Data.(type) {
	case *js_ast.ECall:
		w.collectVarDeclsFromCallTarget(e)
	case *js_ast.EBinary:
		// Handle: expr && (function(){...})(), expr || (function(){...})()
		w.collectVarDeclsFromExpr(e.Left)
		w.collectVarDeclsFromExpr(e.Right)
	case *js_ast.EUnary:
		// Handle: !function(){...}()
		w.collectVarDeclsFromExpr(e.Value)
	}
}

// collectVarDeclsFromCallTarget handles extracting function bodies from IIFE patterns.
func (w *walker) collectVarDeclsFromCallTarget(call *js_ast.ECall) {
	var body []js_ast.Stmt
	switch fn := call.Target.Data.(type) {
	case *js_ast.EFunction:
		body = fn.Fn.Body.Block.Stmts
	case *js_ast.EArrow:
		body = fn.Body.Block.Stmts
	case *js_ast.EDot:
		// Handle: (function(){}).call(this)
		if fn.Name == "call" || fn.Name == "apply" {
			switch inner := fn.Target.Data.(type) {
			case *js_ast.EFunction:
				body = inner.Fn.Body.Block.Stmts
			case *js_ast.EArrow:
				body = inner.Body.Block.Stmts
			}
		}
	}
	if body != nil {
		w.collectVarDecls(body)
	}
	// Recurse into call target (for nested IIFEs like UMD wrappers)
	w.collectVarDeclsFromExpr(call.Target)
	for _, arg := range call.Args {
		w.collectVarDeclsFromExpr(arg)
	}
}

// collectDecl processes a single variable declaration.
func (w *walker) collectDecl(decl js_ast.Decl) {
	if decl.ValueOrNil.Data == nil {
		return
	}

	switch b := decl.Binding.Data.(type) {
	case *js_ast.BIdentifier:
		ref := w.resolveRef(b.Ref)
		val := decl.ValueOrNil

		// var x = require("mod")
		if path, ok := w.extractRequire(val); ok {
			w.varRequire[ref] = path
			return
		}

		// var e = exports
		if w.isExportsRef(val) {
			w.varExports[ref] = struct{}{}
			return
		}

		// var m = module.exports
		if w.isModuleExportsAccess(val) {
			w.varModExports[ref] = struct{}{}
			return
		}

		// var x = module.exports = {}
		if bin, ok := val.Data.(*js_ast.EBinary); ok && bin.Op == js_ast.BinOpAssign {
			if w.isModuleExportsAccess(bin.Left) {
				// Track as module.exports alias so x.foo = ... adds exports
				w.varModExports[ref] = struct{}{}
				if obj, ok := bin.Right.Data.(*js_ast.EObject); ok {
					info := &objInfo{props: make(map[string]struct{})}
					w.extractObjectProps(obj, info)
					w.varObject[ref] = info
				}
			}
		}

		// var o = { ... }
		if obj, ok := val.Data.(*js_ast.EObject); ok {
			info := &objInfo{props: make(map[string]struct{})}
			w.extractObjectProps(obj, info)
			w.varObject[ref] = info
			return
		}

		// var f = function() {} or var f = () => {}
		if fn, ok := val.Data.(*js_ast.EFunction); ok {
			w.varFunc[ref] = &funcInfo{body: fn.Fn.Body.Block.Stmts}
			return
		}
		if fn, ok := val.Data.(*js_ast.EArrow); ok {
			w.varFunc[ref] = &funcInfo{body: fn.Body.Block.Stmts}
			return
		}

		// var x = process.env.NODE_ENV
		if w.isProcessEnvNodeEnv(val) {
			w.nodeEnvAliases[ref] = struct{}{}
			return
		}

	case *js_ast.BObject:
		// const { NODE_ENV } = process.env
		// const { NODE_ENV: alias } = process.env
		if w.isProcessEnv(decl.ValueOrNil) {
			for _, prop := range b.Properties {
				keyName := w.exprToString(prop.Key)
				if keyName == "NODE_ENV" {
					if id, ok := prop.Value.Data.(*js_ast.BIdentifier); ok {
						w.nodeEnvAliases[w.resolveRef(id.Ref)] = struct{}{}
					}
				}
			}
		}
	}
}

// walkStmts processes statements for export patterns.
func (w *walker) walkStmts(stmts []js_ast.Stmt) {
	for _, stmt := range stmts {
		w.walkStmt(stmt)
	}
}

// walkStmt processes a single statement.
func (w *walker) walkStmt(stmt js_ast.Stmt) {
	switch s := stmt.Data.(type) {
	case *js_ast.SExpr:
		w.walkExpr(s.Value)
	case *js_ast.SLocal:
		// Walk declaration values for export patterns
		for _, decl := range s.Decls {
			if decl.ValueOrNil.Data == nil {
				continue
			}
			w.walkExpr(decl.ValueOrNil)
		}
	case *js_ast.SBlock:
		w.walkStmts(s.Stmts)
	case *js_ast.SIf:
		w.walkIfStmt(s)
	case *js_ast.SFunction:
		// function Foo() {} -- track it
		if s.Fn.Body.Block.Stmts != nil {
			w.varFunc[w.resolveRef(s.Fn.Name.Ref)] = &funcInfo{body: s.Fn.Body.Block.Stmts}
		}
	}
}

// walkExpr processes an expression for export patterns.
func (w *walker) walkExpr(expr js_ast.Expr) {
	switch e := expr.Data.(type) {
	case *js_ast.EBinary:
		w.walkBinaryExpr(e)
	case *js_ast.ECall:
		w.walkCallExpr(e)
	case *js_ast.EUnary:
		// Handle: !function(){...}()
		w.walkExpr(e.Value)
	}
}

// walkBinaryExpr processes binary expressions.
func (w *walker) walkBinaryExpr(e *js_ast.EBinary) {
	switch e.Op {
	case js_ast.BinOpAssign:
		w.checkExportAssignment(e.Left, e.Right)
		// Also recurse into RHS for chained assignments and nested patterns
		w.walkExpr(e.Right)

	case js_ast.BinOpLogicalAnd:
		// Pattern: 0 && (module.exports = {...})  -- annotation pattern
		if w.isFalsyLiteral(e.Left) {
			w.walkAnnotationExpr(e.Right)
			return
		}
		// Pattern: "production" !== process.env.NODE_ENV && (function(){...})()
		if w.opts.NodeEnv != "" {
			if w.evaluateNodeEnvCondition(e.Left) {
				w.walkExpr(e.Right)
			}
			return
		}
		w.walkExpr(e.Right)

	case js_ast.BinOpLogicalOr:
		// Pattern: exports.foo || (exports.foo = {})
		w.walkExpr(e.Left)
		w.walkExpr(e.Right)

	case js_ast.BinOpLooseNe, js_ast.BinOpStrictNe:
		// These show up in NODE_ENV checks; handled by parent context.

	case js_ast.BinOpComma:
		w.walkExpr(e.Left)
		w.walkExpr(e.Right)
	}
}

// walkAnnotationExpr handles the RHS of falsy && expr (annotation pattern).
func (w *walker) walkAnnotationExpr(expr js_ast.Expr) {
	switch e := expr.Data.(type) {
	case *js_ast.EBinary:
		if e.Op == js_ast.BinOpAssign {
			if w.isModuleExportsAccess(e.Left) {
				if obj, ok := e.Right.Data.(*js_ast.EObject); ok {
					w.handleModuleExportsObject(obj)
				}
			}
		}
	}
}

// walkCallExpr processes function call expressions.
func (w *walker) walkCallExpr(call *js_ast.ECall) {
	// Object.defineProperty(exports, "name", { ... })
	if w.isObjectDefineProperty(call) {
		w.handleDefineProperty(call)
		return
	}

	// Object.defineProperty(module, "exports", { value: {...} })
	if w.isModuleDefineProperty(call) {
		w.handleModuleDefineProperty(call)
		return
	}

	// Object.assign(module.exports, {...}, ...)
	if w.isObjectAssign(call) && len(call.Args) >= 2 {
		if w.isModuleExportsAccess(call.Args[0]) {
			w.handleObjectAssignToModuleExports(call.Args[1:])
			return
		}
	}

	// Object.assign(module, { exports: {...} })
	if w.isObjectAssign(call) && len(call.Args) >= 2 {
		if w.isModuleRef(call.Args[0]) {
			w.handleObjectAssignToModule(call.Args[1:])
			return
		}
	}

	// __exportStar({...}, exports) or require("tslib").__exportStar({...}, exports)
	// or (0, tslib.__exportStar)({...}, exports) or (0, __exportStar)({...}, exports)
	if w.isExportStarCall(call) {
		w.handleExportStarCall(call)
		return
	}

	// __export({...}) or __export(require("..."))
	if w.isExportCall(call) {
		w.handleExportCall(call)
		return
	}

	// IIFE: (function(){...})() or (() => {...})()
	var body []js_ast.Stmt
	switch fn := call.Target.Data.(type) {
	case *js_ast.EFunction:
		body = fn.Fn.Body.Block.Stmts
	case *js_ast.EArrow:
		body = fn.Body.Block.Stmts
	case *js_ast.EDot:
		// (function(){}).call(this)
		if fn.Name == "call" || fn.Name == "apply" {
			switch inner := fn.Target.Data.(type) {
			case *js_ast.EFunction:
				body = inner.Fn.Body.Block.Stmts
			case *js_ast.EArrow:
				body = inner.Body.Block.Stmts
			}
		}
	}
	if body != nil {
		w.walkStmts(body)
		return
	}

	// Recurse into call target and args for nested patterns
	w.walkExpr(call.Target)
	for _, arg := range call.Args {
		w.walkExpr(arg)
	}
}

// checkExportAssignment checks if an assignment targets exports.
func (w *walker) checkExportAssignment(left js_ast.Expr, right js_ast.Expr) {
	// exports.foo = value
	if name, ok := w.getExportsPropertyName(left); ok {
		if !w.moduleExportsOverridden {
			w.addExport(name)
		}
		return
	}

	// module.exports.foo = value (always add, even after override)
	if name, ok := w.getModuleExportsPropertyName(left); ok {
		w.addExport(name)
		return
	}

	// module.exports = value
	if w.isModuleExportsAccess(left) {
		w.handleModuleExportsAssignment(right)
		return
	}

	// alias.foo = value (where alias is exports or module.exports alias)
	if dot, ok := left.Data.(*js_ast.EDot); ok {
		if id, ok := dot.Target.Data.(*js_ast.EIdentifier); ok {
			ref := w.resolveRef(id.Ref)
			// Check if target is exports alias
			if _, isAlias := w.varExports[ref]; isAlias {
				w.addExport(dot.Name)
				return
			}
			// Check if target is module.exports alias
			if _, isAlias := w.varModExports[ref]; isAlias {
				w.addExport(dot.Name)
				return
			}
			// Check if target is a tracked object variable
			if info, ok := w.varObject[ref]; ok {
				info.props[dot.Name] = struct{}{}
				return
			}
			// Check if target is a require()'d module
			if _, isReq := w.varRequire[ref]; isReq {
				w.addExport(dot.Name)
				return
			}
		}
	}

	// alias["foo"] = value
	if idx, ok := left.Data.(*js_ast.EIndex); ok {
		if name := w.exprToString(idx.Index); name != "" {
			if id, ok := idx.Target.Data.(*js_ast.EIdentifier); ok {
				ref := w.resolveRef(id.Ref)
				if _, isAlias := w.varExports[ref]; isAlias {
					w.addExport(name)
					return
				}
				if _, isAlias := w.varModExports[ref]; isAlias {
					w.addExport(name)
					return
				}
			}
		}
	}
}

// handleModuleExportsAssignment processes module.exports = <value>.
func (w *walker) handleModuleExportsAssignment(value js_ast.Expr) {
	w.moduleExportsOverridden = true
	w.exports = make(map[string]struct{})
	w.reexports = make(map[string]struct{})

	switch v := value.Data.(type) {
	case *js_ast.EObject:
		w.handleModuleExportsObject(v)

	case *js_ast.ECall:
		// module.exports = require("lib")
		if path, ok := w.extractRequire(js_ast.Expr{Data: v}); ok {
			w.addReexport(path)
			return
		}
		// module.exports = require("lib")()
		if path := w.extractRequireCall(v); path != "" {
			w.addReexport(path + "()")
			return
		}
		// module.exports = fn()
		if id, ok := v.Target.Data.(*js_ast.EIdentifier); ok {
			ref := w.resolveRef(id.Ref)
			if fi, ok := w.varFunc[ref]; ok {
				w.analyzeFuncBody(fi.body)
				return
			}
		}
		// module.exports = someFunc()
		w.walkCallExpr(v)

	case *js_ast.EIdentifier:
		// module.exports = someVar
		ref := w.resolveRef(v.Ref)
		// module.exports = require("lib") variable
		if path, ok := w.varRequire[ref]; ok {
			w.addReexport(path)
			// Also check if this variable had property assignments
			if info, ok := w.varObject[ref]; ok {
				for name := range info.props {
					w.addExport(name)
				}
			}
			// Check for direct property assignments on the require variable
			w.collectExportsFromVarProps(ref)
			return
		}
		// module.exports = obj variable
		if info, ok := w.varObject[ref]; ok {
			for name := range info.props {
				w.addExport(name)
			}
			for _, path := range info.spreads {
				w.addReexport(path)
			}
			return
		}
		// module.exports = funcVar (in call mode, analyze func body)
		if fi, ok := w.varFunc[ref]; ok {
			if w.opts.CallMode {
				w.analyzeFuncBody(fi.body)
			} else {
				// Even without call mode, check for static properties on the function
				w.collectExportsFromVarProps(ref)
			}
			return
		}

	case *js_ast.EFunction:
		// module.exports = function() { ... }
		if w.opts.CallMode {
			w.analyzeFuncBody(v.Fn.Body.Block.Stmts)
		}

	case *js_ast.EArrow:
		// module.exports = () => { ... }
		if w.opts.CallMode {
			w.analyzeFuncBody(v.Body.Block.Stmts)
		}
	}
}

// collectExportsFromVarProps scans all parts for property assignments on a ref.
func (w *walker) collectExportsFromVarProps(ref ast.Ref) {
	for _, part := range w.tree.Parts {
		for _, stmt := range part.Stmts {
			if s, ok := stmt.Data.(*js_ast.SExpr); ok {
				if bin, ok := s.Value.Data.(*js_ast.EBinary); ok && bin.Op == js_ast.BinOpAssign {
					if dot, ok := bin.Left.Data.(*js_ast.EDot); ok {
						if id, ok := dot.Target.Data.(*js_ast.EIdentifier); ok && w.refsEqual(id.Ref, ref) {
							w.addExport(dot.Name)
						}
					}
				}
			}
		}
	}
}

// handleModuleExportsObject extracts exports from module.exports = { ... }.
func (w *walker) handleModuleExportsObject(obj *js_ast.EObject) {
	for _, prop := range obj.Properties {
		if prop.Kind == js_ast.PropertySpread {
			// ...require("mod") or ...obj
			w.handleSpreadProp(prop)
			continue
		}
		name := w.exprToString(prop.Key)
		if name != "" {
			w.addExport(name)
		}
	}
}

// handleSpreadProp handles spread properties in object literals.
func (w *walker) handleSpreadProp(prop js_ast.Property) {
	spread := prop.ValueOrNil
	if spread.Data == nil {
		// Try Key for spread-only properties
		spread = prop.Key
	}
	if path, ok := w.extractRequire(spread); ok {
		w.addReexport(path)
		return
	}
	// ...obj -> look up variable
	if id, ok := spread.Data.(*js_ast.EIdentifier); ok {
		ref := w.resolveRef(id.Ref)
		if info, ok := w.varObject[ref]; ok {
			for name := range info.props {
				w.addExport(name)
			}
			for _, path := range info.spreads {
				w.addReexport(path)
			}
		}
	}
}

// handleDefineProperty handles Object.defineProperty(exports, "name", { ... }).
func (w *walker) handleDefineProperty(call *js_ast.ECall) {
	if len(call.Args) < 2 {
		return
	}

	target := call.Args[0]
	nameExpr := call.Args[1]

	// Allow (0, exports) as target
	target = w.unwrapCommaExpr(target)

	isExports := w.isExportsRef(target) || w.isModuleExportsAccess(target)
	if !isExports {
		return
	}

	name := w.exprToString(nameExpr)
	if name == "" {
		return
	}

	// Check if descriptor has "value" or "get" property (skip if only has non-value properties like {})
	if len(call.Args) >= 3 {
		if obj, ok := call.Args[2].Data.(*js_ast.EObject); ok {
			hasValueOrGet := false
			for _, prop := range obj.Properties {
				key := w.exprToString(prop.Key)
				if key == "value" || key == "get" {
					hasValueOrGet = true
					break
				}
			}
			if !hasValueOrGet {
				return
			}
		}
	}

	w.addExport(name)
}

// handleModuleDefineProperty handles Object.defineProperty(module, "exports", { value: {...} }).
func (w *walker) handleModuleDefineProperty(call *js_ast.ECall) {
	if len(call.Args) < 3 {
		return
	}

	nameExpr := call.Args[1]
	name := w.exprToString(nameExpr)
	if name != "exports" {
		return
	}

	// Extract value from descriptor
	desc := call.Args[2]
	if obj, ok := desc.Data.(*js_ast.EObject); ok {
		for _, prop := range obj.Properties {
			key := w.exprToString(prop.Key)
			if key == "value" {
				if innerObj, ok := prop.ValueOrNil.Data.(*js_ast.EObject); ok {
					// Reset exports since this replaces module.exports
					w.moduleExportsOverridden = true
					w.exports = make(map[string]struct{})
					w.reexports = make(map[string]struct{})
					w.handleModuleExportsObject(innerObj)
				}
				return
			}
		}
	}
}

// handleObjectAssignToModuleExports handles Object.assign(module.exports, {...}, ...).
func (w *walker) handleObjectAssignToModuleExports(args []js_ast.Expr) {
	for _, arg := range args {
		switch v := arg.Data.(type) {
		case *js_ast.EObject:
			for _, prop := range v.Properties {
				if prop.Kind == js_ast.PropertySpread {
					w.handleSpreadProp(prop)
					continue
				}
				name := w.exprToString(prop.Key)
				if name != "" {
					w.addExport(name)
				}
			}
		case *js_ast.ECall:
			if path, ok := w.extractRequire(arg); ok {
				w.addReexport(path)
			}
		case *js_ast.EIdentifier:
			ref := w.resolveRef(v.Ref)
			if path, ok := w.varRequire[ref]; ok {
				w.addReexport(path)
			}
		}
	}
}

// handleObjectAssignToModule handles Object.assign(module, { exports: {...} }).
func (w *walker) handleObjectAssignToModule(args []js_ast.Expr) {
	for _, arg := range args {
		if obj, ok := arg.Data.(*js_ast.EObject); ok {
			for _, prop := range obj.Properties {
				name := w.exprToString(prop.Key)
				if name == "exports" {
					// module.exports is being replaced
					w.moduleExportsOverridden = true
					w.exports = make(map[string]struct{})
					w.reexports = make(map[string]struct{})
					if innerObj, ok := prop.ValueOrNil.Data.(*js_ast.EObject); ok {
						w.handleModuleExportsObject(innerObj)
					}
					return
				}
			}
		}
	}
}

// isExportStarCall checks for __exportStar(..., exports) patterns.
func (w *walker) isExportStarCall(call *js_ast.ECall) bool {
	if len(call.Args) != 2 {
		return false
	}
	// Second arg must be exports
	if !w.isExportsRef(call.Args[1]) {
		return false
	}

	// Direct: __exportStar(...)
	if id, ok := call.Target.Data.(*js_ast.EIdentifier); ok {
		name := w.symbolName(id.Ref)
		if name == "__exportStar" {
			return true
		}
	}

	// require("tslib").__exportStar(...)
	if dot, ok := call.Target.Data.(*js_ast.EDot); ok && dot.Name == "__exportStar" {
		return true
	}

	// (0, tslib.__exportStar)(...) or (0, __exportStar)(...)
	if target := w.unwrapCommaExpr(call.Target); target.Data != call.Target.Data {
		if dot, ok := target.Data.(*js_ast.EDot); ok && dot.Name == "__exportStar" {
			return true
		}
		if id, ok := target.Data.(*js_ast.EIdentifier); ok {
			name := w.symbolName(id.Ref)
			if name == "__exportStar" {
				return true
			}
		}
	}

	return false
}

// handleExportStarCall processes __exportStar({...}, exports) or __exportStar(require("..."), exports).
func (w *walker) handleExportStarCall(call *js_ast.ECall) {
	if len(call.Args) < 2 {
		return
	}
	first := call.Args[0]
	// __exportStar({foo: ...}, exports)
	if obj, ok := first.Data.(*js_ast.EObject); ok {
		for _, prop := range obj.Properties {
			name := w.exprToString(prop.Key)
			if name != "" {
				w.addExport(name)
			}
		}
		return
	}
	// __exportStar(require("./path"), exports)
	if path, ok := w.extractRequire(first); ok {
		w.addReexport(path)
	}
}

// isExportCall checks for __export({...}) pattern (esbuild/TypeScript output).
func (w *walker) isExportCall(call *js_ast.ECall) bool {
	if len(call.Args) != 1 {
		return false
	}
	if id, ok := call.Target.Data.(*js_ast.EIdentifier); ok {
		name := w.symbolName(id.Ref)
		return name == "__export"
	}
	return false
}

// handleExportCall processes __export({...}) or __export(require("...")).
func (w *walker) handleExportCall(call *js_ast.ECall) {
	if len(call.Args) < 1 {
		return
	}
	first := call.Args[0]
	if obj, ok := first.Data.(*js_ast.EObject); ok {
		for _, prop := range obj.Properties {
			name := w.exprToString(prop.Key)
			if name != "" {
				w.addExport(name)
			}
		}
		return
	}
	if path, ok := w.extractRequire(first); ok {
		w.addReexport(path)
	}
}

// walkIfStmt processes if statements with NODE_ENV-aware evaluation.
func (w *walker) walkIfStmt(s *js_ast.SIf) {
	if w.opts.NodeEnv != "" {
		result := w.evaluateCondition(s.Test)
		switch result {
		case condTrue:
			w.walkStmtBody(s.Yes)
			return
		case condFalse:
			if s.NoOrNil.Data != nil {
				w.walkStmtBody(s.NoOrNil)
			}
			return
		}
	}

	// If we can't evaluate the condition, walk both branches.
	w.walkStmtBody(s.Yes)
	if s.NoOrNil.Data != nil {
		w.walkStmtBody(s.NoOrNil)
	}
}

// walkStmtBody unwraps a statement body (which might be a block or single statement).
func (w *walker) walkStmtBody(stmt js_ast.Stmt) {
	switch s := stmt.Data.(type) {
	case *js_ast.SBlock:
		w.walkStmts(s.Stmts)
	default:
		w.walkStmt(stmt)
	}
}

type condResult int

const (
	condUnknown condResult = iota
	condTrue
	condFalse
)

// evaluateCondition evaluates a condition expression, handling NODE_ENV checks.
func (w *walker) evaluateCondition(expr js_ast.Expr) condResult {
	switch e := expr.Data.(type) {
	case *js_ast.EBinary:
		return w.evaluateConditionBinary(e)
	case *js_ast.EUnary:
		if e.Op == js_ast.UnOpNot {
			inner := w.evaluateCondition(e.Value)
			switch inner {
			case condTrue:
				return condFalse
			case condFalse:
				return condTrue
			}
		}
	case *js_ast.EBoolean:
		if e.Value {
			return condTrue
		}
		return condFalse
	}
	return condUnknown
}

// evaluateConditionBinary evaluates binary condition expressions.
func (w *walker) evaluateConditionBinary(e *js_ast.EBinary) condResult {
	switch e.Op {
	case js_ast.BinOpLooseEq, js_ast.BinOpStrictEq:
		return w.evaluateEqualityCheck(e.Left, e.Right, true)
	case js_ast.BinOpLooseNe, js_ast.BinOpStrictNe:
		return w.evaluateEqualityCheck(e.Left, e.Right, false)
	case js_ast.BinOpLogicalAnd:
		left := w.evaluateCondition(e.Left)
		if left == condFalse {
			return condFalse
		}
		right := w.evaluateCondition(e.Right)
		if left == condTrue {
			return right
		}
		// typeof module !== 'undefined' -- assume true in CJS context
		return condUnknown
	case js_ast.BinOpLogicalOr:
		left := w.evaluateCondition(e.Left)
		if left == condTrue {
			return condTrue
		}
		right := w.evaluateCondition(e.Right)
		if left == condFalse {
			return right
		}
		return condUnknown
	}
	return condUnknown
}

// evaluateNodeEnvCondition evaluates a NODE_ENV comparison (returns true if condition evaluates to true).
func (w *walker) evaluateNodeEnvCondition(expr js_ast.Expr) bool {
	return w.evaluateCondition(expr) == condTrue
}

// evaluateEqualityCheck evaluates an equality or inequality check.
func (w *walker) evaluateEqualityCheck(left, right js_ast.Expr, isEquals bool) condResult {
	// Try both orderings
	if result := w.evaluateEqualityOnce(left, right, isEquals); result != condUnknown {
		return result
	}
	return w.evaluateEqualityOnce(right, left, isEquals)
}

// evaluateEqualityOnce attempts to evaluate left <op> right.
func (w *walker) evaluateEqualityOnce(left, right js_ast.Expr, isEquals bool) condResult {
	// Check if left is a NODE_ENV reference
	nodeEnvValue := ""
	if w.isProcessEnvNodeEnv(left) {
		nodeEnvValue = w.opts.NodeEnv
	}
	if id, ok := left.Data.(*js_ast.EIdentifier); ok {
		if _, isAlias := w.nodeEnvAliases[w.resolveRef(id.Ref)]; isAlias {
			nodeEnvValue = w.opts.NodeEnv
		}
	}
	if nodeEnvValue == "" {
		// typeof module !== "undefined" -> always true in CJS
		if w.isTypeofCheck(left, right, "module", "undefined") {
			// typeof module !== "undefined" => true, typeof module === "undefined" => false
			if isEquals {
				return condFalse
			}
			return condTrue
		}
		if w.isTypeofCheck(left, right, "exports", "undefined") {
			if isEquals {
				return condFalse
			}
			return condTrue
		}
		return condUnknown
	}

	// Right must be a string literal
	rightStr := w.exprToString(right)
	if rightStr == "" {
		return condUnknown
	}

	match := nodeEnvValue == rightStr
	if isEquals {
		if match {
			return condTrue
		}
		return condFalse
	}
	if match {
		return condFalse
	}
	return condTrue
}

// isTypeofCheck checks for typeof X <op> "string" pattern.
func (w *walker) isTypeofCheck(left, right js_ast.Expr, identName, strValue string) bool {
	unary, ok := left.Data.(*js_ast.EUnary)
	if !ok || unary.Op != js_ast.UnOpTypeof {
		return false
	}
	// typeof X where X is the identifier
	if id, ok := unary.Value.Data.(*js_ast.EIdentifier); ok {
		name := w.symbolName(id.Ref)
		if name != identName {
			return false
		}
	} else {
		return false
	}
	rightStr := w.exprToString(right)
	return rightStr == strValue
}

// analyzeFuncBody analyzes a function body for return statements to extract exports.
func (w *walker) analyzeFuncBody(stmts []js_ast.Stmt) {
	for _, stmt := range stmts {
		w.analyzeFuncStmt(stmt)
	}
}

// analyzeFuncStmt analyzes a statement inside a function body.
func (w *walker) analyzeFuncStmt(stmt js_ast.Stmt) {
	switch s := stmt.Data.(type) {
	case *js_ast.SReturn:
		if s.ValueOrNil.Data != nil {
			w.analyzeReturnValue(s.ValueOrNil)
		}
	case *js_ast.SLocal:
		for _, decl := range s.Decls {
			w.collectDecl(decl)
		}
	case *js_ast.SExpr:
		// Handle property assignments inside the function
		if bin, ok := s.Value.Data.(*js_ast.EBinary); ok && bin.Op == js_ast.BinOpAssign {
			// Check for mod.foo = value where mod is a tracked object
			if dot, ok := bin.Left.Data.(*js_ast.EDot); ok {
				if id, ok := dot.Target.Data.(*js_ast.EIdentifier); ok {
					ref := w.resolveRef(id.Ref)
					if info, ok := w.varObject[ref]; ok {
						info.props[dot.Name] = struct{}{}
					}
				}
			}
		}
	case *js_ast.SIf:
		// Handle conditional returns in function body
		if w.opts.NodeEnv != "" {
			result := w.evaluateCondition(s.Test)
			switch result {
			case condTrue:
				w.analyzeFuncStmtBody(s.Yes)
				return
			case condFalse:
				if s.NoOrNil.Data != nil {
					w.analyzeFuncStmtBody(s.NoOrNil)
				}
				return
			}
		}
		w.analyzeFuncStmtBody(s.Yes)
		if s.NoOrNil.Data != nil {
			w.analyzeFuncStmtBody(s.NoOrNil)
		}
	case *js_ast.SBlock:
		w.analyzeFuncBody(s.Stmts)
	}
}

// analyzeFuncStmtBody unwraps a statement for function analysis.
func (w *walker) analyzeFuncStmtBody(stmt js_ast.Stmt) {
	switch s := stmt.Data.(type) {
	case *js_ast.SBlock:
		w.analyzeFuncBody(s.Stmts)
	default:
		w.analyzeFuncStmt(stmt)
	}
}

// analyzeReturnValue extracts exports from a return value.
func (w *walker) analyzeReturnValue(expr js_ast.Expr) {
	switch v := expr.Data.(type) {
	case *js_ast.EObject:
		for _, prop := range v.Properties {
			if prop.Kind == js_ast.PropertySpread {
				continue
			}
			name := w.exprToString(prop.Key)
			if name != "" {
				w.addExport(name)
			}
		}
	case *js_ast.EIdentifier:
		ref := w.resolveRef(v.Ref)
		if info, ok := w.varObject[ref]; ok {
			for name := range info.props {
				w.addExport(name)
			}
		}
	}
}

// --- Helper methods ---

// isExportsRef checks if an expression is a reference to the `exports` symbol.
func (w *walker) isExportsRef(expr js_ast.Expr) bool {
	if id, ok := expr.Data.(*js_ast.EIdentifier); ok {
		return w.symbolName(id.Ref) == "exports"
	}
	return false
}

// isModuleRef checks if an expression is a reference to the `module` symbol.
func (w *walker) isModuleRef(expr js_ast.Expr) bool {
	if id, ok := expr.Data.(*js_ast.EIdentifier); ok {
		return w.symbolName(id.Ref) == "module"
	}
	return false
}

// isModuleExportsAccess checks for module.exports or module["exports"].
func (w *walker) isModuleExportsAccess(expr js_ast.Expr) bool {
	if dot, ok := expr.Data.(*js_ast.EDot); ok {
		if dot.Name == "exports" {
			return w.isModuleRef(dot.Target)
		}
	}
	if idx, ok := expr.Data.(*js_ast.EIndex); ok {
		if w.exprToString(idx.Index) == "exports" {
			return w.isModuleRef(idx.Target)
		}
	}
	return false
}

// getExportsPropertyName returns the property name if expr is exports.X or exports["X"].
func (w *walker) getExportsPropertyName(expr js_ast.Expr) (string, bool) {
	if dot, ok := expr.Data.(*js_ast.EDot); ok {
		if w.isExportsRef(dot.Target) {
			return dot.Name, true
		}
	}
	if idx, ok := expr.Data.(*js_ast.EIndex); ok {
		if w.isExportsRef(idx.Target) {
			name := w.exprToString(idx.Index)
			if name != "" {
				return name, true
			}
		}
	}
	return "", false
}

// getModuleExportsPropertyName returns the property name if expr is module.exports.X or module["exports"]["X"].
func (w *walker) getModuleExportsPropertyName(expr js_ast.Expr) (string, bool) {
	if dot, ok := expr.Data.(*js_ast.EDot); ok {
		if w.isModuleExportsAccess(dot.Target) {
			return dot.Name, true
		}
	}
	if idx, ok := expr.Data.(*js_ast.EIndex); ok {
		if w.isModuleExportsAccess(idx.Target) {
			name := w.exprToString(idx.Index)
			if name != "" {
				return name, true
			}
		}
	}
	return "", false
}

// extractRequire extracts the module path from a require("...") call expression.
func (w *walker) extractRequire(expr js_ast.Expr) (string, bool) {
	call, ok := expr.Data.(*js_ast.ECall)
	if !ok {
		return "", false
	}
	if len(call.Args) != 1 {
		return "", false
	}
	if id, ok := call.Target.Data.(*js_ast.EIdentifier); ok {
		name := w.symbolName(id.Ref)
		if name == "require" {
			path := w.exprToString(call.Args[0])
			if path != "" {
				return path, true
			}
		}
	}
	return "", false
}

// extractRequireCall extracts the module path from require("...")() (function call on require result).
func (w *walker) extractRequireCall(call *js_ast.ECall) string {
	if innerCall, ok := call.Target.Data.(*js_ast.ECall); ok {
		if path, ok := w.extractRequire(js_ast.Expr{Data: innerCall}); ok {
			return path
		}
	}
	return ""
}

// isObjectDefineProperty checks for Object.defineProperty(exports, ...) or Object.defineProperty((0, exports), ...).
func (w *walker) isObjectDefineProperty(call *js_ast.ECall) bool {
	if len(call.Args) < 2 {
		return false
	}
	dot, ok := call.Target.Data.(*js_ast.EDot)
	if !ok || dot.Name != "defineProperty" {
		return false
	}
	if id, ok := dot.Target.Data.(*js_ast.EIdentifier); ok {
		name := w.symbolName(id.Ref)
		if name != "Object" {
			return false
		}
	} else {
		return false
	}

	target := w.unwrapCommaExpr(call.Args[0])
	return w.isExportsRef(target) || w.isModuleExportsAccess(target)
}

// isModuleDefineProperty checks for Object.defineProperty(module, "exports", ...).
func (w *walker) isModuleDefineProperty(call *js_ast.ECall) bool {
	if len(call.Args) < 3 {
		return false
	}
	dot, ok := call.Target.Data.(*js_ast.EDot)
	if !ok || dot.Name != "defineProperty" {
		return false
	}
	if id, ok := dot.Target.Data.(*js_ast.EIdentifier); ok {
		name := w.symbolName(id.Ref)
		if name != "Object" {
			return false
		}
	} else {
		return false
	}

	return w.isModuleRef(call.Args[0])
}

// isObjectAssign checks for Object.assign(...).
func (w *walker) isObjectAssign(call *js_ast.ECall) bool {
	dot, ok := call.Target.Data.(*js_ast.EDot)
	if !ok || dot.Name != "assign" {
		return false
	}
	if id, ok := dot.Target.Data.(*js_ast.EIdentifier); ok {
		name := w.symbolName(id.Ref)
		return name == "Object"
	}
	return false
}

// isProcessEnvNodeEnv checks if an expression is process.env.NODE_ENV.
func (w *walker) isProcessEnvNodeEnv(expr js_ast.Expr) bool {
	dot, ok := expr.Data.(*js_ast.EDot)
	if !ok || dot.Name != "NODE_ENV" {
		return false
	}
	return w.isProcessEnv(dot.Target)
}

// isProcessEnv checks if an expression is process.env.
func (w *walker) isProcessEnv(expr js_ast.Expr) bool {
	dot, ok := expr.Data.(*js_ast.EDot)
	if !ok || dot.Name != "env" {
		return false
	}
	if id, ok := dot.Target.Data.(*js_ast.EIdentifier); ok {
		return w.symbolName(id.Ref) == "process"
	}
	return false
}

// isFalsyLiteral checks if an expression is a falsy literal (0, false, null, undefined, "").
func (w *walker) isFalsyLiteral(expr js_ast.Expr) bool {
	switch e := expr.Data.(type) {
	case *js_ast.ENumber:
		return e.Value == 0
	case *js_ast.EBoolean:
		return !e.Value
	case *js_ast.ENull:
		return true
	case *js_ast.EUndefined:
		return true
	case *js_ast.EString:
		return len(e.Value) == 0
	}
	return false
}

// unwrapCommaExpr unwraps (0, expr) -> expr.
func (w *walker) unwrapCommaExpr(expr js_ast.Expr) js_ast.Expr {
	if bin, ok := expr.Data.(*js_ast.EBinary); ok && bin.Op == js_ast.BinOpComma {
		return bin.Right
	}
	return expr
}

// exprToString extracts a string value from a string literal expression.
func (w *walker) exprToString(expr js_ast.Expr) string {
	switch e := expr.Data.(type) {
	case *js_ast.EString:
		return helpers.UTF16ToString(e.Value)
	case *js_ast.EIdentifier:
		// For shorthand properties like { foo } the key is an identifier
		return w.symbolName(e.Ref)
	}
	return ""
}

// resolveRef follows symbol links to get the canonical ref.
func (w *walker) resolveRef(ref ast.Ref) ast.Ref {
	for {
		if int(ref.InnerIndex) >= len(w.tree.Symbols) {
			return ref
		}
		link := w.tree.Symbols[ref.InnerIndex].Link
		if link == ast.InvalidRef {
			return ref
		}
		ref = link
	}
}

// refsEqual compares two refs after resolving symbol links.
func (w *walker) refsEqual(a, b ast.Ref) bool {
	return w.resolveRef(a) == w.resolveRef(b)
}

// symbolName returns the original name of a symbol by ref.
func (w *walker) symbolName(ref ast.Ref) string {
	ref = w.resolveRef(ref)
	if int(ref.InnerIndex) < len(w.tree.Symbols) {
		return w.tree.Symbols[ref.InnerIndex].OriginalName
	}
	return ""
}

// extractObjectProps extracts property names from an object literal into an objInfo.
func (w *walker) extractObjectProps(obj *js_ast.EObject, info *objInfo) {
	for _, prop := range obj.Properties {
		if prop.Kind == js_ast.PropertySpread {
			// Handle ...require("mod")
			spread := prop.ValueOrNil
			if spread.Data == nil {
				spread = prop.Key
			}
			if path, ok := w.extractRequire(spread); ok {
				info.spreads = append(info.spreads, path)
				continue
			}
			// Handle ...otherObj
			if id, ok := spread.Data.(*js_ast.EIdentifier); ok {
				ref := w.resolveRef(id.Ref)
				if other, ok := w.varObject[ref]; ok {
					for name := range other.props {
						info.props[name] = struct{}{}
					}
					info.spreads = append(info.spreads, other.spreads...)
				}
			}
			continue
		}
		name := w.exprToString(prop.Key)
		if name != "" {
			info.props[name] = struct{}{}
		}
	}
}

// annotationRe matches the annotation pattern: 0 && (module.exports = {...})
var annotationRe = regexp.MustCompile(`(?:^|[;\s])0\s*&&\s*\(\s*module\.exports\s*=\s*\{([^}]*)\}`)

// scanAnnotationPattern scans the raw source for the 0 && (module.exports = {...}) pattern.
// esbuild's parser constant-folds this away, so we detect it via text matching.
func (w *walker) scanAnnotationPattern(source, filename string) {
	matches := annotationRe.FindAllStringSubmatch(source, -1)
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		body := match[1]
		// Parse the property names from the object literal body
		// Handle: foo, bar, baz or "foo": val, "bar": val
		for _, part := range strings.Split(body, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			// Handle "key": value or key: value or shorthand key
			colonIdx := strings.Index(part, ":")
			if colonIdx >= 0 {
				part = strings.TrimSpace(part[:colonIdx])
			}
			// Remove quotes if present
			part = strings.Trim(part, "\"'` ")
			if part != "" {
				w.addExport(part)
			}
		}
	}
}

// addExport adds an export name.
func (w *walker) addExport(name string) {
	w.exports[name] = struct{}{}
}

// addReexport adds a reexport path.
func (w *walker) addReexport(path string) {
	w.reexports[path] = struct{}{}
}

// sortedExports returns exports in insertion order (approximated by sorted order).
func (w *walker) sortedExports() []string {
	if len(w.exports) == 0 {
		return nil
	}
	result := make([]string, 0, len(w.exports))
	for name := range w.exports {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// sortedReexports returns reexports in sorted order.
func (w *walker) sortedReexports() []string {
	if len(w.reexports) == 0 {
		return nil
	}
	result := make([]string, 0, len(w.reexports))
	for path := range w.reexports {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}
