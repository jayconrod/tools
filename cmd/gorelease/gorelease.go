// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// gorelease is an experimental tool that helps module authors avoid common
// problems before releasing a new version of a module.
//
// gorelease is intended to eventually be merged into the go command
// as "go release". See golang.org/issues/26420.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

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
// Before pushing to x/tools
// * Special message if release version does not start with 'v'.
// * Audit code and documentation.
// * Audit test coverage.
// * Audit TODOs and skipped tests.
// * Audit error messages.
// * Restrict to go1.13.
// * Try on more real world examples.
//
// After pushing to x/tools
// * Packages import from earlier major version of same module.
// * Check that proposed prerelease will not sort below pseudo-versions.
// * First version of nested module.
// * Error messages point to HTML documentation.
// * Positional arguments should specify which packages to check. Without
//   these, we check all non-internal packages in the module.
// * Nested module doesn't require parent.
// * Mechanism to suppress error messages.
// * Support for other VCS tools.

var CmdRelease = &base.Command{
	UsageLine: "gorelease [-base version] [-version version]",
	Short:     "Check for common problems before releasing a new version of a module",
	Long: `
gorelease is an experimental tool that helps module authors avoid common
problems before releasing a new version of a module.

gorelease suggests a new version tag that satisfies semantic versioning
requirements by comparing the public API of a module at two revisions:
a base version, and the currently checked out revision. The base version
may be determined automatically as the most recent version tag on the
current branch, or it may be specified explicitly with the -base flag.

If there are no differences in the module's public API, gorelease will suggest
a new version that increments the base version's patch version number.
For example, if the base version is "v2.3.1", gorelease would suggest
"v2.3.2" as the new version. If there are only compatible differences
in the module's public API, gorelease will suggest a new version that
increments the base versions's minor version number. For example,
if the base version is "v2.3.1", gorelease will suggest "v2.4.0". If there
are incompatible differences, gorelease will exit with a non-zero status.
Incompatible differences may only be released in a new major version,
which involves creating a module with a different path. For example,
if incompatible changes are made in the module "rsc.io/quote", a new major
version must be released as a new module, "rsc.io/quote/v2".

If the -version flag is given, gorelease will validate the proposed version
instead of suggesting a new version. For example, if the base version is
"v2.3.1", and the proposed version is "v2.3.2", and there are compatible
changes in the module's API, gorelease will exit with a non-zero status
since the minor version number was not incremented.

gorelease accepts the following flags:

	-base version
		The base version that the currently checked out revision will be compared
		against. The version must be a semantic version (for example, "v2.3.4").
	-version version
		The proposed version to be released. If specified, gorelease will
		confirm whether this is a valid semantic version, given changes that are
		made in the module's public API. gorelease will exit with a non-zero
		status if the version is not valid.

gorelease is intended to eventually be merged into the go command
as "go release". See golang.org/issues/26420.
`,
}

var (
	baseVersion    = CmdRelease.Flag.String("base", "", "base version of the module to compare")
	releaseVersion = CmdRelease.Flag.String("version", "", "proposed version of the module.")
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
		base.Fatalf("gorelease: no arguments allowed")
	}
	wd, err := os.Getwd()
	if err != nil {
		base.Fatalf("gorelease: %v", err)
	}
	report, err := makeReleaseReport(wd, *baseVersion, *releaseVersion)
	if err != nil {
		base.Fatalf("gorelease: %v", err)
	}
	if err := report.Text(os.Stdout); err != nil {
		base.Fatalf("gorelease: %v", err)
	}
	if !report.isSuccessful() {
		base.SetExitStatus(1)
	}
}

func makeReleaseReport(dir, baseVersion, releaseVersion string) (report, error) {
	if baseVersion != "" {
		if canonical := semver.Canonical(baseVersion); canonical != baseVersion {
			return report{}, fmt.Errorf("-base version %q is not a canonical semantic version", baseVersion)
		}
	}
	if baseVersion != "" && releaseVersion != "" {
		if cmp := semver.Compare(baseVersion, releaseVersion); cmp == 0 {
			return report{}, errors.New("-base and -version must be different versions")
		} else if cmp > 0 {
			return report{}, errors.New("-base must be older than -version")
		}
	}

	// Locate the module root and repository root directories.
	modRoot := findModuleRoot(dir)
	if modRoot == "" {
		return report{}, fmt.Errorf("could not find go.mod in any parent directory of %s", dir)
	}
	repoRoot, err := findRepoRoot(dir)
	if err != nil {
		return report{}, err
	}
	if err := repoHasPendingChanges(repoRoot); err != nil {
		return report{}, err
	}

	if !str.HasFilePathPrefix(modRoot, repoRoot) {
		return report{}, fmt.Errorf("module directory %q is not in repository root directory %q", modRoot, repoRoot)
	}

	// Read the module path from the go.mod file.
	goModPath := filepath.Join(modRoot, "go.mod")
	modData, err := ioutil.ReadFile(goModPath)
	if err != nil {
		return report{}, err
	}
	modFile, err := modfile.ParseLax(goModPath, modData, nil)
	if err != nil {
		return report{}, err
	}
	if modFile.Module == nil {
		return report{}, fmt.Errorf("no module statement in %s", goModPath)
	}
	modPath := modFile.Module.Mod.Path
	if err := checkModPath(modPath); err != nil {
		return report{}, err
	}
	// TODO(jayconrod): check for invalid characters.
	modPrefix, modPathMajor, ok := module.SplitPathVersion(modPath)
	if !ok {
		return report{}, fmt.Errorf("%s: could not find version suffix in module path", modPath)
	}

	// Determine the module path prefix of the repository root (codeRoot)
	// and the version tag prefix of the current module (tagPrefix).
	// For example, if the current module is "github.com/a/b/c/v2" defined in
	// "c/v2/go.mod", codeRoot is "github.com/a/b", and tagPrefix is "c/".
	codeRoot := modPrefix
	tagPrefix := ""
	if modRoot != repoRoot {
		if strings.HasPrefix(modPathMajor, ".") {
			return report{}, fmt.Errorf("%s: module path starts with gopkg.in and must be declared in the root directory of the repository", modPath)
		}
		codeDir := filepath.ToSlash(modRoot[len(repoRoot)+1:])
		var suffix string
		if modPathMajor == "" || modPathMajor[0] != '/' {
			// module has no major version suffix or has a gopkg.in-style suffix.
			// codeDir must be a suffix of modPath
			// tagPrefix is codeDir with a trailing slash.
			if !strings.HasSuffix(modPath, "/"+codeDir) {
				return report{}, fmt.Errorf("%s: module path must end with %[2]q, since it is in subdirectory %[2]q", modPath, codeDir)
			}
			suffix = "/" + codeDir
			tagPrefix = codeDir + "/"
		} else {
			if strings.HasSuffix(modPath, "/"+codeDir) {
				// module has a major version suffix and is in a major version subdirectory.
				// codeDir must be a suffix of modPath.
				// tagPrefix must not include the major version.
				suffix = "/" + codeDir
				tagPrefix = codeDir[:len(codeDir)-len(modPathMajor)+1]
			} else if strings.HasSuffix(modPath, "/"+codeDir+modPathMajor) {
				// module has a major version suffix and is not in a major version subdirectory.
				// codeDir + modPathMajor is a suffix of modPath.
				// tagPrefix is codeDir with a trailing slash.
				suffix = "/" + codeDir + modPathMajor
				tagPrefix = codeDir + "/"
			} else {
				return report{}, fmt.Errorf("%s: module path must end with %[2]q or %q, since it is in subdirectory %[2]q", modPath, codeDir, codeDir+modPathMajor)
			}
		}
		codeRoot = modPath[:len(modPath)-len(suffix)]
	}
	// TODO(jayconrod): if the origin fully resolves the v2+ module path
	// as was the case for nanomsg.org/go/mangos/v2, codeRoot must include the
	// major version suffix, and major versions may not be in subdirectories.
	// This allows major versions to be in different repositories.

	// Initialize code host and repo. We use these to access revisions
	// in the local repository other than HEAD.
	// TODO(jayconrod): we set the repo directory to be the .git directory itself
	// since codehost generally expects a bare repository and does some weird
	// things in the parent directory like creating an info directory.
	// We add a trailing slash because codehost generates a lock file path by
	// appending ".lock" to the path, so we get a .git.lock file.
	code, err := codehost.LocalGitRepo(filepath.Join(repoRoot, ".git") + string(os.PathSeparator))
	if err != nil {
		return report{}, err
	}
	repo, err := fakemodfetch.NewCodeRepo(code, codeRoot, modPath)
	if err != nil {
		return report{}, err
	}

	// Auto-detect the base version if one wasn't specified.
	// Any checks that don't require comparing versions should be performed
	// before this point.
	shouldCompare := baseVersion != "" || !likelyFirstVersion(releaseVersion)
	if baseVersion == "" {
		var baseTag string
		if modPathMajor != "" {
			baseTag, err = code.RecentTag("HEAD", tagPrefix, modPathMajor[1:])
		} else {
			baseTag, err = code.RecentTag("HEAD", tagPrefix, "v1")
			if baseTag == "" || err != nil {
				baseTag, err = code.RecentTag("HEAD", tagPrefix, "v0")
			}
		}
		if baseTag != "" && err == nil {
			baseVersion = baseTag[len(tagPrefix):]
			if releaseVersion != "" {
				if cmp := semver.Compare(baseVersion, releaseVersion); cmp == 0 {
					return report{}, fmt.Errorf("detected base version %s is equal to release version.\nUse the -base flag to set the base version explicitly.", baseVersion)
				} else if cmp > 0 {
					return report{}, fmt.Errorf("detected base version %s is greater than release version %s.\nUse the -base flag to set the base version explicitly.", baseVersion, releaseVersion)
				}
			}
		} else if shouldCompare {
			// If we couldn't detect a base version, only report an error if
			// releaseVersion looks like it's not the first version for this module.
			if err != nil {
				return report{}, fmt.Errorf("could not detect base vesion: %v", err)
			}
			if baseTag == "" {
				return report{}, fmt.Errorf("could not detect base version.\nUse the -base flag to set it explicitly.")
			}
		}
	}

	// Check out the old and new versions to temporary directories.
	scratchDir, err := ioutil.TempDir("", "gorelease-")
	if err != nil {
		return report{}, err
	}
	defer os.RemoveAll(scratchDir)

	newPkgs, diagnostics, err := checkoutAndLoad(repo, "HEAD", nil, scratchDir)
	if err != nil {
		return report{}, err
	}
	var oldPkgs []*packages.Package
	if shouldCompare {
		oldPkgs, _, err = checkoutAndLoad(repo, baseVersion, modData, scratchDir)
		if err != nil {
			return report{}, err
		}
	}

	// Compare each pair of packages.
	// Ignore internal packages.
	// If we don't have a base version to compare against, just check the new
	// packages for errors.
	isInternal := func(pkgPath string) bool {
		if !str.HasPathPrefix(pkgPath, modPath) {
			panic(fmt.Sprintf("package %s not in module %s", pkgPath, modPath))
		}
		for pkgPath != modPath {
			if path.Base(pkgPath) == "internal" {
				return true
			}
			pkgPath = path.Dir(pkgPath)
		}
		return false
	}
	oldIndex, newIndex := 0, 0
	r := report{
		modulePath:     modPath,
		baseVersion:    baseVersion,
		releaseVersion: releaseVersion,
		tagPrefix:      tagPrefix,
		diagnostics:    diagnostics,
	}
	for oldIndex < len(oldPkgs) || newIndex < len(newPkgs) {
		if oldIndex < len(oldPkgs) && (newIndex == len(newPkgs) || oldPkgs[oldIndex].PkgPath < newPkgs[newIndex].PkgPath) {
			oldPkg := oldPkgs[oldIndex]
			oldIndex++
			if !isInternal(oldPkg.PkgPath) || len(oldPkg.Errors) > 0 {
				pr := PackageReport{
					Path:      oldPkg.PkgPath,
					OldErrors: oldPkg.Errors,
				}
				if !isInternal(oldPkg.PkgPath) {
					pr.Report = apidiff.Report{
						Changes: []apidiff.Change{{
							Message:    "package removed",
							Compatible: false,
						}},
					}
				}
				r.addPackage(pr)
			}
		} else if newIndex < len(newPkgs) && (oldIndex == len(oldPkgs) || newPkgs[newIndex].PkgPath < oldPkgs[oldIndex].PkgPath) {
			newPkg := newPkgs[newIndex]
			newIndex++
			if isInternal(newPkg.PkgPath) && len(newPkg.Errors) == 0 && !shouldCompare {
				// If we aren't comparing against a base version, don't say
				// "package added". Only report packages with errors.
				continue
			}
			pr := PackageReport{
				Path:      newPkg.PkgPath,
				NewErrors: newPkg.Errors,
			}
			if !isInternal(newPkg.PkgPath) && shouldCompare {
				pr.Report = apidiff.Report{
					Changes: []apidiff.Change{{
						Message:    "package added",
						Compatible: true,
					}},
				}
			}
			r.addPackage(pr)
		} else {
			oldPkg := oldPkgs[oldIndex]
			newPkg := newPkgs[newIndex]
			oldIndex++
			newIndex++
			if !isInternal(oldPkg.PkgPath) {
				pr := PackageReport{
					Path:      oldPkg.PkgPath,
					OldErrors: oldPkg.Errors,
					NewErrors: newPkg.Errors,
				}
				if len(oldPkg.Errors) == 0 && len(newPkg.Errors) == 0 {
					pr.Report = apidiff.Changes(oldPkg.Types, newPkg.Types)
				}
				r.addPackage(pr)
			}
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

// returns whether there are pending changes in the repository rooted at
// the given directory.
// TODO: generalize to version control systems other than git.
func repoHasPendingChanges(root string) error {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = root
	if out, err := cmd.Output(); err != nil {
		return fmt.Errorf("could not determine if there were uncommitted changes in the current repository: %v", err)
	} else if len(out) > 0 {
		return errors.New("there are uncommitted changes in the current repository")
	}
	return nil
}

// checkModPath is like golang.org/x/mod/module.CheckPath, but it returns
// friendlier error messages for common mistakes.
//
// TODO(jayconrod): update module.CheckPath and delete this function.
func checkModPath(modPath string) error {
	if path.IsAbs(modPath) || filepath.IsAbs(modPath) {
		// TODO(jayconrod): improve error message in x/mod instead of checking here.
		return fmt.Errorf("module path %q may not be an absolute path.\nIt must be an address where your module may be found.", modPath)
	}
	if suffix := dirMajorSuffix(modPath); suffix == "v0" || suffix == "v1" {
		return fmt.Errorf("module path %q has major version suffix %q.\nA major version suffix is only allowed for v2 or later.", modPath, suffix)
	} else if strings.HasPrefix(suffix, "v0") {
		return fmt.Errorf("module path %q has major version suffix %q.\nA major version may not have a leading zero.", modPath, suffix)
	} else if strings.ContainsRune(suffix, '.') {
		return fmt.Errorf("module path %q has major version suffix %q.\nA major version may not contain dots.", modPath, suffix)
	}
	return module.CheckPath(modPath)
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

// likelyFirstVersion returns whether vers is likely the first version for
// a given major version.
func likelyFirstVersion(vers string) bool {
	_, minor, patch, err := splitVersionNumbers(vers)
	if err != nil {
		return false
	}
	return minor == "0" && patch == "0" || vers == "v0.1.0" || vers == "v0.0.1"
}

// splitVersionNumbers returns the major, minor, and patch numbers for a given
// version.
//
// TODO(jayconrod): extend semver to do this and delete this function.
func splitVersionNumbers(vers string) (major, minor, patch string, err error) {
	if !strings.HasPrefix(vers, "v") {
		return "", "", "", fmt.Errorf("version %q does not start with 'v'", vers)
	}
	base := vers[1:]
	if i := strings.IndexByte(vers, '-'); i >= 0 {
		base = base[:i] // trim prerelease
	}
	if i := strings.IndexByte(vers, '+'); i >= 0 {
		base = base[:i] // trim build
	}
	parts := strings.Split(base, ".")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("version %q should have three numbers", vers)
	}
	return parts[0], parts[1], parts[2], nil
}

// checkoutAndLoad extracts a specific revision of a module to a temporary
// directory, then loads type information for packages within the module.
//
// repo is an interface to access the module.
//
// rev is the revision to check out.
//
// goMod is the contents of the go.mod file at the release revision (HEAD).
// If rev is the release revision, goMod should be nil. Otherwise, if a go.mod
// file is not present, one will be written with these contents. This lets us
// load packages with similar versions of dependencies (as opposed to the
// latest version of everything). However, missing modules will be added at
// their latest versions, which may upgrade other dependencies.
//
// scratchDir is a temporary directory. checkoutAndLoad will check out the
// source to a subdirectory named after rev. The caller is responsible for
// deleting scratchDir, even when an error occurs.
func checkoutAndLoad(repo fakemodfetch.Repo, rev string, goMod []byte, scratchDir string) (pkgs []*packages.Package, diagnostics []string, err error) {
	// TODO: ensure a go.mod is present, even if one was not present
	// in the original version. Without this, we won't be able to load packages.
	dir, err := fakemodfetch.Checkout(repo, rev, scratchDir)
	if err != nil {
		return nil, nil, err
	}

	// Verify or write go.mod, depending on what version this is.
	goModPath := filepath.Join(dir, "go.mod")
	goSumPath := filepath.Join(dir, "go.sum")
	var origGoMod, origGoSum []byte
	var haveOrigGoSum bool
	if goMod != nil {
		// goMod != nil indicates this is the base version.
		if _, err := os.Stat(goModPath); os.IsNotExist(err) {
			if err := ioutil.WriteFile(goModPath, goMod, 0666); err != nil {
				return nil, nil, err
			}
		} else if err != nil {
			return nil, nil, err
		} else {
			// Check that the module path matches the expected path.
			goModData, err := ioutil.ReadFile(goModPath)
			if err != nil {
				return nil, nil, fmt.Errorf("could not read go.mod in revision %s: %v", rev, err)
			}
			modFile, err := modfile.ParseLax(goModPath, goModData, nil)
			if err != nil || modFile.Module == nil {
				return nil, nil, fmt.Errorf("could not parse go.mod in revision %s: %v", rev, err)
			}
			if modFile.Module.Mod.Path != repo.ModulePath() {
				return nil, nil, fmt.Errorf("module path changed in go.mod\nfrom: %s (at revision %s)\n  to: %s", modFile.Module.Mod.Path, rev, repo.ModulePath())
			}
		}
	} else {
		// goMod == nil indicates this is the release version.
		// Load go.mod and go.sum so we can compare them later.
		// go.sum may not exist if the module doesn't depend on other modules.
		origGoMod, err = ioutil.ReadFile(goModPath)
		if err != nil {
			return nil, nil, fmt.Errorf("could not read go.mod in revision %s: %v", rev, err)
		}
		goSumPath := filepath.Join(dir, "go.sum")
		origGoSum, err = ioutil.ReadFile(goSumPath)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, nil, fmt.Errorf("could not read go.sum in revision %s: %v", rev, err)
			}
		} else {
			haveOrigGoSum = true
		}
	}

	// Load all packages in the module and transitive dependencies.
	loadMode := packages.NeedName | packages.NeedTypes | packages.NeedImports | packages.NeedDeps
	cfg := &packages.Config{
		Mode: loadMode,
		Dir:  dir,
	}
	pkgs, err = packages.Load(cfg, "./...")
	if err != nil {
		return nil, nil, err
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

	// If this is the release, version, check that loading packages did not modify
	// go.mod or go.sum.
	if origGoMod != nil {
		var goModUntidy bool
		newGoMod, err := ioutil.ReadFile(goModPath)
		if err != nil {
			return nil, nil, fmt.Errorf("could not read go.mod in revision %s: %v", rev, err)
		}
		if !bytes.Equal(origGoMod, newGoMod) {
			goModUntidy = true
			diagnostics = append(diagnostics, "go.mod is not tidy. Run 'go mod tidy'.")
		}

		newGoSum, err := ioutil.ReadFile(goSumPath)
		if err != nil {
			if haveOrigGoSum || !os.IsNotExist(err) {
				return nil, nil, fmt.Errorf("could not read go.sum in revision %s: %v", rev, err)
			}
		} else if !haveOrigGoSum {
			diagnostics = append(diagnostics, "go.sum is not committed to version control.")
		} else if !bytes.Equal(origGoSum, newGoSum) && !goModUntidy {
			diagnostics = append(diagnostics, "go.sum is missing one or more hashes. Run 'go mod tidy'.")
		}
	}

	return pkgs, diagnostics, nil
}

type report struct {
	modulePath                                                 string
	baseVersion, releaseVersion, tagPrefix                     string
	packages                                                   []PackageReport
	diagnostics                                                []string
	haveCompatibleChanges, haveIncompatibleChanges, haveErrors bool
}

// Text formats and writes a report to w. The report lists error, compatible
// and incompatible changes in each package. If releaseVersion is set, it
// states whether releaseVersion is valid (and why). If releaseVersion is not
// set, it suggests a new version.
func (r *report) Text(w io.Writer) error {
	for _, p := range r.packages {
		if err := p.Text(w); err != nil {
			return err
		}
	}

	var summary string
	if len(r.diagnostics) > 0 {
		summary = strings.Join(r.diagnostics, "\n")
	} else if r.releaseVersion != "" {
		if err := r.validateVersion(); err != nil {
			summary = err.Error()
		} else {
			if r.tagPrefix == "" {
				summary = fmt.Sprintf("%s is a valid semantic version for this release.", r.releaseVersion)
			} else {
				summary = fmt.Sprintf("%[2]s (with tag %[1]s%[2]s) is a valid semantic version for this release", r.tagPrefix, r.releaseVersion)
			}
		}
	} else if r.haveErrors {
		summary = "Errors were detected, so no version will be suggested."
	} else if r.haveIncompatibleChanges && r.baseVersion != "" && semver.Major(r.baseVersion) != "v0" {
		suggestedVersion := r.suggestVersion()
		summary = fmt.Sprintf(`Incompatible changes detected, so no version will be suggested.
Use -release=%s to verify a new major version.
Avoid creating new major versions if possible though.
`, suggestedVersion)
		// TODO(jayconrod): link to documentation on releasing major versions
	} else {
		suggestedVersion := r.suggestVersion()
		if r.tagPrefix == "" {
			summary = fmt.Sprintf("Suggested version: %s", suggestedVersion)
		} else {
			summary = fmt.Sprintf("Suggested version: %[2]s (with tag %[1]s%[2]s)", r.tagPrefix, suggestedVersion)
		}
	}
	if _, err := fmt.Fprintln(w, summary); err != nil {
		return err
	}

	return nil
}

func (r *report) addPackage(p PackageReport) {
	r.packages = append(r.packages, p)
	for _, c := range p.Changes {
		if c.Compatible {
			r.haveCompatibleChanges = true
		} else {
			r.haveIncompatibleChanges = true
		}
	}
	if len(p.NewErrors) > 0 {
		r.haveErrors = true
	}
}

// validateVersion checks whether r.releaseVersion is valid.
// If r.releaseVersion is not valid, an error is returned explaining why.
// r.releaseVersion must be set.
func (r *report) validateVersion() error {
	if r.releaseVersion == "" {
		panic("validateVersion called without version")
	}
	if r.haveErrors {
		return fmt.Errorf(`%s is not a valid semantic version for this release.
Errors were found in one or more packages.`, r.releaseVersion)
	}

	// TODO(jayconrod): link to documentation for all of these errors.

	// Check that the major version matches the module path.
	_, suffix, ok := module.SplitPathVersion(r.modulePath)
	if !ok {
		return fmt.Errorf("%s: could not find version suffix in module path", r.modulePath)
	}
	if suffix != "" {
		if suffix[0] != '/' && suffix[0] != '.' {
			return fmt.Errorf("%s: unknown module path version suffix: %q", r.modulePath, suffix)
		}
		pathMajor := suffix[1:]
		major := semver.Major(r.releaseVersion)
		if pathMajor != major {
			return fmt.Errorf(`%s is not a valid semantic version for this release.
The major version %s does not match the major version suffix
in the module path: %s`, r.releaseVersion, r.modulePath, major)
		}
	} else if major := semver.Major(r.releaseVersion); major != "v0" && major != "v1" {
		return fmt.Errorf(`%s is not a valid semantic version for this release.
The module path does not end with the major version suffix /%s,
which is required for major versions v2 or greater.`, r.releaseVersion, major)
	}

	// Check that compatible / incompatible changes are consistent.
	if semver.Major(r.baseVersion) == "v0" {
		return nil
	}
	if r.haveIncompatibleChanges {
		return fmt.Errorf(`%s is not a valid semantic version for this release.
There are incompatible changes.`, r.releaseVersion)
	}
	if r.haveCompatibleChanges && semver.MajorMinor(r.baseVersion) == semver.MajorMinor(r.releaseVersion) {
		return fmt.Errorf(`%s is not a valid semantic version for this release.
There are compatible changes, but the major and minor version numbers
are the same as the base version %s.`, r.releaseVersion, r.baseVersion)
	}

	return nil
}

// suggestVersion suggests a new version consistent with observed changes.
func (r *report) suggestVersion() string {
	major, minor, patch, err := splitVersionNumbers(r.baseVersion)
	if err != nil {
		panic(fmt.Sprintf("could not parse base version: %v", err))
	}

	if r.haveIncompatibleChanges && major != "0" {
		major = incDecimal(major)
		minor = "0"
		patch = "0"
	} else if r.haveCompatibleChanges || (r.haveIncompatibleChanges && major == "0") {
		minor = incDecimal(minor)
		patch = "0"
	} else {
		patch = incDecimal(patch)
	}
	return fmt.Sprintf("v%s.%s.%s", major, minor, patch)
}

// isSuccessful returns whether observed changes are consistent with
// r.releaseVersion. If r.releaseVersion is set, isSuccessful tests whether
// r.validateVersion() returns an error. If r.releaseVersion is not set,
// isSuccessful returns true if there were no incompatible changes or if
// a new version could be released without changing the module path.
func (r *report) isSuccessful() bool {
	if r.haveErrors || len(r.diagnostics) > 0 {
		return false
	}
	if r.releaseVersion != "" {
		return r.validateVersion() == nil
	}
	return !r.haveIncompatibleChanges || semver.Major(r.baseVersion) == "v0"
}

// incDecimal returns the decimal string incremented by 1.
func incDecimal(decimal string) string {
	// Scan right to left turning 9s to 0s until you find a digit to increment.
	digits := []byte(decimal)
	i := len(digits) - 1
	for ; i >= 0 && digits[i] == '9'; i-- {
		digits[i] = '0'
	}
	if i >= 0 {
		digits[i]++
	} else {
		// digits is all zeros
		digits[0] = '1'
		digits = append(digits, '0')
	}
	return string(digits)
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