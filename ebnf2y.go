// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"io"
	"log"
	"os"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"code.google.com/p/go.exp/ebnf"
	"github.com/cznic/strutil"
)

const (
	sep = ""
)

var todo = strings.ToUpper("todo")

func dbg(s string, va ...interface{}) {
	_, fn, fl, _ := runtime.Caller(1)
	fmt.Printf("%s:%d: ", path.Base(fn), fl)
	fmt.Printf(s, va...)
	fmt.Println()
}

type job struct {
	grm            ebnf.Grammar
	literals       map[string]bool
	namedTerminals ebnf.Grammar
	names          map[string]bool
	nonTerminals   ebnf.Grammar
	repetitions    map[string]bool
	tPrefix        string
	term2name      map[string]string
}

func (j *job) inventName(prefix, sep string) (s string) {
	for i := 0; ; i++ {
		switch {
		case i == 0 && sep == "":
			s = fmt.Sprintf("%s%s", prefix, sep)
		case i == 0:
			continue
		case i != 0:
			s = fmt.Sprintf("%s%s%d", prefix, sep, i)
		}
		if _, ok := j.names[s]; !ok {
			j.names[s] = true
			return s
		}
	}
}
func (j *job) toBnf() {
	bnf := ebnf.Grammar{}
	j.repetitions = map[string]bool{}

	var f func(string, ebnf.Expression, int) ebnf.Expression

	add := func(name string, expr ebnf.Expression) (nm *ebnf.Name) {
		j.names[name] = true
		nm = &ebnf.Name{String: name}
		bnf[name] = &ebnf.Production{
			Name: nm,
			Expr: f(name, expr, 0),
		}
		return
	}

	f = func(name string, expr ebnf.Expression, nest int) ebnf.Expression {
		nest++
		switch x := expr.(type) {
		case ebnf.Alternative:
			if nest == 1 {
				var y ebnf.Alternative
				for _, v := range x {
					y = append(y, f(name, v, nest))
				}
				return y
			}

			return add(j.inventName(name, sep), x)
		case *ebnf.Group:
			return add(j.inventName(name, sep), x.Body)
		case *ebnf.Name:
			return &ebnf.Name{String: x.String}
		case *ebnf.Option:
			return add(j.inventName(name, sep), ebnf.Alternative{
				0: nil,
				1: x.Body,
			})
		case *ebnf.Repetition:
			newName := j.inventName(name, sep)
			j.repetitions[newName] = true
			return add(newName, ebnf.Alternative{
				0: nil,
				1: ebnf.Sequence{
					0: &ebnf.Name{String: newName},
					1: x.Body,
				},
			})
		case *ebnf.Range:
			return &ebnf.Range{
				Begin: &ebnf.Token{String: x.Begin.String},
				End:   &ebnf.Token{String: x.End.String},
			}
		case ebnf.Sequence:
			var y ebnf.Sequence
			for _, v := range x {
				y = append(y, f(name, v, nest))
			}
			return y
		case *ebnf.Token:
			return &ebnf.Token{String: x.String}
		case nil:
			return nil
		default:
			log.Fatalf("internal error %T(%#v)", x, x)
			panic("unreachable")
		}
	}

	for name, prod := range j.grm {
		add(name, prod.Expr)
	}
	j.grm = bnf
}

func (j *job) checkTerminals(start string) {
	j.nonTerminals = ebnf.Grammar{}
	j.namedTerminals = ebnf.Grammar{}
	j.literals = map[string]bool{}
	visited := map[*ebnf.Production]bool{}
	var f func(interface{})

	f = func(v interface{}) {
		switch x := v.(type) {
		case *ebnf.Production:
			if x == nil || visited[x] {
				return
			}

			visited[x] = true
			nm := x.Name.String
			if !ast.IsExported(nm) {
				j.namedTerminals[nm] = x
				return
			}

			j.nonTerminals[nm] = x
			f(x.Expr)
		case ebnf.Sequence:
			for _, v := range x {
				f(v)
			}
		case *ebnf.Repetition:
			f(x.Body)
		case *ebnf.Token:
			j.literals[x.String] = true
		case *ebnf.Name:
			f(j.grm[x.String])
		case ebnf.Alternative:
			for _, v := range x {
				f(v)
			}
		case *ebnf.Group:
			f(x.Body)
		case *ebnf.Option:
			f(x.Body)
		case nil:
			// nop
		case *ebnf.Range:
			// nop
		default:
			log.Fatalf("internal error %T(%#v)", x, x)
		}

	}

	f(j.grm[start])
	return
}

func toAscii(s string) string {
	var r []byte
	for _, b := range s {
		if b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' {
			r = append(r, byte(b))
		}
	}
	return string(r)
}

func (j *job) str(expr ebnf.Expression) (s string) {
	switch x := expr.(type) {
	case nil:
		return "/* EMPTY */"
	case *ebnf.Name:
		switch name := x.String; ast.IsExported(name) {
		case true:
			return name
		default:
			return j.term2name[name]
		}
	case ebnf.Sequence:
		a := []string{}
		for _, v := range x {
			a = append(a, j.str(v))
		}
		return strings.Join(a, " ")
	case *ebnf.Token:
		switch s := x.String; len(s) {
		case 1:
			return strconv.QuoteRune(rune(s[0]))
		default:
			hint := ""
			if _, ok := j.literals[s]; ok && toAscii(s) == "" {
				hint = fmt.Sprintf(" /* %q */", s)
			}
			return fmt.Sprintf("%s%s", j.term2name[s], hint)
		}
	default:
		log.Fatalf("%T(%#v)", x, x)
		panic("unreachable")
	}
}

var sIsStart = map[bool]string{
	false: "$$",
	true:  "_parserResult",
}

const (
	rep0 = iota
	rep1
)

func (j *job) ystr(expr ebnf.Expression, name, start string, rep int) (s string) {
	a := []string{}

	var f func(ebnf.Expression)
	f = func(expr ebnf.Expression) {
		switch x := expr.(type) {
		case nil:
			// nop
		case *ebnf.Name:
			a = append(a, fmt.Sprintf("$%d", len(a)+1))
		case ebnf.Sequence:
			for _, v := range x {
				f(v)
			}
		case *ebnf.Token:
			a = append(a, fmt.Sprintf("%q", x.String))
		default:
			log.Fatalf("%T(%#v)", x, x)
			panic("unreachable")
		}
	}

	f(expr)
	switch j.repetitions[name] {
	case true:
		switch rep {
		case 0:
			return fmt.Sprintf("$$ = []%s(nil)", name)
		case 1:
			return fmt.Sprintf("$$ = append($1.([]%s), %s)", name, strings.Join(a[1:], ", "))
		default:
			log.Fatal("internal error")
			panic("unreachable")
		}
	case false:
		switch len(a) {
		case 0:
			return fmt.Sprintf("%s = nil", sIsStart[name == start])
		case 1:
			return fmt.Sprintf("%s = %s", sIsStart[name == start], a[0])
		default:
			return fmt.Sprintf("%s = []%s{%s}", sIsStart[name == start], name, strings.Join(a, ", "))
		}
	}
	panic("unreachable")
}

func (j *job) render(w io.Writer, start string) (err error) {
	f := strutil.IndentFormatter(w, "\t")
	f.Format(`%%{

//%s Put your favorite license here
		
// yacc source generated by ebnf2y[1]
// at %s.
//
// CAUTION: If this file is a Go source file (*.go), it was generated
// automatically by '$ go tool yacc' from a *.y file - DO NOT EDIT in that case!
// 
//   [1]: http://github.com/cznic/ebnf2y

package main //%s real package name

//%s required only be the demo _dump function
import (
	"bytes"
	"fmt"
	"strings"

	"github.com/cznic/strutil"
)

%%}

%%union {
	item interface{} //%s insert real field(s)
}

`, todo, time.Now(), todo, todo, todo)
	j.term2name = map[string]string{}
	a := []string{}
	for name := range j.namedTerminals {
		token := j.inventName(j.tPrefix+strings.ToUpper(name), "")
		j.term2name[name] = token
		a = append(a, token)
	}
	if len(a) != 0 {
		sort.Strings(a)
		for _, name := range a {
			f.Format("%%token\t%s\n", name)
		}
		f.Format("\n%%type\t<item> \t/*%s real type(s), if/where applicable */\n", todo)
		for _, name := range a {
			f.Format("\t%s\n", name)
		}
		f.Format("\n")
	}

	j.inventName(j.tPrefix+"TOK", "")
	a = a[:0]
	for lit := range j.literals {
		if len(lit) == 1 || toAscii(lit) != "" {
			continue
		}

		j.term2name[lit] = j.inventName(j.tPrefix+"TOK", "")
		a = append(a, lit)
	}
	if len(a) != 0 {
		for _, lit := range a {
			f.Format("%%token\t%s\t/*%s Name for %q */\n", j.term2name[lit], todo, lit)
		}
		f.Format("\n")
		f.Format("%%type\t<item> \t/*%s real type(s), if/where applicable */\n", todo)
		for _, lit := range a {
			f.Format("\t%s\n", j.term2name[lit])
		}
		f.Format("\n")
	}

	a = a[:0]
	for lit := range j.literals {
		nm := toAscii(lit)
		if len(lit) == 1 || nm == "" {
			continue
		}

		name := j.inventName(j.tPrefix+strings.ToUpper(nm), "")
		j.term2name[lit] = name
		a = append(a, name)
	}
	if len(a) != 0 {
		sort.Strings(a)
		for _, name := range a {
			f.Format("%%token %s\n", name)
		}
		f.Format("\n")
	}

	a = a[:0]
	for name := range j.nonTerminals {
		a = append(a, name)
	}
	sort.Strings(a)
	f.Format("%%type\t<item> \t/*%s real type(s), if/where applicable */\n", todo)
	for _, name := range a {
		f.Format("\t%s\n", name)
	}
	f.Format("\n")

	f.Format("/*%s %%left, %%right, ... declarations */\n\n%%start %s\n\n%%%%\n\n", todo, start)

	rule := 0
	for _, name := range a {
		f.Format("%s:\n\t", name)
		expr := j.grm[name].Expr
		switch x := expr.(type) {
		case ebnf.Alternative:
			for i, v := range x {
				if i != 0 {
					f.Format("|\t")
				}
				rule++
				f.Format("%s\n\t{\n\t\t%s //%s %d\n\t}\n", j.str(v), j.ystr(v, name, start, i), todo, rule)
			}
		default:
			rule++
			f.Format("%s\n\t{\n\t\t%s //%s %d\n\t}\n", j.str(x), j.ystr(x, name, start, -1), todo, rule)
		}
		f.Format("\n")
	}

	f.Format(`%%%%

//%s remove demo stuff below

var _parserResult interface{}

type (%i
`, todo)

	for _, name := range a {
		f.Format("%s interface{}\n", name)
	}

	f.Format(`%u)
	
func _dump() {
	s := fmt.Sprintf("%%#v", _parserResult)
	s = strings.Replace(s, "%%", "%%%%", -1)
	s = strings.Replace(s, "{", "{%%i\n", -1)
	s = strings.Replace(s, "}", "%%u\n}", -1)
	s = strings.Replace(s, ", ", ",\n", -1)
	var buf bytes.Buffer
	strutil.IndentFormatter(&buf, ". ").Format(s)
	buf.WriteString("\n")
	a := strings.Split(buf.String(), "\n")
	for _, v := range a {
		if strings.HasSuffix(v, "(nil)") || strings.HasSuffix(v, "(nil),") {
			continue
		}
	
		fmt.Println(v)
	}
}

// End of demo stuff
`)
	return
}

func prodLen(expr ebnf.Expression) (y int) {
	var f func(ebnf.Expression)
	f = func(expr ebnf.Expression) {
		switch x := expr.(type) {
		case nil:
			// nop
		case ebnf.Sequence:
			for _, v := range x {
				f(v)
			}
		case ebnf.Alternative:
			for _, v := range x {
				f(v)
			}
		case *ebnf.Option:
			f(x.Body)
		case *ebnf.Group:
			f(x.Body)
		case *ebnf.Repetition:
			f(x.Body)
		case *ebnf.Name, *ebnf.Token, *ebnf.Range:
			y++
		default:
			log.Fatalf("internal error %T(%#v)", x, x)
		}
	}
	f(expr)
	return
}

func unGroup(expr ebnf.Expression, safe bool) ebnf.Expression {
	for {
		if x, ok := expr.(*ebnf.Group); ok && (!safe || prodLen(x.Body) == 1) {
			expr = x.Body
			continue
		}

		break
	}
	return expr
}

func minEbnf(fname string, grm ebnf.Grammar) (b []byte) {
	refs := map[string][]*ebnf.Expression{}
	selfRefs := map[string]bool{}
	f := func(string, *ebnf.Expression) {}
	f = func(name string, expr *ebnf.Expression) {
		switch x := (*expr).(type) {
		case nil:
			selfRefs[name] = true
		case *ebnf.Token, *ebnf.Range:
			// nop
		case ebnf.Alternative:
			for i := range x {
				f(name, &x[i])
			}
		case ebnf.Sequence:
			for i := range x {
				f(name, &x[i])
			}
		case *ebnf.Repetition:
			f(name, &x.Body)
		case *ebnf.Option:
			f(name, &x.Body)
		case *ebnf.Group:
			f(name, &x.Body)
		case *ebnf.Name:
			switch nm := x.String; nm == name {
			case true:
				selfRefs[name] = true
			default:
				refs[nm] = append(refs[nm], expr)
			}
		default:
			log.Fatalf("internal error %T(%#v)", x, x)
		}
	}
	for name, prod := range grm {
		f(name, &prod.Expr)
	}
	for stable := false; !stable; {
		stable = true
		for name, prod := range grm {
			ref := refs[name]
			if len(ref) == 0 || len(ref) > 1 || selfRefs[name] ||
				!ast.IsExported(name) {
				if !(len(ref) != 0 && prodLen(prod.Expr) == 1) {
					continue
				}
			}

			for _, ref := range ref {
				expr := prod.Expr
				if _, ok := expr.(*ebnf.Token); ok {
					continue
				}

				stable = false
				delete(refs, name)
				delete(grm, name)
				*ref = &ebnf.Group{Body: prod.Expr}
			}
		}

	}

	file, err := os.Create(fname)
	if err != nil {
		log.Fatal(err)
	}
	a := []string{}
	for name := range grm {
		a = append(a, name)
	}
	sort.Strings(a)
	var buf bytes.Buffer
	fm := strutil.IndentFormatter(&buf, "\t")
	g := (func(ebnf.Expression))(nil)
	g = func(expr ebnf.Expression) {
		expr = unGroup(expr, true)
		switch x := expr.(type) {
		case nil:
			// nop
		case ebnf.Sequence:
			for stable := false; !stable; {
				stable = true
			L:
				for i := 0; i < len(x); i++ {
					v := x[i]
					if xx, ok := v.(*ebnf.Group); ok {
						if xxx, ok := xx.Body.(ebnf.Sequence); ok {
							for _, v := range xxx {
								if prodLen(v) != 1 {
									continue L
								}
							}

							stable = false
							x = append(x[:i], x[i+1:]...)
							for _, v := range xxx {
								x = append(x[:i], append(ebnf.Sequence{0: v}, x[i:]...)...)
								i++
							}
						}
					}
				}
			}
			for _, v := range x {
				g(v)
			}
		case ebnf.Alternative:
			for stable := false; !stable; {
				stable = true
				for i, v := range x {
					if xx, ok := v.(*ebnf.Group); ok {
						x[i] = xx.Body
						stable = false
					}
				}
				for i := 0; i < len(x); i++ {
					if xx, ok := x[i].(*ebnf.Alternative); ok {
						for _, v := range *xx {
							x = append(x, v)
						}
						x = append(x[:i], x[i+1:]...)
						stable = false
					}
				}
			}
			for i, v := range x {
				switch i {
				case 0:
					fm.Format("\n ")
					g(v)
				default:
					fm.Format("\n|")
					g(v)
				}
			}
		case *ebnf.Name:
			fm.Format(" %s", x.String)
		case *ebnf.Repetition:
			switch prodLen(x.Body) {
			case 1:
				fm.Format(" {")
			default:
				fm.Format(" {%i\n")
			}
			g(unGroup(x.Body, false))
			switch prodLen(x.Body) {
			case 1:
				fm.Format(" }")
			default:
				fm.Format("\n%u }")
			}
		case *ebnf.Group:
			switch prodLen(x.Body) {
			case 1:
				fm.Format(" (")
			default:
				fm.Format(" (%i\n")
			}
			g(unGroup(x.Body, false))
			switch prodLen(x.Body) {
			case 1:
				fm.Format(" )")
			default:
				fm.Format("\n%u )")
			}
		case *ebnf.Option:
			switch prodLen(x.Body) {
			case 1:
				fm.Format(" [")
			default:
				fm.Format(" [%i\n")
			}
			g(unGroup(x.Body, false))
			switch prodLen(x.Body) {
			case 1:
				fm.Format(" ]")
			default:
				fm.Format("\n%u ]")
			}
		case *ebnf.Token:
			fm.Format(" %q", x.String)
		case *ebnf.Range:
			fm.Format(" %q … %q", x.Begin.String, x.End.String)
		default:
			log.Fatalf("%T(%#v)", x, x)
		}
	}
	for _, name := range a {
		fm.Format("%s =", name)
		prod := grm[name].Expr
		switch prodLen(prod) {
		case 0:
			fm.Format(" .\n")
		default:
			fm.Format("%i")
			g(grm[name].Expr)
			fm.Format(" .%u\n")
		}
	}
	b = buf.Bytes()
	c := func(o, n string) {
		for {
			l := len(b)
			b = bytes.Replace(b, []byte(o), []byte(n), -1)
			if len(b) == l {
				return
			}
		}
	}
	c("|\n\t\t", "|\n\t")
	c("|\n\t", "|")
	c(" \n", "\n")
	c("\t\n", "\n")
	c("\n\n", "\n")
	n, err := file.Write(b)
	if n != len(b) {
		log.Fatalf("%q: Short write", fname)
	}

	if err != nil {
		log.Fatal(err)
	}

	err = file.Close()
	if err != nil {
		log.Fatal(err)
	}

	return
}

func main() {
	oStart := flag.String("start", "SourceFile", "Start production name.")
	oOut := flag.String("o", "", "Output file. Stdout if left blank.")
	oPrefix := flag.String("p", "", "Prefix for token names, eg. \"_\". Default blank.")
	//TODO oMinEbnf := flag.String("me", "", `Minimize EBNF and write it to <arg>.`)
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	if flag.NArg() > 1 {
		log.Fatal("Atmost one input file may be specified.")
	}

	var err error
	var in *os.File

	switch name := flag.Arg(0); {
	case name == "":
		in = os.Stdin
	default:
		if in, err = os.Open(name); err != nil {
			log.Fatal(err)
		}
	}

	grm, err := ebnf.Parse(in.Name(), in)
	if err != nil {
		log.Fatal(err)
	}

	if err := ebnf.Verify(grm, *oStart); err != nil {
		log.Fatal(err)
	}

	//TODO
	//if nm := *oMinEbnf; nm != "" {
	//	b := minEbnf(nm, grm)
	//	buf := bytes.NewReader(b)
	//	grm, err = ebnf.Parse(nm, buf)
	//	if err != nil {
	//		log.Fatal(err)
	//	}

	//	if err := ebnf.Verify(grm, *oStart); err != nil {
	//		log.Fatal(err)
	//	}

	//}

	j := &job{
		grm:     grm,
		names:   map[string]bool{},
		tPrefix: *oPrefix,
	}
	for _, name := range []string{
		"break", "default", "func", "interface", "select",
		"case", "defer", "go", "map", "struct",
		"chan", "else", "goto", "package", "switch",
		"const", "fallthrough", "if", "range", "type",
		"continue", "for", "import", "return", "var",
	} {
		j.names[name] = true
	}
	for name := range grm {
		if j.names[name] {
			log.Fatalf("Reserved word %q cannot be used as a production name.", name)
		}

		j.names[name] = true
	}
	start := j.inventName("Start", "")
	j.grm[start] = &ebnf.Production{
		Name: &ebnf.Name{String: start},
		Expr: &ebnf.Name{String: *oStart},
	}

	j.toBnf()
	j.checkTerminals(start)
	out := os.Stdout
	if s := *oOut; s != "" {
		if out, err = os.Create(s); err != nil {
			log.Fatal(err)
		}
	}

	w := bufio.NewWriter(out)
	if err = j.render(w, start); err != nil {
		log.Fatal(err)
	}

	if err = w.Flush(); err != nil {
		log.Fatal(err)
	}

	if err = out.Close(); err != nil {
		log.Fatal(err)
	}
}
