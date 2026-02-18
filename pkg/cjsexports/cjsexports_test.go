package cjsexports

import (
	"sort"
	"strings"
	"testing"
)

// helper to parse and return sorted exports and reexports.
func parseTest(t *testing.T, source string, opts Options) (exports, reexports []string) {
	t.Helper()
	result, err := Parse(source, "index.cjs", opts)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	return result.Exports, result.Reexports
}

func assertExports(t *testing.T, got []string, want string) {
	t.Helper()
	gotStr := strings.Join(got, ",")
	if gotStr != want {
		t.Errorf("exports: got %q, want %q", gotStr, want)
	}
}

func assertReexports(t *testing.T, got []string, want string) {
	t.Helper()
	gotStr := strings.Join(got, ",")
	if gotStr != want {
		t.Errorf("reexports: got %q, want %q", gotStr, want)
	}
}

// assertExportsUnordered compares exports ignoring order.
func assertExportsUnordered(t *testing.T, got []string, want string) {
	t.Helper()
	wantParts := strings.Split(want, ",")
	if want == "" {
		wantParts = nil
	}
	sort.Strings(wantParts)
	gotSorted := make([]string, len(got))
	copy(gotSorted, got)
	sort.Strings(gotSorted)
	gotStr := strings.Join(gotSorted, ",")
	wantStr := strings.Join(wantParts, ",")
	if gotStr != wantStr {
		t.Errorf("exports (unordered): got %q, want %q", gotStr, wantStr)
	}
}

// assertReexportsUnordered compares reexports ignoring order.
func assertReexportsUnordered(t *testing.T, got []string, want string) {
	t.Helper()
	wantParts := strings.Split(want, ",")
	if want == "" {
		wantParts = nil
	}
	sort.Strings(wantParts)
	gotSorted := make([]string, len(got))
	copy(gotSorted, got)
	sort.Strings(gotSorted)
	gotStr := strings.Join(gotSorted, ",")
	wantStr := strings.Join(wantParts, ",")
	if gotStr != wantStr {
		t.Errorf("reexports (unordered): got %q, want %q", gotStr, wantStr)
	}
}

// --- Test Case 1: Object.defineProperty ---
func TestDefineProperty(t *testing.T) {
	source := `
		const c = 'c'
		Object.defineProperty(exports, 'a', { value: 1 });
		Object.defineProperty(exports, 'b', { get: () => 1 });
		Object.defineProperty(exports, c, { get() { return 1 } });
		Object.defineProperty(exports, 'd', { "value": 1 });
		Object.defineProperty(exports, 'e', { "get": () => 1 });
		Object.defineProperty(exports, 'f', {});
		Object.defineProperty((0, exports), 'g', { value: 1 });
		Object.defineProperty(module.exports, '__esModule', { value: 1 });
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	// 'f' should not be included (empty descriptor)
	// 'c' is included because exprToString resolves the identifier name
	assertExportsUnordered(t, exports, "__esModule,a,b,c,d,e,g")
}

// --- Test Case 2: Object.defineProperty(module, "exports", ...) ---
func TestDefinePropertyModuleExports(t *testing.T) {
	source := `
		const alas = true
		const obj = { bar: 123 }
		Object.defineProperty(exports, 'ew', { value: 1 })
		Object.defineProperty(module, 'exports', { value: { alas, foo: 'bar', ...obj, ...require('a'), ...require('b') } })
	`
	exports, reexports := parseTest(t, source, Options{NodeEnv: "development"})
	assertExportsUnordered(t, exports, "alas,foo,bar")
	assertReexportsUnordered(t, reexports, "a,b")
}

// --- Test Case 5: Basic exports.foo and module.exports.bar ---
func TestBasicExports(t *testing.T) {
	source := `
		exports.foo = 'bar'
		module.exports.bar = 123
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExportsUnordered(t, exports, "bar,foo")
}

// --- Test Case 6: module.exports = {...} overrides prior exports ---
func TestModuleExportsOverrides(t *testing.T) {
	source := `
		const alas = true
		const obj = { boom: 1 }
		obj.coco = 1
		exports.foo = 'bar'
		module.exports.bar = 123
		module.exports = { alas, ...obj, ...require('a'), ...require('b') }
	`
	exports, reexports := parseTest(t, source, Options{NodeEnv: "development"})
	assertExportsUnordered(t, exports, "alas,boom,coco")
	assertReexportsUnordered(t, reexports, "a,b")
}

// --- Test Case 7: Bracket notation ---
func TestBracketNotation(t *testing.T) {
	source := `
		exports['foo'] = 'bar'
		module['exports']['bar'] = 123
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExportsUnordered(t, exports, "bar,foo")
}

// --- Test Case 8: module.exports = function, then module.exports.foo ---
func TestModuleExportsFunctionThenProp(t *testing.T) {
	source := `
		module.exports = function() {}
		module.exports.foo = 'bar';
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "foo")
}

// --- Test Case 9: module.exports = require ---
func TestModuleExportsRequire(t *testing.T) {
	source := `
		module.exports = require("lib")
	`
	_, reexports := parseTest(t, source, Options{NodeEnv: "development"})
	assertReexports(t, reexports, "lib")
}

// --- Test Case 9.1: var lib = require("lib"); module.exports = lib ---
func TestModuleExportsRequireVar(t *testing.T) {
	source := `
		var lib = require("lib")
		module.exports = lib
	`
	_, reexports := parseTest(t, source, Options{NodeEnv: "development"})
	assertReexports(t, reexports, "lib")
}

// --- Test Case 10: function with static props assigned, then module.exports = func ---
func TestFunctionStaticProps(t *testing.T) {
	source := `
		function Module() {}
		Module.foo = 'bar'
		module.exports = Module
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "foo")
}

// --- Test Case 10.3: var lib = require(...); lib.foo = ...; module.exports = lib ---
func TestRequireVarWithProps(t *testing.T) {
	source := `
		var lib = require("lib")
		lib.foo = 'bar'
		module.exports = lib
	`
	exports, reexports := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "foo")
	assertReexports(t, reexports, "lib")
}

// --- Test Case 10.4: var e = exports; e.foo = 'bar' ---
func TestExportsAlias(t *testing.T) {
	source := `
		var e = exports
		e.foo = 'bar'
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "foo")
}

// --- Test Case 10.5: var mod = module.exports; mod.foo = 'bar' ---
func TestModuleExportsAlias(t *testing.T) {
	source := `
		var mod = module.exports
		mod.foo = 'bar'
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "foo")
}

// --- Test Case 12: IIFE with module.exports ---
func TestIIFE(t *testing.T) {
	source := `
		(function() {
			module.exports = { foo: 'bar' }
		})()
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "foo")
}

// --- Test Case 12.1: Arrow IIFE ---
func TestArrowIIFE(t *testing.T) {
	source := `
		(() => {
			module.exports = { foo: 'bar' }
		})()
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "foo")
}

// --- Test Case 12.3: ~function IIFE ---
func TestTildeIIFE(t *testing.T) {
	source := `
		~function() {
			module.exports = { foo: 'bar' }
		}()
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "foo")
}

// --- Test Case 12.4: !function IIFE ---
func TestBangIIFE(t *testing.T) {
	source := `
		!function() {
			module.exports = { foo: 'bar' }
		}()
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "foo")
}

// --- Test Case 12.5: .call(this) IIFE ---
func TestCallIIFE(t *testing.T) {
	source := `
		(function() {
			if (typeof exports !== 'undefined') {
				exports = module.exports = {
					foo: 'bar'
				};
			}
		}).call(this)
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "foo")
}

// --- Test Case 12.7: variable assigned, then used in IIFE ---
func TestVarInIIFE(t *testing.T) {
	source := `
		let es = { foo: 'bar' };
		(function() {
			module.exports = es
		})()
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "foo")
}

// --- Test Case 13: Block scope ---
func TestBlockScope(t *testing.T) {
	source := `
		{
			module.exports = { foo: 'bar' }
		}
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "foo")
}

// --- Test Case 13.1: Block scope with spread objects ---
func TestBlockScopeSpreadObjects(t *testing.T) {
	source := `
		const obj1 = { foo: 'bar' }
		{
			const obj2 = { bar: 123 }
			module.exports = { ...obj1, ...obj2 }
		}
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExportsUnordered(t, exports, "bar,foo")
}

// --- Test Case 14: NODE_ENV conditional ---
func TestNodeEnvConditional(t *testing.T) {
	source := `
		if (process.env.NODE_ENV === 'development') {
			module.exports = { foo: 'bar' }
		}
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "foo")
}

// --- Test Case 14.4: NODE_ENV !== ---
func TestNodeEnvNotEqual(t *testing.T) {
	source := `
		if (process.env.NODE_ENV !== 'development') {
			module.exports = { foo: 'bar' }
		}
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "")
}

// --- Test Case 14.5: typeof module !== 'undefined' ---
func TestTypeofModule(t *testing.T) {
	source := `
		if (typeof module !== 'undefined' && module.exports) {
			module.exports = { foo: 'bar' }
		}
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "foo")
}

// --- Test Case 14.7: "production" !== process.env.NODE_ENV && (function(){...})() ---
func TestNodeEnvGuardProduction(t *testing.T) {
	source := `
		"production" !== process.env.NODE_ENV && (function () {
			module.exports = { foo: 'bar' }
		})()
	`
	// When nodeEnv is "production", "production" !== "production" is false, so skip
	exports, _ := parseTest(t, source, Options{NodeEnv: "production"})
	assertExports(t, exports, "")
}

// --- Test Case 14.6: Same as 14.7 but with development ---
func TestNodeEnvGuardDevelopment(t *testing.T) {
	source := `
		"production" !== process.env.NODE_ENV && (function () {
			module.exports = { foo: 'bar' }
		})()
	`
	// When nodeEnv is "development", "production" !== "development" is true
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "foo")
}

// --- Test Case 17: module.exports = require("lib")() ---
func TestModuleExportsRequireCall(t *testing.T) {
	source := `
		module.exports = require("lib")()
	`
	_, reexports := parseTest(t, source, Options{NodeEnv: "development"})
	assertReexports(t, reexports, "lib()")
}

// --- Test Case 16: module.exports = fn() ---
func TestModuleExportsFnCall(t *testing.T) {
	source := `
		function fn() { return { foo: 'bar' } };
		module.exports = fn()
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "foo")
}

// --- Test Case 16.1: module.exports = arrowFn() ---
func TestModuleExportsArrowFnCall(t *testing.T) {
	source := `
		let fn = () => ({ foo: 'bar' });
		module.exports = fn()
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "foo")
}

// --- Test Case 16.2: function with intermediate object ---
func TestModuleExportsFnCallWithObj(t *testing.T) {
	source := `
		function fn() {
			const mod = { foo: 'bar' }
			mod.bar = 123
			return mod
		};
		module.exports = fn()
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExportsUnordered(t, exports, "bar,foo")
}

// --- Test Case 18: CallMode with module.exports = function ---
func TestCallModeFunction(t *testing.T) {
	source := `
		module.exports = function () {
			const mod = { foo: 'bar' }
			mod.bar = 123
			return mod
		};
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development", CallMode: true})
	assertExportsUnordered(t, exports, "bar,foo")
}

// --- Test Case 18.1: CallMode with function reference ---
func TestCallModeFunctionRef(t *testing.T) {
	source := `
		function fn() {
			const mod = { foo: 'bar' }
			mod.bar = 123
			return mod
		}
		module.exports = fn;
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development", CallMode: true})
	assertExportsUnordered(t, exports, "bar,foo")
}

// --- Test Case 18.2: CallMode with arrow function ref ---
func TestCallModeArrowRef(t *testing.T) {
	source := `
		const fn = () => {
			const mod = { foo: 'bar' }
			mod.bar = 123
			return mod
		}
		module.exports = fn;
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development", CallMode: true})
	assertExportsUnordered(t, exports, "bar,foo")
}

// --- Test Case 20.3: chained assignment ---
func TestChainedAssignment(t *testing.T) {
	source := `
		var foo = exports.foo = "bar";
		exports.greeting = "hello";
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "production"})
	assertExportsUnordered(t, exports, "foo,greeting")
}

// --- Test Case 20.4: chained assignment with module.exports ---
func TestChainedAssignmentModuleExports(t *testing.T) {
	source := `
		var foo = module.exports.foo = "bar";
		exports.greeting = "hello";
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "production"})
	assertExportsUnordered(t, exports, "foo,greeting")
}

// --- Test Case 20.5: multiple chained assignments ---
func TestMultipleChainedAssignment(t *testing.T) {
	source := `
		var title = exports.name = exports.title = exports.short = "untitled";
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "production"})
	assertExportsUnordered(t, exports, "name,short,title")
}

// --- Test Case 22: var url = module.exports = {} ---
func TestVarModuleExportsEmpty(t *testing.T) {
	source := `
		var url = module.exports = {};
		url.foo = 'bar';
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "production"})
	assertExports(t, exports, "foo")
}

// --- Test Case 22.2: chained exports init ---
func TestExportsChainedInit(t *testing.T) {
	source := `
		exports.i18n = exports.use = exports.t = undefined;
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "production"})
	assertExportsUnordered(t, exports, "i18n,t,use")
}

// --- Test Case 23: __export pattern ---
func TestExportHelper(t *testing.T) {
	source := `
		Object.defineProperty(exports, "__esModule", { value: true });
		__export({foo:"bar"});
		__export(require("./lib"));
	`
	exports, reexports := parseTest(t, source, Options{NodeEnv: "production"})
	assertExportsUnordered(t, exports, "__esModule,foo")
	assertReexports(t, reexports, "./lib")
}

// --- Test Case 24: Annotation pattern 0 && (module.exports = {...}) ---
func TestAnnotationPattern(t *testing.T) {
	source := `
		0 && (module.exports = {
			foo,
			bar
		});
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "production"})
	assertExportsUnordered(t, exports, "bar,foo")
}

// --- Test Case 3: Object.assign ---
func TestObjectAssign(t *testing.T) {
	source := `
		const alas = true
		const obj = { bar: 1 }
		obj.meta = 1
		Object.assign(module.exports, { alas, foo: 'bar', ...obj }, { ...require('a') }, require('b'))
	`
	exports, reexports := parseTest(t, source, Options{NodeEnv: "development"})
	assertExportsUnordered(t, exports, "alas,bar,foo,meta")
	assertReexportsUnordered(t, reexports, "a,b")
}

// --- Test Case 4: Object.assign(module, { exports: {...} }) ---
func TestObjectAssignModuleReplace(t *testing.T) {
	source := `
		Object.assign(module.exports, { foo: 'bar', ...require('lib') })
		Object.assign(module, { exports: { nope: true } })
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "nope")
}

// --- Test Case 19: __exportStar ---
func TestExportStar(t *testing.T) {
	source := `
		require("tslib").__exportStar({foo: 'bar'}, exports)
		exports.bar = 123
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "production"})
	assertExportsUnordered(t, exports, "bar,foo")
}

// --- Test Case 19.4: tslib exportStar with require ---
func TestExportStarRequire(t *testing.T) {
	source := `
		var tslib_1 = require("tslib");
		(0, tslib_1.__exportStar)(require("./crossPlatformSha256"), exports);
	`
	_, reexports := parseTest(t, source, Options{NodeEnv: "production"})
	assertReexports(t, reexports, "./crossPlatformSha256")
}

// --- Test Case 19.5: var __exportStar with require ---
func TestVarExportStar(t *testing.T) {
	source := `
		var __exportStar = function() {}
		Object.defineProperty(exports, "foo", { value: 1 });
		__exportStar(require("./bar"), exports);
	`
	exports, reexports := parseTest(t, source, Options{NodeEnv: "production"})
	assertExportsUnordered(t, exports, "foo")
	assertReexports(t, reexports, "./bar")
}

// --- Test: Empty source ---
func TestEmptySource(t *testing.T) {
	result, err := Parse("", "empty.js", Options{})
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(result.Exports) != 0 {
		t.Errorf("expected no exports, got %v", result.Exports)
	}
	if len(result.Reexports) != 0 {
		t.Errorf("expected no reexports, got %v", result.Reexports)
	}
}

// --- Test: Invalid source ---
func TestInvalidSource(t *testing.T) {
	// esbuild parser is very resilient, so we just check it doesn't panic
	result, err := Parse("function {{{", "bad.js", Options{})
	_ = result
	_ = err
}

// --- Test Case 14.1: Destructured NODE_ENV ---
func TestNodeEnvDestructured(t *testing.T) {
	source := `
		const { NODE_ENV } = process.env
		if (NODE_ENV === 'development') {
			module.exports = { foo: 'bar' }
		}
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "foo")
}

// --- Test Case 14.3: var denv = process.env.NODE_ENV ---
func TestNodeEnvVarAlias(t *testing.T) {
	source := `
		const denv = process.env.NODE_ENV
		if (denv === 'development') {
			module.exports = { foo: 'bar' }
		}
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "foo")
}

// --- Test Case 20.1: exports.foo || (exports.foo = {}) pattern ---
func TestExportsLogicalOr(t *testing.T) {
	source := `
		var foo;
		foo = exports.foo || (exports.foo = {});
		var bar = exports.bar || (exports.bar = {});
		exports.greeting = "hello";
	`
	exports, _ := parseTest(t, source, Options{NodeEnv: "production"})
	assertExportsUnordered(t, exports, "bar,foo,greeting")
}

// --- Test: module.exports = require("lib"); lib.foo = 'bar' ---
func TestRequireWithPropertyAssignment(t *testing.T) {
	source := `
		var lib = require("lib")
		lib.foo = 'bar'
		module.exports = lib
	`
	exports, reexports := parseTest(t, source, Options{NodeEnv: "development"})
	assertExports(t, exports, "foo")
	assertReexports(t, reexports, "lib")
}

// --- Test: Basic sanity ---
func TestSanity(t *testing.T) {
	source := `
		exports.hello = 'world';
		exports.answer = 42;
	`
	exports, _ := parseTest(t, source, Options{})
	assertExportsUnordered(t, exports, "answer,hello")
}

// --- Test: module.exports object with various property types ---
func TestModuleExportsObjectLiteral(t *testing.T) {
	source := `
		const a = 1;
		module.exports = {
			a,
			b: 2,
			c: function() {},
			d: () => {},
		}
	`
	exports, _ := parseTest(t, source, Options{})
	assertExportsUnordered(t, exports, "a,b,c,d")
}
