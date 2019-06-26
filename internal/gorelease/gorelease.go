// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/internal/apidiff"
	"golang.org/x/tools/internal/gorelease/internal/base"
	"golang.org/x/tools/internal/gorelease/internal/cfg"
	"golang.org/x/tools/internal/gorelease/internal/codehost"
	"golang.org/x/tools/internal/gorelease/internal/fakemodfetch"
	"golang.org/x/tools/internal/gorelease/internal/modfile"
	"golang.org/x/tools/internal/gorelease/internal/str"
)

var CmdRelease = &base.Command{
	UsageLine: "gorelease [-old version] [-new version]",
	Short:     "Check for common problems before releasing a new version of a module",
	Long: `
gorelease is an experimental tool that helps module authors avoid common
problems before releasing a new version of a module. It reports
API differences (both compatible and incompatible), and it warns about
other common mistakes (for example, tagged a v2.x.y version without a
/v2 suffix).

gorelease is intended to eventually be merged into the go command
as "go release". See golang.org/issues/26420.
`,
}

var (
	oldVersion = CmdRelease.Flag.String("old", "", "base version of the module to compare")
	newVersion = CmdRelease.Flag.String("new", "", "new version of the module to compare")
)

func init() {
	CmdRelease.Run = runRelease

	base.Go.Commands = []*base.Command{CmdRelease}
}

func main() {
	log.SetFlags(0)

	if len(os.Args) > 1 && (os.Args[1] == "help" || os.Args[1] == "-h" || os.Args[1] == "-help" || os.Args[1] == "--help") {
		printHelp()
		os.Exit(0)
	}

	// Set environment (GOOS, GOARCH, etc) explicitly.
	// In theory all the commands we invoke should have
	// the same default computation of these as we do,
	// but in practice there might be skew
	// This makes sure we all agree.
	cfg.OrigEnv = os.Environ()
	cfg.CmdEnv = mkenv()
	for _, env := range cfg.CmdEnv {
		if os.Getenv(env.Name) != env.Value {
			os.Setenv(env.Name, env.Value)
		}
	}

	cfg.ModulesEnabled = true

	CmdRelease.Flag.Parse(os.Args[1:])
	CmdRelease.Run(CmdRelease, CmdRelease.Flag.Args())
	base.Exit()
}

func runRelease(cmd *base.Command, args []string) {
	if len(args) != 0 {
		base.Fatalf("go release: no arguments allowed")
	}
	if *oldVersion == "" {
		base.Fatalf("go release: -old not set")
	}
	if *newVersion == "" {
		base.Fatalf("go release: -new not set")
	}
	if *oldVersion == *newVersion {
		base.Fatalf("go release: -old and -new must be different versions")
	}

	// Locate the module root and repository root directories.
	wd, err := os.Getwd()
	if err != nil {
		base.Fatalf("go release: %v", err)
	}
	modRoot := findModuleRoot(wd)
	if modRoot == "" {
		base.Fatalf("go release: could not find go.mod in any parent directory of %s", wd)
	}
	repoRoot, err := findRepoRoot(wd)
	if err != nil {
		base.Fatalf("go release: %v", err)
	}

	if !str.HasFilePathPrefix(modRoot, repoRoot) {
		base.Fatalf("go release: module root directory %q is not in repository root directory %q", modRoot, repoRoot)
	}
	subdir := ""
	if modRoot != repoRoot {
		subdir = filepath.ToSlash(modRoot[len(repoRoot)+1:])
	}
	if subdir != "" {
		// TODO: implement
		base.Fatalf("go release: submodules not implemented")
	}

	// Read the module path from the go.mod file.
	// Determine the module path for the repository root.
	goModPath := filepath.Join(modRoot, "go.mod")
	modData, err := ioutil.ReadFile(goModPath)
	if err != nil {
		base.Fatalf("go release: %v", err)
	}
	modFile, err := modfile.ParseLax(goModPath, modData, nil)
	if err != nil {
		base.Fatalf("go release: %v", err)
	}
	if modFile.Module == nil {
		base.Fatalf("go release: no module statement in %s", goModPath)
	}
	modPath := modFile.Module.Mod.Path
	codeRoot := modPath

	// Check out the old and new versions to temporary directories.
	code, err := codehost.LocalGitRepo(repoRoot)
	if err != nil {
		base.Fatalf("go release: %v", err)
	}
	repo, err := fakemodfetch.NewCodeRepo(code, codeRoot, modPath)
	if err != nil {
		base.Fatalf("go release: %v", err)
	}

	scratchDir, err := ioutil.TempDir("", "gorelease-")
	if err != nil {
		base.Fatalf("go release: %v", err)
	}
	defer os.RemoveAll(scratchDir)

	oldDir, err := fakemodfetch.Checkout(repo, *oldVersion, scratchDir)
	if err != nil {
		base.Fatalf("go release: %v", err)
	}
	newDir, err := fakemodfetch.Checkout(repo, *newVersion, scratchDir)
	if err != nil {
		base.Fatalf("go release: %v", err)
	}

	// Load packages from each version.
	loadMode := packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedTypesSizes
	oldCfg := &packages.Config{
		Mode: loadMode,
		Dir:  oldDir,
	}
	oldPkgs, err := packages.Load(oldCfg, "./...")
	if err != nil {
		base.Fatalf("go release: %v", err)
	}
	sort.Slice(oldPkgs, func(i, j int) bool { return oldPkgs[i].PkgPath < oldPkgs[j].PkgPath })
	newCfg := &packages.Config{
		Mode: loadMode,
		Dir:  newDir,
	}
	newPkgs, err := packages.Load(newCfg, "./...")
	if err != nil {
		base.Fatalf("go release: %v", err)
	}
	sort.Slice(newPkgs, func(i, j int) bool { return newPkgs[i].PkgPath < newPkgs[j].PkgPath })

	// Compare each pair of packages.
	oldIndex, newIndex := 0, 0
	var r report
	for oldIndex < len(oldPkgs) || newIndex < len(newPkgs) {
		if oldIndex < len(oldPkgs) && (newIndex == len(newPkgs) || oldPkgs[oldIndex].PkgPath < newPkgs[newIndex].PkgPath) {
			r.packages = append(r.packages, packageReport{
				path: oldPkgs[oldIndex].PkgPath,
				Report: apidiff.Report{
					Changes: []apidiff.Change{{
						Message:    fmt.Sprintf("%s: package removed", oldPkgs[oldIndex].PkgPath),
						Compatible: false,
					}},
				},
			})
			oldIndex++
		} else if newIndex < len(newPkgs) && (oldIndex == len(newPkgs) || newPkgs[newIndex].PkgPath < oldPkgs[oldIndex].PkgPath) {
			r.packages = append(r.packages, packageReport{
				path: newPkgs[newIndex].PkgPath,
				Report: apidiff.Report{
					Changes: []apidiff.Change{{
						Message:    fmt.Sprintf("%s: package added", newPkgs[newIndex].PkgPath),
						Compatible: true,
					}},
				},
			})
			newIndex++
		} else {
			oldPkg := oldPkgs[oldIndex]
			newPkg := newPkgs[newIndex]
			r.packages = append(r.packages, packageReport{
				path:   oldPkg.PkgPath,
				Report: apidiff.Changes(oldPkg.Types, newPkg.Types),
			})
			oldIndex++
			newIndex++
		}
	}

	r.Text(os.Stdout)
}

func printHelp() {
	fmt.Fprintf(os.Stderr, "usage: %s\n\n%s\n", CmdRelease.UsageLine, strings.TrimSpace(CmdRelease.Long))
}

func findRepoRoot(wd string) (string, error) {
	d := wd
	for {
		_, err := os.Stat(filepath.Join(d, ".git"))
		if err == nil {
			return d, nil
		} else if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "%#v\n", err)
			return "", fmt.Errorf("could not locate repository root for directory %s: %v", wd, err)
		}
		prev := d
		d = filepath.Dir(d)
		if d == prev {
			return "", fmt.Errorf("could not locate repository root for directory %s", wd)
		}
	}
}

// copied from cmd/go/internal/modload.findModuleRoot
func findModuleRoot(dir string) (root string) {
	dir = filepath.Clean(dir)

	// Look for enclosing go.mod.
	for {
		if fi, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !fi.IsDir() {
			return dir
		}
		d := filepath.Dir(dir)
		if d == dir {
			break
		}
		dir = d
	}
	return ""
}

// dirMajorSuffix returns a major version suffix for a slash-separated path.
// For example, for the path "foo/bar/v2", dirMajorSuffix would return "v2".
// If no major version suffix is found, "" is returned.
//
// dirMajorSuffix is less strict than module.SplitPathVersion so that incorrect
// suffixes like "v0", "v02", "v1.2" can be detected. It doesn't handle
// special cases for gopkg.in paths.
func dirMajorSuffix(path string) string {
	i := len(path)
	for i > 0 && ('0' <= path[i-1] && path[i-1] <= '9') || path[i-1] == '.' {
		i--
	}
	if i <= 1 || i == len(path) || path[i-1] != 'v' || (i > 1 && path[i-2] != '/') {
		return ""
	}
	return path[i-1:]
}

type report struct {
	packages []packageReport
}

func (r *report) Text(w io.Writer) error {
	for _, p := range r.packages {
		if len(p.Changes) == 0 {
			continue
		}
		if _, err := fmt.Fprintf(w, "%s\n%s\n", p.path, strings.Repeat("-", len(p.path))); err != nil {
			return err
		}
		if err := p.Text(w); err != nil {
			return err
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			return err
		}
	}
	return nil
}

type packageReport struct {
	apidiff.Report
	path string
}
