// Copyright (c) 2013 Yasuhiro Matsumoto, Jason McVetta.
// This is Free Software,  released under the MIT license.
// See http://mattn.mit-license.org/2013 for details.

// goveralls is a Go client for Coveralls.io.
package main

import (
	"bytes"
	_ "crypto/sha512"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

/*
	https://coveralls.io/docs/api_reference
*/

// Flags are extra flags to the tests
type Flags []string

// String implements flag.Value interface.
func (a *Flags) String() string {
	return strings.Join(*a, ",")
}

// Set implements flag.Value interface.
func (a *Flags) Set(value string) error {
	*a = append(*a, value)
	return nil
}

var (
	extraFlags    Flags
	pkg           = flag.String("package", "", "Go package")
	verbose       = flag.Bool("v", false, "Pass '-v' argument to 'go test' and output to stdout")
	race          = flag.Bool("race", false, "Pass '-race' argument to 'go test'")
	debug         = flag.Bool("debug", false, "Enable debug output")
	coverprof     = flag.String("coverprofile", "", "Must be supplied; a comma-separated list of of go cover profile files")
	covermode     = flag.String("covermode", "count", "sent as covermode argument to go test")
	repotoken     = flag.String("repotoken", os.Getenv("COVERALLS_TOKEN"), "Repository Token on coveralls")
	reponame      = flag.String("reponame", "", "Repository name")
	repotokenfile = flag.String("repotokenfile", os.Getenv("COVERALLS_TOKEN_FILE"), "Repository Token file on coveralls")
	parallel      = flag.Bool("parallel", os.Getenv("COVERALLS_PARALLEL") != "", "Submit as parallel")
	endpoint      = flag.String("endpoint", "https://coveralls.io", "Hostname to submit Coveralls data to")
	service       = flag.String("service", "", "The CI service or other environment in which the test suite was run. ")
	shallow       = flag.Bool("shallow", false, "Shallow coveralls internal server errors")
	ignore        = flag.String("ignore", "", "Comma separated files to ignore")
	insecure      = flag.Bool("insecure", false, "Set insecure to skip verification of certificates")
	show          = flag.Bool("show", false, "Show which package is being tested")
	customJobID   = flag.String("jobid", "", "Custom set job token")
	jobNumber     = flag.String("jobnumber", "", "Custom set job number")
	flagName      = flag.String("flagname", os.Getenv("COVERALLS_FLAG_NAME"), "Job flag name, e.g. \"Unit\", \"Functional\", or \"Integration\". Will be shown in the Coveralls UI.")

	parallelFinish = flag.Bool("parallel-finish", false, "finish parallel test")
)

/*
func init() {
	flag.Var((*buildutil.TagsFlag)(&build.Default.BuildTags), "tags", buildutil.TagsFlagDoc)
}*/

// usage supplants package flag's Usage variable
var usage = func() {
	cmd := os.Args[0]
	// fmt.Fprintf(os.Stderr, "Usage of %s:\n", cmd)
	s := "Usage: %s [options]\n"
	fmt.Fprintf(os.Stderr, s, cmd)
	flag.PrintDefaults()
}

// A SourceFile represents a source code file and its coverage data for a
// single job.
type SourceFile struct {
	Name     string        `json:"name"`     // File path of this source file
	Source   string        `json:"source"`   // Full source code of this file
	Coverage []interface{} `json:"coverage"` // Requires both nulls and integers
}

// A Job represents the coverage data from a single run of a test suite.
type Job struct {
	RepoToken          *string       `json:"repo_token,omitempty"`
	ServiceJobID       string        `json:"service_job_id"`
	ServiceJobNumber   string        `json:"service_job_number,omitempty"`
	ServicePullRequest string        `json:"service_pull_request,omitempty"`
	ServiceName        string        `json:"service_name"`
	FlagName           string        `json:"flag_name,omitempty"`
	SourceFiles        []*SourceFile `json:"source_files"`
	Parallel           *bool         `json:"parallel,omitempty"`
	Git                *Git          `json:"git,omitempty"`
	RunAt              time.Time     `json:"run_at"`
}

// A Response is returned by the Coveralls.io API.
type Response struct {
	Message string `json:"message"`
	URL     string `json:"url"`
	Error   bool   `json:"error"`
}

// A WebHookResponse is returned by the Coveralls.io WebHook.
type WebHookResponse struct {
	Done bool `json:"done"`
}

// getPkgs returns packages for measuring coverage. Returned packages doesn't
// contain vendor packages.
func getPkgs(pkg string) ([]string, error) {
	argList := []string{"list"}
	if pkg == "" {
		argList = append(argList, "./...")
	} else {
		argList = append(argList, strings.Split(pkg, " ")...)
	}
	out, err := exec.Command("go", argList...).CombinedOutput()
	if err != nil {
		return nil, err
	}
	allPkgs := strings.Split(strings.Trim(string(out), "\n"), "\n")
	pkgs := make([]string, 0, len(allPkgs))
	for _, p := range allPkgs {
		if strings.Contains(p, "/vendor/") {
			continue
		}
		// go modules output
		if strings.Contains(p, "go: ") {
			continue
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

func getCoverage() ([]*SourceFile, error) {
	if *coverprof == "" {
		return nil, errors.New("no cover profile files provided")
	}

	// find the module name
	b, err := ioutil.ReadFile("go.mod")
	if err != nil {
		return nil, err
	}

	var moduleName string
	for _, lineBytes := range bytes.Split(b, []byte{'\n'}) {
		line := string(lineBytes)

		if !strings.HasPrefix(line, "module") {
			continue
		}

		s := strings.SplitN(line, " ", 2)
		if len(s) != 2 {
			break
		}

		moduleName = s[1]
		break
	}

	if moduleName == "" {
		return nil, errors.New("cannot find module name in go.mod")
	}

	return parseCover(moduleName, *coverprof)
}

var vscDirs = []string{".git", ".hg", ".bzr", ".svn"}

func findRepositoryRoot(dir string) (string, bool) {
	for _, vcsdir := range vscDirs {
		if d, err := os.Stat(filepath.Join(dir, vcsdir)); err == nil && d.IsDir() {
			return dir, true
		}
	}
	nextdir := filepath.Dir(dir)
	if nextdir == dir {
		return "", false
	}
	return findRepositoryRoot(nextdir)
}

func getCoverallsSourceFileName(name string) string {
	if dir, ok := findRepositoryRoot(name); ok {
		name = strings.TrimPrefix(name, dir+string(os.PathSeparator))
	}
	return filepath.ToSlash(name)
}

func process() error {
	log.SetFlags(log.Ltime | log.Lshortfile)
	//
	// Parse Flags
	//
	flag.Usage = usage
	flag.Var(&extraFlags, "flags", "extra flags to the tests")
	flag.Parse()
	if len(flag.Args()) > 0 {
		flag.Usage()
		os.Exit(1)
	}

	//
	// Setup PATH environment variable
	//
	paths := filepath.SplitList(os.Getenv("PATH"))
	if goroot := os.Getenv("GOROOT"); goroot != "" {
		paths = append(paths, filepath.Join(goroot, "bin"))
	}
	if gopath := os.Getenv("GOPATH"); gopath != "" {
		for _, path := range filepath.SplitList(gopath) {
			paths = append(paths, filepath.Join(path, "bin"))
		}
	}
	os.Setenv("PATH", strings.Join(paths, string(filepath.ListSeparator)))

	//
	// Handle certificate verification configuration
	//
	if *insecure {
		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	//
	// Initialize Job
	//

	// flags are never nil, so no nil check needed
	githubEvent := getGithubEvent()
	var jobID string
	if *customJobID != "" {
		jobID = *customJobID
	} else if ServiceJobID := os.Getenv("COVERALLS_SERVICE_JOB_ID"); ServiceJobID != "" {
		jobID = ServiceJobID
	} else if travisjobID := os.Getenv("TRAVIS_JOB_ID"); travisjobID != "" {
		jobID = travisjobID
	} else if circleCIJobID := os.Getenv("CIRCLE_BUILD_NUM"); circleCIJobID != "" {
		jobID = circleCIJobID
	} else if appveyorJobID := os.Getenv("APPVEYOR_JOB_ID"); appveyorJobID != "" {
		jobID = appveyorJobID
	} else if semaphorejobID := os.Getenv("SEMAPHORE_BUILD_NUMBER"); semaphorejobID != "" {
		jobID = semaphorejobID
	} else if jenkinsjobID := os.Getenv("BUILD_NUMBER"); jenkinsjobID != "" {
		jobID = jenkinsjobID
	} else if buildID := os.Getenv("BUILDKITE_BUILD_ID"); buildID != "" {
		jobID = buildID
	} else if droneBuildNumber := os.Getenv("DRONE_BUILD_NUMBER"); droneBuildNumber != "" {
		jobID = droneBuildNumber
	} else if buildkiteBuildNumber := os.Getenv("BUILDKITE_BUILD_NUMBER"); buildkiteBuildNumber != "" {
		jobID = buildkiteBuildNumber
	} else if codeshipjobID := os.Getenv("CI_BUILD_ID"); codeshipjobID != "" {
		jobID = codeshipjobID
	} else if githubRunID := os.Getenv("GITHUB_RUN_ID"); githubRunID != "" {
		jobID = githubRunID
	}

	if *repotoken == "" && *repotokenfile != "" {
		tokenBytes, err := ioutil.ReadFile(*repotokenfile)
		if err != nil {
			return err
		}
		*repotoken = strings.TrimSpace(string(tokenBytes))
	}

	if *parallelFinish {
		return processParallelFinish(jobID, *repotoken)
	}

	if *repotoken == "" {
		repotoken = nil // remove the entry from json
	}

	head := "HEAD"
	var pullRequest string
	if prNumber := os.Getenv("CIRCLE_PR_NUMBER"); prNumber != "" {
		// for Circle CI (pull request from forked repo)
		pullRequest = prNumber
	} else if prNumber := os.Getenv("TRAVIS_PULL_REQUEST"); prNumber != "" && prNumber != "false" {
		pullRequest = prNumber
	} else if prURL := os.Getenv("CI_PULL_REQUEST"); prURL != "" {
		// for Circle CI
		pullRequest = regexp.MustCompile(`[0-9]+$`).FindString(prURL)
	} else if prNumber := os.Getenv("APPVEYOR_PULL_REQUEST_NUMBER"); prNumber != "" {
		pullRequest = prNumber
	} else if prNumber := os.Getenv("PULL_REQUEST_NUMBER"); prNumber != "" {
		pullRequest = prNumber
	} else if prNumber := os.Getenv("BUILDKITE_PULL_REQUEST"); prNumber != "" {
		pullRequest = prNumber
	} else if prNumber := os.Getenv("DRONE_PULL_REQUEST"); prNumber != "" {
		pullRequest = prNumber
	} else if prNumber := os.Getenv("BUILDKITE_PULL_REQUEST"); prNumber != "" {
		pullRequest = prNumber
	} else if prNumber := os.Getenv("CI_PR_NUMBER"); prNumber != "" {
		pullRequest = prNumber
	} else if os.Getenv("GITHUB_EVENT_NAME") == "pull_request" {
		number := githubEvent["number"].(float64)
		pullRequest = strconv.Itoa(int(number))

		ghPR := githubEvent["pull_request"].(map[string]interface{})
		ghHead := ghPR["head"].(map[string]interface{})
		head = ghHead["sha"].(string)
	}

	if *service == "" && os.Getenv("TRAVIS_JOB_ID") != "" {
		*service = "travis-ci"
	}

	sourceFiles, err := getCoverage()
	if err != nil {
		return err
	}

	j := Job{
		RunAt:              time.Now(),
		RepoToken:          repotoken,
		ServicePullRequest: pullRequest,
		Parallel:           parallel,
		Git:                collectGitInfo(head),
		SourceFiles:        sourceFiles,
		ServiceName:        *service,
		FlagName:           *flagName,
	}

	// Only include a job ID if it's known, otherwise, Coveralls looks
	// for the job and can't find it.
	if jobID != "" {
		j.ServiceJobID = jobID
	}
	j.ServiceJobNumber = *jobNumber

	// Ignore files
	if len(*ignore) > 0 {
		patterns := strings.Split(*ignore, ",")
		for i, pattern := range patterns {
			patterns[i] = strings.TrimSpace(pattern)
		}
		var files []*SourceFile
	Files:
		for _, file := range j.SourceFiles {
			for _, pattern := range patterns {
				match, err := filepath.Match(pattern, file.Name)
				if err != nil {
					return err
				}
				if match {
					fmt.Printf("ignoring %s\n", file.Name)
					continue Files
				}
			}
			files = append(files, file)
		}
		j.SourceFiles = files
	}

	if *debug {
		b, err := json.MarshalIndent(j, "", "  ")
		if err != nil {
			return err
		}
		log.Printf("Posting data: %s", b)
	}

	return submitCoverallsJob(j)
}

func getGithubEvent() map[string]interface{} {
	jsonFilePath := os.Getenv("GITHUB_EVENT_PATH")
	if jsonFilePath == "" {
		return nil
	}

	jsonFile, err := os.Open(jsonFilePath)
	if err != nil {
		log.Fatal(err)
	}
	defer jsonFile.Close()

	jsonByte, _ := ioutil.ReadAll(jsonFile)

	// unmarshal the json into a release event
	var event map[string]interface{}
	err = json.Unmarshal(jsonByte, &event)
	if err != nil {
		log.Fatal(err)
	}

	return event
}

func main() {
	if err := process(); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}
}
