// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"archive/zip"
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var workDir string

var (
	testwork     = flag.Bool("testwork", false, "if true, work directory will be preserved")
	updateGolden = flag.Bool("updategolden", false, "if true, files in testdata/golden will be updated")
)

func TestMain(m *testing.M) {
	status := 1
	defer func() {
		if !*testwork && workDir != "" {
			os.RemoveAll(workDir)
		}
		os.Exit(status)
	}()

	flag.Parse()

	var err error
	workDir, err = ioutil.TempDir("", "")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	if *testwork {
		fmt.Fprintf(os.Stderr, "test work dir: %s\n", workDir)
	}

	repoZipDir := filepath.Join("testdata", "repos")
	infos, err := ioutil.ReadDir(repoZipDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	for _, info := range infos {
		name := info.Name()
		if !info.IsDir() && filepath.Ext(name) == ".zip" {
			if err := extractZip(workDir, filepath.Join(repoZipDir, name)); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
		}
	}

	status = m.Run()
}

func TestRelease(t *testing.T) {
	infos, err := ioutil.ReadDir(filepath.FromSlash("testdata/golden"))
	if err != nil {
		t.Fatal(err)
	}
	testNames := make([]string, 0, len(infos))
	for _, info := range infos {
		if name := info.Name(); filepath.Ext(name) == ".test" {
			testNames = append(testNames, name[:len(name)-len(".test")])
		}
	}

	for _, info := range infos {
		name := info.Name()
		if filepath.Ext(name) != ".test" {
			continue
		}
		t.Run(name[:len(name)-len(".test")], func(t *testing.T) {
			// Read the test file, and find a line that contains "---".
			// Above this are key=value configuration settings.
			// Below this is the expected output.
			testPath := filepath.Join("testdata", "golden", name)
			f, err := os.OpenFile(testPath, os.O_RDWR, 0666)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := f.Close(); err != nil && *updateGolden {
					t.Fatalf("error closing golden file: %v", err)
				}
			}()

			data, err := ioutil.ReadAll(f)
			if err != nil {
				t.Fatal(err)
			}
			sep := []byte("\n---\n")
			var configData, want []byte
			sepOffset := bytes.Index(data, sep)
			if sepOffset < 0 {
				t.Fatal("could not find separator")
			} else {
				configData = data[:sepOffset]
				want = bytes.TrimSpace(data[sepOffset+len(sep):])
			}

			var repo, oldVersion, newVersion string
			for lineNum, line := range bytes.Split(configData, []byte("\n")) {
				line = bytes.TrimSpace(line)
				if len(line) == 0 {
					continue
				}
				var key, value string
				if i := bytes.IndexByte(line, '='); i < 0 {
					t.Fatalf("line %d: no '=' found", lineNum+1)
				} else {
					key = string(line[:i])
					value = string(line[i+1:])
				}
				switch key {
				case "repo":
					repo = value
				case "oldVersion":
					oldVersion = value
				case "newVersion":
					newVersion = value
				default:
					t.Fatalf("line %d: unknown key: %q", lineNum, key)
				}
			}

			dir := filepath.Join(workDir, repo)
			r, err := makeReleaseReport(dir, oldVersion, newVersion)
			if err != nil {
				t.Fatal(err)
			}
			buf := &bytes.Buffer{}
			if err := r.Text(buf); err != nil {
				t.Fatal(err)
			}
			got := bytes.TrimSpace(buf.Bytes())
			if !bytes.Equal(got, want) {
				if *updateGolden {
					if err := f.Truncate(int64(sepOffset + len(sep))); err != nil {
						t.Fatalf("error truncating golden file: %v", err)
					}
					if _, err := f.Seek(0, 2); err != nil {
						t.Fatalf("error seeking golden file: %v", err)
					}
					if _, err := f.Write(got); err != nil {
						t.Fatalf("error writing golden file: %v", err)
					}
				} else {
					t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
				}
			}
		})
	}
}

func extractZip(destDir, zipPath string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()

	extractFile := func(f *zip.File) (err error) {
		outPath := filepath.Join(destDir, f.Name)
		if strings.HasSuffix(f.Name, "/") {
			return os.MkdirAll(outPath, 0777)
		}
		if err := os.MkdirAll(filepath.Dir(outPath), 0777); err != nil {
			return err
		}
		r, err := f.Open()
		if err != nil {
			return err
		}
		defer r.Close()
		w, err := os.Create(outPath)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := w.Close(); err == nil && cerr != nil {
				err = cerr
			}
		}()
		if _, err := io.Copy(w, r); err != nil {
			return err
		}
		return nil
	}

	for _, f := range zr.File {
		if err := extractFile(f); err != nil {
			return err
		}
	}
	return nil
}
