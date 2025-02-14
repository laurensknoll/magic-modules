package vcr

import (
	"fmt"
	"io/fs"
	"magician/exec"
	"magician/provider"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type Tester interface {
	SetRepoPath(version provider.Version, repoPath string)
	FetchCassettes(version provider.Version) error
	Run(mode Mode, version provider.Version) (*Result, error)
	Cleanup() error
}

type Result struct {
	PassedTests  []string
	SkippedTests []string
	FailedTests  []string
}

type Mode int

const (
	Replaying Mode = iota
	Recording
)

const numModes = 2

func (m Mode) Lower() string {
	switch m {
	case Replaying:
		return "replaying"
	case Recording:
		return "recording"
	}
	return "unknown"
}

func (m Mode) Upper() string {
	return strings.ToUpper(m.Lower())
}

type logKey struct {
	mode    Mode
	version provider.Version
}

type vcrTester struct {
	env           map[string]string           // shared environment variables for running tests
	rnr           *exec.Runner                // for running commands and manipulating files
	baseDir       string                      // the directory in which this tester was created
	saKeyPath     string                      // where sa_key.json is relative to baseDir
	cassettePaths map[provider.Version]string // where cassettes are relative to baseDir by version
	logPaths      map[logKey]string           // where logs are relative to baseDir by version and mode
	repoPaths     map[provider.Version]string // relative paths of already cloned repos by version
}

const accTestParalellism = 32

const replayingTimeout = "240m"

var testResultsExpression = regexp.MustCompile(`(?m:^--- (PASS|FAIL|SKIP): TestAcc(\w+))`)

// Create a new tester in the current working directory and write the service account key file.
func NewTester(env map[string]string, rnr *exec.Runner) (Tester, error) {
	saKeyPath := "sa_key.json"
	if err := rnr.WriteFile(saKeyPath, env["SA_KEY"]); err != nil {
		return nil, err
	}
	return &vcrTester{
		env:           env,
		rnr:           rnr,
		baseDir:       rnr.GetCWD(),
		saKeyPath:     saKeyPath,
		cassettePaths: make(map[provider.Version]string, provider.NumVersions),
		logPaths:      make(map[logKey]string, provider.NumVersions*numModes),
		repoPaths:     make(map[provider.Version]string, provider.NumVersions),
	}, nil
}

func (vt *vcrTester) SetRepoPath(version provider.Version, repoPath string) {
	vt.repoPaths[version] = repoPath
}

// Fetch the cassettes for the current version if not already fetched.
// Should be run from the base dir.
func (vt *vcrTester) FetchCassettes(version provider.Version) error {
	cassettePath, ok := vt.cassettePaths[version]
	if ok {
		return nil
	}
	cassettePath = filepath.Join("cassettes", version.String())
	vt.rnr.Mkdir(cassettePath)
	bucketPath := fmt.Sprintf("gs://ci-vcr-cassettes/%sfixtures/*", version.BucketPath())
	// Fetch the cassettes.
	args := []string{"-m", "-q", "cp", bucketPath, cassettePath}
	fmt.Println("Fetching cassettes:\n", "gsutil", strings.Join(args, " "))
	if _, err := vt.rnr.Run("gsutil", args, nil); err != nil {
		return err
	}
	vt.cassettePaths[version] = cassettePath
	return nil
}

// Run the vcr tests in the given mode and provider version and return the result.
// This will overwrite any existing logs for the given mode and version.
func (vt *vcrTester) Run(mode Mode, version provider.Version) (*Result, error) {
	lgky := logKey{mode, version}
	logPath, ok := vt.logPaths[lgky]
	if !ok {
		// We've never run this mode and version.
		logPath = filepath.Join("testlogs", mode.Lower(), version.String())
		if err := vt.rnr.Mkdir(logPath); err != nil {
			return nil, err
		}
		vt.logPaths[lgky] = logPath
	}

	repoPath, ok := vt.repoPaths[version]
	if !ok {
		return nil, fmt.Errorf("no repo cloned for version %s in %v", version, vt.repoPaths)
	}
	if err := vt.rnr.PushDir(repoPath); err != nil {
		return nil, err
	}
	testDirs, err := vt.googleTestDirectory()
	if err != nil {
		return nil, err
	}

	cassettePath := filepath.Join("cassettes", version.String())
	if mode == Replaying {
		cassettePath, ok = vt.cassettePaths[version]
		if !ok {
			return nil, fmt.Errorf("cassettes not fetched for version %s", version)
		}
	}

	args := []string{"test"}
	args = append(args, testDirs...)
	args = append(args,
		"-parallel",
		strconv.Itoa(accTestParalellism),
		"-v",
		"-run=TestAcc",
		"-timeout",
		replayingTimeout,
		`-ldflags=-X=github.com/hashicorp/terraform-provider-google-beta/version.ProviderVersion=acc`,
		"-vet=off",
	)
	env := map[string]string{
		"VCR_PATH":                       filepath.Join(vt.baseDir, cassettePath),
		"VCR_MODE":                       mode.Upper(),
		"ACCTEST_PARALLELISM":            strconv.Itoa(accTestParalellism),
		"GOOGLE_CREDENTIALS":             filepath.Join(vt.baseDir, vt.saKeyPath),
		"GOOGLE_APPLICATION_CREDENTIALS": filepath.Join(vt.baseDir, vt.saKeyPath),
		"GOOGLE_TEST_DIRECTORY":          strings.Join(testDirs, " "),
		"TF_LOG":                         "DEBUG",
		"TF_LOG_SDK_FRAMEWORK":           "INFO",
		"TF_LOG_PATH_MASK":               filepath.Join(vt.baseDir, logPath, "%s.log"),
		"TF_ACC":                         "1",
		"TF_SCHEMA_PANIC_ON_ERROR":       "1",
	}
	for ev, val := range vt.env {
		env[ev] = val
	}
	var printedEnv string
	for ev, val := range env {
		if ev == "SA_KEY" || ev == "GITHUB_TOKEN" {
			val = "{hidden}"
		}
		printedEnv += fmt.Sprintf("%s=%s\n", ev, val)
	}
	fmt.Printf(`Running go:
	env:
%v
	args:
%s
`, printedEnv, strings.Join(args, " "))
	output, err := vt.rnr.Run("go", args, env)
	if err != nil {
		// Use error as output for log.
		output = fmt.Sprintf("Error replaying tests:\n%v", err)
	}
	// Leave repo directory.
	if err := vt.rnr.PopDir(); err != nil {
		return nil, err
	}

	logFileName := filepath.Join(logPath, "all_tests.log")
	// Write output (or error) to test log.
	if err := vt.rnr.WriteFile(logFileName, output); err != nil {
		return nil, fmt.Errorf("error writing replaying log: %v, test output: %v", err, output)
	}
	if err := vt.uploadLogs(logPath, "vcr-check-cassettes"); err != nil {
		return nil, fmt.Errorf("error uploading logs: %v", err)
	}
	return collectResult(output), nil
}

// Deletes the service account key.
func (vt *vcrTester) Cleanup() error {
	if err := vt.rnr.RemoveAll(vt.saKeyPath); err != nil {
		return err
	}
	return nil
}

// Returns a list of all directories to run tests in.
// Must be called after changing into the provider dir.
func (vt *vcrTester) googleTestDirectory() ([]string, error) {
	var testDirs []string
	if allPackages, err := vt.rnr.Run("go", []string{"list", "./..."}, nil); err != nil {
		return nil, err
	} else {
		for _, dir := range strings.Split(allPackages, "\n") {
			if !strings.Contains(dir, "github.com/hashicorp/terraform-provider-google-beta/scripts") {
				testDirs = append(testDirs, dir)
			}
		}
	}
	return testDirs, nil
}

// Print all log file names and contents, except for all_tests.log.
// Must be called after running tests.
func (vt *vcrTester) printLogs(logPath string) {
	vt.rnr.Walk(logPath, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.Name() == "all_tests.log" {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		fmt.Println("======= ", info.Name(), " =======")
		if logContent, err := vt.rnr.ReadFile(path); err == nil {
			fmt.Println(logContent)
		}
		return nil
	})
}

func (vt *vcrTester) uploadLogs(logPath, logBucket string) error {
	bucketPath := fmt.Sprintf("gs://%s/", logBucket)
	args := []string{"-m", "-q", "cp", "-r", logPath, bucketPath}
	fmt.Println("Uploading logs:\n", "gsutil", strings.Join(args, " "))
	if _, err := vt.rnr.Run("gsutil", args, nil); err != nil {
		return err
	}
	args = []string{"-m", "-q", "cp", "-r", "cassettes", bucketPath}
	fmt.Println("Uploading cassettes:\n", "gsutil", strings.Join(args, " "))
	if _, err := vt.rnr.Run("gsutil", args, nil); err != nil {
		return err
	}
	return nil
}

func collectResult(output string) *Result {
	matches := testResultsExpression.FindAllStringSubmatch(output, -1)
	results := make(map[string][]string, len(matches))
	for _, submatches := range matches {
		if len(submatches) != 3 {
			fmt.Printf("Warning: unexpected regex match found in test output: %v", submatches)
			continue
		}
		results[submatches[1]] = append(results[submatches[1]], submatches[2])
	}
	return &Result{
		FailedTests:  results["FAIL"],
		PassedTests:  results["PASS"],
		SkippedTests: results["SKIP"],
	}
}
