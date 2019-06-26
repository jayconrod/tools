// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package modfetch is a modified, partial copy of cmd/go/internal/modfetch.
// Its API is similar so that gorelease can be integrated easily into
// cmd/go in the future. However, it avoids a lot of complexity and
// dependencies of the real modfetch package.
package fakemodfetch

import (
	"io"
	"sort"
	"time"

	"golang.org/x/tools/internal/gorelease/internal/semver"
)

// A Repo represents a repository storing all versions of a single module.
// It must be safe for simultaneous use by multiple goroutines.
type Repo interface {
	// ModulePath returns the module path.
	ModulePath() string

	// Versions lists all known versions with the given prefix.
	// Pseudo-versions are not included.
	// Versions should be returned sorted in semver order
	// (implementations can use SortVersions).
	Versions(prefix string) (tags []string, err error)

	// Stat returns information about the revision rev.
	// A revision can be any identifier known to the underlying service:
	// commit hash, branch, tag, and so on.
	Stat(rev string) (*RevInfo, error)

	// Latest returns the latest revision on the default branch,
	// whatever that means in the underlying source code repository.
	// It is only used when there are no tagged versions.
	Latest() (*RevInfo, error)

	// GoMod returns the go.mod file for the given version.
	GoMod(version string) (data []byte, err error)

	// Zip writes a zip file for the given version to dst.
	Zip(dst io.Writer, version string) error
}

// A Rev describes a single revision in a module repository.
type RevInfo struct {
	Version string    // version string
	Time    time.Time // commit time

	// These fields are used for Stat of arbitrary rev,
	// but they are not recorded when talking about module versions.
	Name  string `json:"-"` // complete ID in underlying repository
	Short string `json:"-"` // shortened ID, for use in pseudo-version
}

func SortVersions(list []string) {
	sort.Slice(list, func(i, j int) bool {
		cmp := semver.Compare(list[i], list[j])
		if cmp != 0 {
			return cmp < 0
		}
		return list[i] < list[j]
	})
}
