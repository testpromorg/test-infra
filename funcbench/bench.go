// Copyright 2020 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/tools/benchmark/parse"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
)

var benchmarkerTimeout = 2 * time.Hour

// TODO: Add unit test.
type Benchmarker struct {
	logger Logger

	benchFunc      string
	isRaceEnabled  bool
	benchTime      time.Duration
	resultCacheDir string

	c    *commander
	repo *git.Repository
}

func newBenchmarker(logger Logger, env Environment, c *commander, benchTime time.Duration, resultCacheDir string) (*Benchmarker, error) {
	if _, err := c.exec("go", "get", "golang.org/x/tools/cmd/benchcmp"); err != nil {
		return nil, err
	}
	return &Benchmarker{
		logger:         logger,
		benchFunc:      env.BenchFunc(),
		isRaceEnabled:  env.IsRaceEnabled(),
		benchTime:      benchTime,
		c:              c,
		repo:           env.Repo(),
		resultCacheDir: resultCacheDir,
	}, nil
}

func (b *Benchmarker) benchOutFileName(commit plumbing.Hash) (string, error) {
	// Sanitize bench func.
	bb := bytes.Buffer{}
	e := base64.NewEncoder(base64.StdEncoding, &bb)
	if _, err := e.Write([]byte(b.benchFunc)); err != nil {
		return "", err
	}
	if err := e.Close(); err != nil {
		return "", err
	}

	return fmt.Sprintf("%s-%s.out", bb.String(), commit.String()), nil
}

func (b *Benchmarker) execBenchmark(pkgRoot string, commit plumbing.Hash) (out string, err error) {
	b.logger.Println("Executing benchmark with func:", b.benchFunc, "race enabled:", b.isRaceEnabled)

	fileName, err := b.benchOutFileName(commit)
	if err != nil {
		return "", err
	}

	if _, err := ioutil.ReadFile(filepath.Join(b.resultCacheDir, fileName)); err == nil {
		fmt.Println("Found previous results for ", fileName, b.benchFunc, "Reusing.")
		return filepath.Join(b.resultCacheDir, fileName), nil
	}

	// TODO(bwplotka): Allow memprofiles.
	var extraArgs []string
	if b.benchTime > 0*time.Second {
		extraArgs = append(extraArgs, "-benchtime", b.benchTime.String())
	}
	if !b.isRaceEnabled {
		extraArgs = append(extraArgs, "-race")
	}
	benchCmd := append([]string{"go", "test", "-run", "^$", "-bench", fmt.Sprintf("^%s$", b.benchFunc), "-benchmem", "-timeout", benchmarkerTimeout.String()}, extraArgs...)
	out, err = b.c.exec(append(benchCmd, filepath.Join(pkgRoot, "..."))...)
	if err != nil {
		return "", errors.Wrap(err, "benchmark ended with an error.")
	}

	fn := fileName
	if b.resultCacheDir != "" {
		fn = filepath.Join(b.resultCacheDir, fileName)
	}
	if err := ioutil.WriteFile(fn, []byte(out), os.ModePerm); err != nil {
		return "", err
	}
	return fn, nil
}

func parseFile(path string) (parse.Set, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	bb, err := parse.ParseSet(f)
	if err != nil {
		return nil, err
	}
	return bb, nil
}

func (b *Benchmarker) compareBenchmarks(beforeFile, afterFile string) ([]BenchCmp, error) {
	before, err := parseFile(beforeFile)
	if err != nil {
		return nil, errors.Wrapf(err, "open %s", beforeFile)
	}

	after, err := parseFile(afterFile)
	if err != nil {
		return nil, errors.Wrapf(err, "open %s", afterFile)
	}

	// Use benchcmp library directly.
	cmps, warnings := Correlate(before, after)
	for _, warn := range warnings {
		b.logger.Println(warn)
	}
	if len(cmps) == 0 {
		return nil, errors.New("no repeated benchmarks")
	}

	return cmps, nil
}

func (b *Benchmarker) compareSubBenchmarks(out string) ([]BenchCmp, error) {
	// TODO(bwplotka):
	return nil, errors.New("not implemented")
}
