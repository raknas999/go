package modindex

import (
	"cmd/go/internal/base"
	"cmd/go/internal/fsys"
	"cmd/go/internal/par"
	"encoding/json"
	"errors"
	"fmt"
	"go/doc"
	"go/scanner"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
)

// indexModule indexes the module at the given directory and returns its
// encoded representation.
func indexModule(modroot string) ([]byte, error) {
	var packages []*rawPackage
	err := fsys.Walk(modroot, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return nil
		}
		// stop at module boundaries
		if modroot != path {
			if fi, err := fsys.Stat(filepath.Join(path, "go.mod")); err == nil && !fi.IsDir() {
				return filepath.SkipDir
			}
		}
		// TODO(matloob): what do we do about symlinks
		rel, err := filepath.Rel(modroot, path)
		if err != nil {
			panic(err)
		}
		packages = append(packages, importRaw(modroot, rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return encodeModule(packages)
}

// rawPackage holds the information from each package that's needed to
// fill a build.Package once the context is available.
type rawPackage struct {
	error string
	dir   string // directory containing package sources, relative to the module root

	// Source files
	sourceFiles []*rawFile
}

type parseError struct {
	ErrorList   *scanner.ErrorList
	ErrorString string
}

// parseErrorToString converts the error from parsing the file into a string
// representation. A nil error is converted to an empty string, and all other
// errors are converted to a JSON-marshalled parseError struct, with ErrorList
// set for errors of type scanner.ErrorList, and ErrorString set to the error's
// string representation for all other errors.
func parseErrorToString(err error) string {
	if err == nil {
		return ""
	}
	var p parseError
	if e, ok := err.(scanner.ErrorList); ok {
		p.ErrorList = &e
	} else {
		p.ErrorString = e.Error()
	}
	s, err := json.Marshal(p)
	if err != nil {
		panic(err) // This should be impossible because scanner.Error contains only strings and ints.
	}
	return string(s)
}

// parseErrorFrom string converts a string produced by parseErrorToString back
// to an error.  An empty string is converted to a nil error, and all
// other strings are expected to be JSON-marshalled parseError structs.
// The two functions are meant to preserve the structure of an
// error of type scanner.ErrorList in a round trip, but may not preserve the
// structure of other errors.
func parseErrorFromString(s string) error {
	if s == "" {
		return nil
	}
	var p parseError
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		base.Fatalf(`go: invalid parse error value in index: %q. This indicates a corrupted index. Run "go clean -cache" to reset the module cache.`, s)
	}
	if p.ErrorList != nil {
		return *p.ErrorList
	}
	return errors.New(p.ErrorString)
}

// rawFile is the struct representation of the file holding all
// information in its fields.
type rawFile struct {
	error      string
	parseError string

	name                 string
	synopsis             string // doc.Synopsis of package comment... Compute synopsis on all of these?
	pkgName              string
	ignoreFile           bool   // starts with _ or . or should otherwise always be ignored
	binaryOnly           bool   // cannot be rebuilt from source (has //go:binary-only-package comment)
	cgoDirectives        string // the #cgo directive lines in the comment on import "C"
	goBuildConstraint    string
	plusBuildConstraints []string
	imports              []rawImport
	embeds               []embed
}

type rawImport struct {
	path     string
	position token.Position
}

type embed struct {
	pattern  string
	position token.Position
}

var pkgcache par.Cache // for packages not in modcache

// importRaw fills the rawPackage from the package files in srcDir.
// dir is the package's path relative to the modroot.
func importRaw(modroot, reldir string) *rawPackage {
	p := &rawPackage{
		dir: reldir,
	}

	absdir := filepath.Join(modroot, reldir)

	// We still haven't checked
	// that p.dir directory exists. This is the right time to do that check.
	// We can't do it earlier, because we want to gather partial information for the
	// non-nil *Package returned when an error occurs.
	// We need to do this before we return early on FindOnly flag.
	if !isDir(absdir) {
		// package was not found
		p.error = fmt.Errorf("cannot find package in:\n\t%s", absdir).Error()
		return p
	}

	entries, err := fsys.ReadDir(absdir)
	if err != nil {
		p.error = err.Error()
		return p
	}

	fset := token.NewFileSet()
	for _, d := range entries {
		if d.IsDir() {
			continue
		}
		if d.Mode()&fs.ModeSymlink != 0 {
			if isDir(filepath.Join(absdir, d.Name())) {
				// Symlinks to directories are not source files.
				continue
			}
		}

		name := d.Name()
		ext := nameExt(name)

		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		info, err := getFileInfo(absdir, name, fset)
		if err != nil {
			p.sourceFiles = append(p.sourceFiles, &rawFile{name: name, error: err.Error()})
			continue
		} else if info == nil {
			p.sourceFiles = append(p.sourceFiles, &rawFile{name: name, ignoreFile: true})
			continue
		}
		rf := &rawFile{
			name:                 name,
			goBuildConstraint:    info.goBuildConstraint,
			plusBuildConstraints: info.plusBuildConstraints,
			binaryOnly:           info.binaryOnly,
		}
		if info.parsed != nil {
			rf.pkgName = info.parsed.Name.Name
		}

		// Going to save the file. For non-Go files, can stop here.
		p.sourceFiles = append(p.sourceFiles, rf)
		if ext != ".go" {
			continue
		}

		if info.parseErr != nil {
			rf.parseError = parseErrorToString(info.parseErr)
			// Fall through: we might still have a partial AST in info.Parsed,
			// and we want to list files with parse errors anyway.
		}

		if info.parsed != nil && info.parsed.Doc != nil {
			rf.synopsis = doc.Synopsis(info.parsed.Doc.Text())
		}

		var cgoDirectives []string
		for _, imp := range info.imports {
			if imp.path == "C" {
				cgoDirectives = append(cgoDirectives, extractCgoDirectives(imp.doc.Text())...)
			}
			rf.imports = append(rf.imports, rawImport{path: imp.path, position: fset.Position(imp.pos)})
		}
		rf.cgoDirectives = strings.Join(cgoDirectives, "\n")
		for _, emb := range info.embeds {
			rf.embeds = append(rf.embeds, embed{emb.pattern, emb.pos})
		}

	}
	return p
}

// extractCgoDirectives filters only the lines containing #cgo directives from the input,
// which is the comment on import "C".
func extractCgoDirectives(doc string) []string {
	var out []string
	for _, line := range strings.Split(doc, "\n") {
		// Line is
		//	#cgo [GOOS/GOARCH...] LDFLAGS: stuff
		//
		line = strings.TrimSpace(line)
		if len(line) < 5 || line[:4] != "#cgo" || (line[4] != ' ' && line[4] != '\t') {
			continue
		}

		out = append(out, line)
	}
	return out
}
