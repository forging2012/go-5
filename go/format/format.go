// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package format implements standard formatting of Go source.
package format

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"strings"
)

var config = printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 8}

const parserMode = parser.ParseComments

// Node formats node in canonical gofmt style and writes the result to dst.
//
// The node type must be *ast.File, *printer.CommentedNode, []ast.Decl,
// []ast.Stmt, or assignment-compatible to ast.Expr, ast.Decl, ast.Spec,
// or ast.Stmt. Node does not modify node. Imports are not sorted for
// nodes representing partial source files (i.e., if the node is not an
// *ast.File or a *printer.CommentedNode not wrapping an *ast.File).
//
// The function may return early (before the entire result is written)
// and return a formatting error, for instance due to an incorrect AST.
//
func Node(dst io.Writer, fset *token.FileSet, node interface{}) error {
	// Determine if we have a complete source file (file != nil).
	var file *ast.File
	var cnode *printer.CommentedNode
	switch n := node.(type) {
	case *ast.File:
		file = n
	case *printer.CommentedNode:
		if f, ok := n.Node.(*ast.File); ok {
			file = f
			cnode = n
		}
	}

	// Sort imports if necessary.
	if file != nil && hasUnsortedImports(file) {
		// Make a copy of the AST because ast.SortImports is destructive.
		// TODO(gri) Do this more efficiently.
		var buf bytes.Buffer
		err := config.Fprint(&buf, fset, file)
		if err != nil {
			return err
		}
		file, err = parser.ParseFile(fset, "", buf.Bytes(), parserMode)
		if err != nil {
			// We should never get here. If we do, provide good diagnostic.
			return fmt.Errorf("format.Node internal error (%s)", err)
		}
		ast.SortImports(fset, file)

		// Use new file with sorted imports.
		node = file
		if cnode != nil {
			node = &printer.CommentedNode{Node: file, Comments: cnode.Comments}
		}
	}

	return config.Fprint(dst, fset, node)
}

// Source formats src in canonical gofmt style and returns the result
// or an (I/O or syntax) error. src is expected to be a syntactically
// correct Go source file, or a list of Go declarations or statements.
//
// If src is a partial source file, the leading and trailing space of src
// is applied to the result (such that it has the same leading and trailing
// space as src), and the result is indented by the same amount as the first
// line of src containing code. Imports are not sorted for partial source files.
//
func Source(src []byte) ([]byte, error) {
	fset := token.NewFileSet()
	file, adjust, adjustIndent, err := parse(fset, "", src, true)
	if err != nil {
		return nil, err
	}

	var res []byte
	if adjust == nil {
		// Complete source file.
		ast.SortImports(fset, file)
		var buf bytes.Buffer
		err := config.Fprint(&buf, fset, file)
		if err != nil {
			return nil, err
		}
		res = buf.Bytes()

	} else {
		// Partial source file.
		// Determine and prepend leading space.
		i, j := 0, 0
		for j < len(src) && isSpace(src[j]) {
			if src[j] == '\n' {
				i = j + 1 // byte offset of last line in leading space
			}
			j++
		}
		res = append(res, src[:i]...)

		// Determine and prepend indentation of first code line.
		// Spaces are ignored unless there are no tabs,
		// in which case spaces count as one tab.
		indent := 0
		hasSpace := false
		for _, b := range src[i:j] {
			switch b {
			case ' ':
				hasSpace = true
			case '\t':
				indent++
			}
		}
		if indent == 0 && hasSpace {
			indent = 1
		}
		for i := 0; i < indent; i++ {
			res = append(res, '\t')
		}

		// Format the source.
		// Write it without any leading and trailing space.
		cfg := config
		cfg.Indent = indent + adjustIndent
		var buf bytes.Buffer
		err := cfg.Fprint(&buf, fset, file)
		if err != nil {
			return nil, err
		}
		res = append(res, adjust(buf.Bytes(), indent+adjustIndent)...)

		// Determine and append trailing space.
		i = len(src)
		for i > 0 && isSpace(src[i-1]) {
			i--
		}
		res = append(res, src[i:]...)
	}

	return res, nil
}

func hasUnsortedImports(file *ast.File) bool {
	for _, d := range file.Decls {
		d, ok := d.(*ast.GenDecl)
		if !ok || d.Tok != token.IMPORT {
			// Not an import declaration, so we're done.
			// Imports are always first.
			return false
		}
		if d.Lparen.IsValid() {
			// For now assume all grouped imports are unsorted.
			// TODO(gri) Should check if they are sorted already.
			return true
		}
		// Ungrouped imports are sorted by default.
	}
	return false
}

// parse parses src, which was read from filename,
// as a Go source file or statement list.
func parse(fset *token.FileSet, filename string, src []byte, stdin bool) (*ast.File, func(src []byte, indent int) []byte, int, error) {
	// Try as whole source file.
	file, err := parser.ParseFile(fset, filename, src, parserMode)
	if err == nil {
		return file, nil, 0, nil
	}
	// If the error is that the source file didn't begin with a
	// package line and this is standard input, fall through to
	// try as a source fragment.  Stop and return on any other error.
	if !stdin || !strings.Contains(err.Error(), "expected 'package'") {
		return nil, nil, 0, err
	}

	// If this is a declaration list, make it a source file
	// by inserting a package clause.
	// Insert using a ;, not a newline, so that the line numbers
	// in psrc match the ones in src.
	psrc := append([]byte("package p;"), src...)
	file, err = parser.ParseFile(fset, filename, psrc, parserMode)
	if err == nil {
		adjust := func(src []byte, indent int) []byte {
			// Remove the package clause.
			// Gofmt has turned the ; into a \n.
			src = src[indent+len("package p\n"):]
			return bytes.TrimSpace(src)
		}
		adjustIndent := 0
		return file, adjust, adjustIndent, nil
	}
	// If the error is that the source file didn't begin with a
	// declaration, fall through to try as a statement list.
	// Stop and return on any other error.
	if !strings.Contains(err.Error(), "expected declaration") {
		return nil, nil, 0, err
	}

	// If this is a statement list, make it a source file
	// by inserting a package clause and turning the list
	// into a function body.  This handles expressions too.
	// Insert using a ;, not a newline, so that the line numbers
	// in fsrc match the ones in src.
	fsrc := append(append([]byte("package p; func _() {"), src...), '\n', '}')
	file, err = parser.ParseFile(fset, filename, fsrc, parserMode)
	if err == nil {
		adjust := func(src []byte, indent int) []byte {
			// Cap adjusted indent to zero.
			if indent < 0 {
				indent = 0
			}
			// Remove the wrapping.
			// Gofmt has turned the ; into a \n\n.
			// There will be two non-blank lines with indent, hence 2*indent.
			src = src[2*indent+len("package p\n\nfunc _() {"):]
			src = src[:len(src)-(indent+len("\n}\n"))]
			return bytes.TrimSpace(src)
		}
		// Gofmt has also indented the function body one level.
		// Remove that indent by returning adjustIndent value of -1.
		adjustIndent := -1
		return file, adjust, adjustIndent, nil
	}

	// Failed, and out of options.
	return nil, nil, 0, err
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
