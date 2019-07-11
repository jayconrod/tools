// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/cmd/gorelease/internal/base"
	"golang.org/x/tools/cmd/gorelease/internal/cfg"
	"golang.org/x/tools/cmd/gorelease/internal/codehost"
	"golang.org/x/tools/cmd/gorelease/internal/fakemodfetch"
	"golang.org/x/tools/cmd/gorelease/internal/modfile"
	"golang.org/x/tools/cmd/gorelease/internal/str"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/internal/apidiff"
)

// TODO:
// * Rename "old" to "base".
// * Base version should be any tag, branch, or commit. We should not require
//   submodule prefixes for tags that look like semantic versions, but we
//   also should not automatically use semantic versions from the root module.
// * Rename "new" to "version". When specified, we check whether that version
//   is valid.
// * Tolerate not having a go.mod file.
// * Print tag, including submodule prefix.
// * Positional arguments should specify which packages to check. Without
//   these, we check all non-internal packages in the module.

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

	CmdRelease.Flag.Parse(os.Args[1:])
	CmdRelease.Run(CmdRelease, CmdRelease.Flag.Args())
	base.Exit()
}

func initEnv() {
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
	wd, err := os.Getwd()
	if err != nil {
		base.Fatalf("go release: %v", err)
	}
	report, err := makeReleaseReport(wd, *oldVersion, *newVersion)
	if err != nil {
		base.Fatalf("go release: %v", err)
	}
	if err := report.Text(os.Stdout); err != nil {
		base.Fatalf("go release: %v", err)
	}
}

func makeReleaseReport(dir, oldVersion, newVersion string) (Report, error) {
	if oldVersion == newVersion {
		return Report{}, errors.New("-old and -new must be different versions")
	}

	// Locate the module root and repository root directories.
	modRoot := findModuleRoot(dir)
	if modRoot == "" {
		return Report{}, fmt.Errorf("could not find go.mod in any parent directory of %s", dir)
	}
	repoRoot, err := findRepoRoot(dir)
	if err != nil {
		return Report{}, err
	}

	if !str.HasFilePathPrefix(modRoot, repoRoot) {
		return Report{}, fmt.Errorf("module directory %q is not in repository root directory %q", modRoot, repoRoot)
	}
	subdir := ""
	if modRoot != repoRoot {
		subdir = filepath.ToSlash(modRoot[len(repoRoot)+1:])
	}
	if subdir != "" {
		// TODO: implement
		return Report{}, errors.New("submodules not implemented")
	}

	// Read the module path from the go.mod file.
	// Determine the module path for the repository root.
	goModPath := filepath.Join(modRoot, "go.mod")
	modData, err := ioutil.ReadFile(goModPath)
	if err != nil {
		return Report{}, err
	}
	modFile, err := modfile.ParseLax(goModPath, modData, nil)
	if err != nil {
		return Report{}, err
	}
	if modFile.Module == nil {
		return Report{}, fmt.Errorf("no module statement in %s", goModPath)
	}
	modPath := modFile.Module.Mod.Path
	codeRoot := modPath

	// Check out the old and new versions to temporary directories.
	code, err := codehost.LocalGitRepo(filepath.Join(repoRoot, ".git"))
	if err != nil {
		return Report{}, err
	}
	repo, err := fakemodfetch.NewCodeRepo(code, codeRoot, modPath)
	if err != nil {
		return Report{}, err
	}

	scratchDir, err := ioutil.TempDir("", "gorelease-")
	if err != nil {
		return Report{}, err
	}
	defer os.RemoveAll(scratchDir)

	oldPkgs, err := checkoutAndLoad(repo, oldVersion, scratchDir)
	if err != nil {
		return Report{}, err
	}
	newPkgs, err := checkoutAndLoad(repo, newVersion, scratchDir)
	if err != nil {
		return Report{}, err
	}

	// Compare each pair of packages.
	oldIndex, newIndex := 0, 0
	r := Report{
		OldVersion: oldVersion,
		NewVersion: newVersion,
	}
	for oldIndex < len(oldPkgs) || newIndex < len(newPkgs) {
		if oldIndex < len(oldPkgs) && (newIndex == len(newPkgs) || oldPkgs[oldIndex].PkgPath < newPkgs[newIndex].PkgPath) {
			r.Packages = append(r.Packages, PackageReport{
				Path:      oldPkgs[oldIndex].PkgPath,
				OldErrors: oldPkgs[oldIndex].Errors,
				Report: apidiff.Report{
					Changes: []apidiff.Change{{
						Message:    "package removed",
						Compatible: false,
					}},
				},
			})
			oldIndex++
		} else if newIndex < len(newPkgs) && (oldIndex == len(oldPkgs) || newPkgs[newIndex].PkgPath < oldPkgs[oldIndex].PkgPath) {
			r.Packages = append(r.Packages, PackageReport{
				Path:      newPkgs[newIndex].PkgPath,
				NewErrors: newPkgs[newIndex].Errors,
				Report: apidiff.Report{
					Changes: []apidiff.Change{{
						Message:    "package added",
						Compatible: true,
					}},
				},
			})
			newIndex++
		} else {
			oldPkg := oldPkgs[oldIndex]
			newPkg := newPkgs[newIndex]
			pr := PackageReport{
				Path:      oldPkg.PkgPath,
				OldErrors: oldPkg.Errors,
				NewErrors: newPkg.Errors,
			}
			if len(oldPkg.Errors) == 0 && len(newPkg.Errors) == 0 {
				pr.Report = apidiff.Changes(oldPkg.Types, newPkg.Types)
			}
			r.Packages = append(r.Packages, pr)
			oldIndex++
			newIndex++
		}
	}

	return r, nil
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

func checkoutAndLoad(repo fakemodfetch.Repo, version, scratchDir string) ([]*packages.Package, error) {
	// TODO: ensure a go.mod is present, even if one was not present
	// in the original version. Without this, we won't be able to load packages.
	dir, err := fakemodfetch.Checkout(repo, version, scratchDir)
	if err != nil {
		return nil, err
	}

	loadMode := packages.NeedName | packages.NeedTypes | packages.NeedImports | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedTypesSizes
	cfg := &packages.Config{
		Mode: loadMode,
		Dir:  dir,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, err
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].PkgPath < pkgs[j].PkgPath })

	// Trim scratchDir from file paths in errors.
	prefix := dir + string(os.PathSeparator)
	for _, pkg := range pkgs {
		for i := range pkg.Errors {
			pos := pkg.Errors[i].Pos
			if j := strings.IndexByte(pos, ':'); j >= 0 {
				file := pos[:j]
				if strings.HasPrefix(file, prefix) {
					pkg.Errors[i].Pos = file[len(prefix):] + pos[j:]
				}
			}
		}
	}

	return pkgs, nil
}

type Report struct {
	Packages               []PackageReport
	OldVersion, NewVersion string
}

func (r *Report) Text(w io.Writer) error {
	for _, p := range r.Packages {
		if err := p.Text(w); err != nil {
			return err
		}
	}
	return nil
}

type PackageReport struct {
	apidiff.Report
	Path                 string
	OldErrors, NewErrors []packages.Error
}

func (p *PackageReport) Text(w io.Writer) error {
	if len(p.Changes) == 0 && len(p.OldErrors) == 0 && len(p.NewErrors) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "%s\n%s\n", p.Path, strings.Repeat("-", len(p.Path))); err != nil {
		return err
	}
	if len(p.OldErrors) > 0 {
		if _, err := fmt.Fprintf(w, "errors in old version:\n"); err != nil {
			return err
		}
		for _, e := range p.OldErrors {
			if _, err := fmt.Fprintf(w, "\t%v\n", e); err != nil {
				return err
			}
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			return err
		}
	}
	if len(p.NewErrors) > 0 {
		if _, err := fmt.Fprintf(w, "errors in new version:\n"); err != nil {
			return err
		}
		for _, e := range p.NewErrors {
			if _, err := fmt.Fprintf(w, "\t%v\n", e); err != nil {
				return err
			}
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			return err
		}
	}
	if len(p.Changes) > 0 {
		if err := p.Report.Text(w); err != nil {
			return err
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			return err
		}
	}
	return nil
}
