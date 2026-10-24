package harnesseval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/subprocess"
)

type behaviorCase struct{ Expression, Want string }

// Private cases use the authored Go expression format, but their expected
// answers stay exclusively in this trusted process. They are never compiled
// into, mounted in, or sent to the candidate program.
func behaviorCases(tests string) ([]behaviorCase, error) {
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, "private.go", tests, 0)
	if err != nil {
		return nil, err
	}
	var cases []behaviorCase
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "TestAcceptance" {
			continue
		}
		for _, statement := range fn.Body.List {
			check, ok := statement.(*ast.IfStmt)
			if !ok {
				return nil, errors.New("harnesseval: unsupported private case statement")
			}
			assignment, ok := check.Init.(*ast.AssignStmt)
			if !ok || len(assignment.Rhs) != 1 {
				return nil, errors.New("harnesseval: private case must capture one result")
			}
			call, ok := assignment.Rhs[0].(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return nil, errors.New("harnesseval: private case must use one expression")
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Sprint" {
				return nil, errors.New("harnesseval: unsupported private formatter")
			}
			comparison, ok := check.Cond.(*ast.BinaryExpr)
			if !ok || comparison.Op != token.NEQ {
				return nil, errors.New("harnesseval: private case must compare its result")
			}
			literal, ok := comparison.Y.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return nil, errors.New("harnesseval: private answer must be a string")
			}
			want, err := strconv.Unquote(literal.Value)
			if err != nil {
				return nil, err
			}
			start, end := set.Position(call.Args[0].Pos()).Offset, set.Position(call.Args[0].End()).Offset
			cases = append(cases, behaviorCase{Expression: tests[start:end], Want: want})
		}
	}
	if len(cases) == 0 || len(cases) > 32 {
		return nil, errors.New("harnesseval: private case count outside bounds")
	}
	return cases, nil
}

// qualifyExpression turns the public API calls into calls on a separately
// compiled candidate package. String literals and selector/field names survive.
func qualifyExpression(expression string, exports map[string]bool) string {
	set := token.NewFileSet()
	file := set.AddFile("expression.go", -1, len(expression))
	var scan scanner.Scanner
	scan.Init(file, []byte(expression), nil, 0)
	type item struct {
		offset int
		tok    token.Token
		text   string
	}
	var tokens []item
	for {
		pos, tok, text := scan.Scan()
		if tok == token.EOF {
			break
		}
		tokens = append(tokens, item{file.Offset(pos), tok, text})
	}
	var out strings.Builder
	copied := 0
	for i, part := range tokens {
		if part.tok != token.IDENT || !exports[part.text] || i > 0 && tokens[i-1].tok == token.PERIOD || i+1 < len(tokens) && tokens[i+1].tok == token.COLON {
			continue
		}
		out.WriteString(expression[copied:part.offset])
		out.WriteString("candidate.")
		out.WriteString(part.text)
		copied = part.offset + len(part.text)
	}
	out.WriteString(expression[copied:])
	return out.String()
}

func exportedNames(source Source) (map[string]bool, error) {
	exports := map[string]bool{}
	for path, data := range source {
		if filepath.Dir(path) != "." || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, data, 0)
		if err != nil {
			return nil, err
		}
		for _, declaration := range file.Decls {
			switch d := declaration.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.IsExported() {
					exports[d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch v := spec.(type) {
					case *ast.TypeSpec:
						if v.Name.IsExported() {
							exports[v.Name.Name] = true
						}
					case *ast.ValueSpec:
						for _, n := range v.Names {
							if n.IsExported() {
								exports[n.Name] = true
							}
						}
					}
				}
			}
		}
	}
	return exports, nil
}

type program struct {
	root, binary string
	confiner     *Confiner
}

func (p *program) Close() { _ = os.RemoveAll(p.root) }

// buildProgram accepts expressions only: the API cannot receive expected
// answers. Compilation does not execute candidate initializers (CGO is off).
func buildProgram(ctx context.Context, source Source, expressions []string) (*program, hx.SpawnResult, error) {
	var empty hx.SpawnResult
	exports, err := exportedNames(source)
	if err != nil {
		return nil, empty, err
	}
	compile, err := os.MkdirTemp("", "forge-compile-*")
	if err != nil {
		return nil, empty, err
	}
	defer os.RemoveAll(compile)
	compile, err = filepath.EvalSymlinks(compile)
	if err != nil {
		return nil, empty, err
	}
	if err = os.Mkdir(filepath.Join(compile, "candidate"), 0700); err != nil {
		return nil, empty, err
	}
	code := Source{}
	for path, text := range source {
		if !strings.HasSuffix(path, "_test.go") {
			code[path] = text
		}
	}
	if err = code.Materialize(filepath.Join(compile, "candidate")); err != nil {
		return nil, empty, err
	}
	var wrapper strings.Builder
	wrapper.WriteString("package main\nimport (\"encoding/json\";\"fmt\";\"os\";candidate \"./candidate\")\nfunc main(){result:=struct{Value string `json:\"value\"`;Panicked bool `json:\"panicked\"`}{ };defer func(){if recover()!=nil{result.Panicked=true};json.NewEncoder(os.Stdout).Encode(result)}();if len(os.Args)!=2{result.Panicked=true;return};switch os.Args[1]{\n")
	for i, expression := range expressions {
		fmt.Fprintf(&wrapper, "case %q: result.Value=fmt.Sprint(%s)\n", strconv.Itoa(i), qualifyExpression(expression, exports))
	}
	wrapper.WriteString("default:result.Panicked=true\n}}\n")
	if err = os.WriteFile(filepath.Join(compile, "runner.go"), []byte(wrapper.String()), 0600); err != nil {
		return nil, empty, err
	}
	if err = seedToolchain(ctx, compile, ".forge/go-cache"); err != nil {
		return nil, empty, err
	}
	compiler, err := NewConfiner(compile)
	if err != nil {
		return nil, empty, err
	}
	if err = compiler.Preflight(ctx); err != nil {
		return nil, empty, err
	}
	run := subprocess.New(compiler).Spawn(hx.SpawnRequest{Ctx: ctx, Argv: []string{filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", filepath.Join(compile, "program"), "."}, Dir: compile, Env: Environment(compile), TimeoutMs: 60000})
	if run.Err != "" {
		return nil, run, errors.Join(ErrInfrastructure, errors.New(run.Err))
	}
	if run.ExitCode != 0 || run.TimedOut {
		return nil, run, nil
	}
	root, err := os.MkdirTemp("", "forge-program-*")
	if err != nil {
		return nil, run, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, run, err
	}
	p := &program{root: root, binary: filepath.Join(root, "program")}
	complete := false
	defer func() {
		if !complete {
			p.Close()
		}
	}()
	in, err := os.Open(filepath.Join(compile, "program"))
	if err != nil {
		return nil, run, err
	}
	defer in.Close()
	out, err := os.OpenFile(p.binary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		return nil, run, err
	}
	_, copyErr := io.Copy(out, in)
	if err = errors.Join(copyErr, out.Close()); err != nil {
		return nil, run, err
	}
	p.confiner, err = NewConfiner(root)
	if err != nil {
		return nil, run, err
	}
	if err = p.confiner.Preflight(ctx); err != nil {
		return nil, run, err
	}
	complete = true
	return p, run, nil
}

func (p *program) query(ctx context.Context, id int) (string, bool, hx.SpawnResult) {
	run := subprocess.New(p.confiner).Spawn(hx.SpawnRequest{Ctx: ctx, Argv: []string{p.binary, strconv.Itoa(id)}, Dir: p.root, Env: Environment(p.root), TimeoutMs: 10000, OutputLimit: 8192})
	var answer struct {
		Value    string `json:"value"`
		Panicked bool   `json:"panicked"`
	}
	decoder := json.NewDecoder(bytes.NewReader(run.Stdout))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&answer)
	var extra any
	end := decoder.Decode(&extra)
	return answer.Value, err == nil && end == io.EOF && !answer.Panicked && run.ExitCode == 0 && run.Err == "" && !run.TimedOut, run
}
