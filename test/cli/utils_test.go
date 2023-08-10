package cli

import (
	"bytes"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/anchore/stereoscope/pkg/imagetest"
)

var showOutput = flag.Bool("show-output", false, "show stdout and stderr for failing tests")

func logOutputOnFailure(t testing.TB, cmd *exec.Cmd, stdout, stderr string) {
	if t.Failed() && showOutput != nil && *showOutput {
		t.Log("STDOUT:\n", stdout)
		t.Log("STDERR:\n", stderr)
		t.Log("COMMAND:", strings.Join(cmd.Args, " "))
	}
}

func setupPKI(t *testing.T, pw string) func() {
	err := os.Setenv("COSIGN_PASSWORD", pw)
	if err != nil {
		t.Fatal(err)
	}

	cosignPath := filepath.Join(repoRoot(t), ".tmp/cosign")
	cmd := exec.Command(cosignPath, "generate-key-pair")
	stdout, stderr, _ := runCommand(cmd, nil)
	if cmd.ProcessState.ExitCode() != 0 {
		t.Log("STDOUT", stdout)
		t.Log("STDERR", stderr)
		t.Fatalf("could not generate keypair")
	}

	return func() {
		err := os.Unsetenv("COSIGN_PASSWORD")
		if err != nil {
			t.Fatal(err)
		}

		err = os.Remove("cosign.key")
		if err != nil {
			t.Fatalf("could not cleanup cosign.key")
		}

		err = os.Remove("cosign.pub")
		if err != nil {
			t.Fatalf("could not cleanup cosign.key")
		}
	}
}

func getFixtureImage(t testing.TB, fixtureImageName string) string {
	t.Logf("obtaining fixture image for %s", fixtureImageName)
	imagetest.GetFixtureImage(t, "docker-archive", fixtureImageName)
	return imagetest.GetFixtureImageTarPath(t, fixtureImageName)
}

func pullDockerImage(t testing.TB, image string) {
	cmd := exec.Command("docker", "pull", image)
	stdout, stderr, _ := runCommand(cmd, nil)
	if cmd.ProcessState.ExitCode() != 0 {
		t.Log("STDOUT", stdout)
		t.Log("STDERR", stderr)
		t.Fatalf("could not pull docker image")
	}
}

// docker run -v $(pwd)/sbom:/sbom cyclonedx/cyclonedx-cli:latest validate --input-format json --input-version v1_4 --input-file /sbom
func runCycloneDXInDocker(t testing.TB, env map[string]string, image string, f *os.File, args ...string) (*exec.Cmd, string, string) {
	allArgs := append(
		[]string{
			"run",
			"-t",
			"-v",
			fmt.Sprintf("%s:/sbom", f.Name()),
			image,
		},
		args...,
	)
	cmd := exec.Command("docker", allArgs...)
	stdout, stderr, _ := runCommand(cmd, env)
	return cmd, stdout, stderr
}

func runSyftInDocker(t testing.TB, env map[string]string, image string, args ...string) (*exec.Cmd, string, string) {
	allArgs := append(
		[]string{
			"run",
			"-t",
			"-e",
			"SYFT_CHECK_FOR_APP_UPDATE=false",
			"-v",
			fmt.Sprintf("%s:/syft", getSyftBinaryLocationByOS(t, "linux")),
			image,
			"/syft",
		},
		args...,
	)
	cmd := exec.Command("docker", allArgs...)
	stdout, stderr, _ := runCommand(cmd, env)
	return cmd, stdout, stderr
}

func runSyft(t testing.TB, env map[string]string, args ...string) (*exec.Cmd, string, string) {
	return runSyftCommand(t, env, true, args...)
}

func runSyftSafe(t testing.TB, env map[string]string, args ...string) (*exec.Cmd, string, string) {
	return runSyftCommand(t, env, false, args...)
}

func runSyftCommand(t testing.TB, env map[string]string, expectError bool, args ...string) (*exec.Cmd, string, string) {
	cancel := make(chan bool, 1)
	defer func() {
		cancel <- true
	}()

	cmd := getSyftCommand(t, args...)
	if env == nil {
		env = make(map[string]string)
	}

	// we should not have tests reaching out for app update checks
	env["SYFT_CHECK_FOR_APP_UPDATE"] = "false"

	timeout := func() {
		select {
		case <-cancel:
			return
		case <-time.After(60 * time.Second):
		}

		if cmd != nil && cmd.Process != nil {
			// get a stack trace printed
			err := cmd.Process.Signal(syscall.SIGABRT)
			if err != nil {
				t.Errorf("error aborting: %+v", err)
			}
		}
	}

	go timeout()

	stdout, stderr, err := runCommand(cmd, env)

	if !expectError && err != nil && stdout == "" {
		t.Errorf("error running syft: %+v", err)
		t.Errorf("STDOUT: %s", stdout)
		t.Errorf("STDERR: %s", stderr)

		// this probably indicates a timeout... lets run it again with more verbosity to help debug issues
		args = append(args, "-vv")
		cmd = getSyftCommand(t, args...)

		go timeout()
		stdout, stderr, err = runCommand(cmd, env)

		if err != nil {
			t.Errorf("error rerunning syft: %+v", err)
			t.Errorf("STDOUT: %s", stdout)
			t.Errorf("STDERR: %s", stderr)
		}
	}

	return cmd, stdout, stderr
}

func runCommandObj(t testing.TB, cmd *exec.Cmd, env map[string]string, expectError bool) (string, string) {
	cancel := make(chan bool, 1)
	defer func() {
		cancel <- true
	}()

	if env == nil {
		env = make(map[string]string)
	}

	// we should not have tests reaching out for app update checks
	env["SYFT_CHECK_FOR_APP_UPDATE"] = "false"

	timeout := func() {
		select {
		case <-cancel:
			return
		case <-time.After(60 * time.Second):
		}

		if cmd != nil && cmd.Process != nil {
			// get a stack trace printed
			err := cmd.Process.Signal(syscall.SIGABRT)
			if err != nil {
				t.Errorf("error aborting: %+v", err)
			}
		}
	}

	go timeout()

	stdout, stderr, err := runCommand(cmd, env)

	if !expectError && err != nil && stdout == "" {
		t.Errorf("error running syft: %+v", err)
		t.Errorf("STDOUT: %s", stdout)
		t.Errorf("STDERR: %s", stderr)
	}

	return stdout, stderr
}

func runCosign(t testing.TB, env map[string]string, args ...string) (*exec.Cmd, string, string) {
	cmd := getCommand(t, ".tmp/cosign", args...)
	if env == nil {
		env = make(map[string]string)
	}

	stdout, stderr, err := runCommand(cmd, env)

	if err != nil {
		t.Errorf("error running cosign: %+v", err)
	}

	return cmd, stdout, stderr
}

func getCommand(t testing.TB, location string, args ...string) *exec.Cmd {
	return exec.Command(filepath.Join(repoRoot(t), location), args...)
}

func runCommand(cmd *exec.Cmd, env map[string]string) (string, string, error) {
	if env != nil {
		cmd.Env = append(os.Environ(), envMapToSlice(env)...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// ignore errors since this may be what the test expects
	err := cmd.Run()

	return stdout.String(), stderr.String(), err
}

func envMapToSlice(env map[string]string) (envList []string) {
	for key, val := range env {
		if key == "" {
			continue
		}
		envList = append(envList, fmt.Sprintf("%s=%s", key, val))
	}
	return
}

func getSyftCommand(t testing.TB, args ...string) *exec.Cmd {
	return exec.Command(getSyftBinaryLocation(t), args...)
}

func getSyftBinaryLocation(t testing.TB) string {
	if os.Getenv("SYFT_BINARY_LOCATION") != "" {
		// SYFT_BINARY_LOCATION is the absolute path to the snapshot binary
		return os.Getenv("SYFT_BINARY_LOCATION")
	}
	bin := getSyftBinaryLocationByOS(t, runtime.GOOS)

	// only run on valid bin target, when not running in CI
	if bin != "" && os.Getenv("CI") == "" {
		buildSyft(t, bin)
		// regardless if we have a successful build, don't attempt to keep building
		_ = os.Setenv("SYFT_BINARY_LOCATION", bin)
	}

	return bin
}

func buildSyft(t testing.TB, outfile string) {
	dir := repoRoot(t)

	start := time.Now()

	var stdout, stderr string
	var err error
	switch "go" {
	case "go":
		stdout, stderr, err = buildSyftWithGo(dir, outfile)
	case "goreleaser":
		stdout, stderr, err = buildSyftWithGoreleaser(dir)
	case "make":
		stdout, stderr, err = buildSyftWithMake(dir)
	}

	took := time.Now().Sub(start).Round(time.Millisecond)
	if err == nil {
		if len(stderr) == 0 {
			t.Logf("binary is up to date: %s in %v", outfile, took)
		} else {
			t.Logf("built binary: %s in %v\naffected paths:\n%s", outfile, took, stderr)
		}
	} else {
		t.Logf("unable to build binary: %s %v\nSTDOUT:\n%s\nSTDERR:\n%s", outfile, err, stdout, stderr)
	}
}

func goreleaserYamlContents(dir string) string {
	b, _ := os.ReadFile(path.Join(dir, ".goreleaser.yaml"))
	return string(b)
}

func buildSyftWithGo(dir string, outfile string) (string, string, error) {
	d := yaml.NewDecoder(strings.NewReader(goreleaserYamlContents(dir)))
	type releaser struct {
		Builds []struct {
			ID      string `yaml:"id"`
			LDFlags string `yaml:"ldflags"`
		} `yaml:"builds"`
	}
	r := releaser{}
	_ = d.Decode(&r)
	ldflags := ""
	for _, b := range r.Builds {
		if b.ID == "linux-build" {
			ldflags = executeTemplate(b.LDFlags, struct {
				Version string
				Commit  string
				Date    string
				Summary string
			}{
				Version: "VERSION",
				Commit:  "COMMIT",
				Date:    "DATE",
				Summary: "SUMMARY",
			})
			break
		}
	}

	cmd := exec.Command("go",
		"build",
		"-v",
		"-o", outfile,
		"-trimpath",
		"-ldflags", ldflags,
		"./cmd/syft",
	)

	cmd.Dir = dir
	stdout, stderr, err := runCommand(cmd, map[string]string{
		"CGO_ENABLED": "0",
	})
	return stdout, stderr, err
}

func buildSyftWithMake(dir string) (string, string, error) {
	cmd := exec.Command("make", "snapshot")
	cmd.Dir = dir
	stdout, stderr, err := runCommand(cmd, map[string]string{})
	if strings.Contains(stderr, "error=docker build failed") {
		err = nil
	}
	return stdout, stderr, err
}

func buildSyftWithGoreleaser(dir string) (string, string, error) {
	tmpDir := path.Join(dir, ".tmp")

	goreleaserYaml := goreleaserYamlContents(dir)

	// # create a config with the dist dir overridden
	tmpGoreleaserYamlFile := path.Join(tmpDir, "goreleaser.yaml")
	_ = os.WriteFile(tmpGoreleaserYamlFile, []byte("dist: snapshot\n"+goreleaserYaml), os.ModePerm)

	cmd := exec.Command(path.Join(tmpDir, "goreleaser"),
		"build",
		"--snapshot",
		"--single-target",
		"--clean",
		"--config", tmpGoreleaserYamlFile,
	)
	cmd.Dir = dir
	stdout, stderr, err := runCommand(cmd, map[string]string{})
	return stdout, stderr, err
}

func executeTemplate(tpl string, data any) string {
	t, err := template.New("tpl").Parse(tpl)
	if err != nil {
		return fmt.Sprintf("ERROR: %v", err)
	}
	out := &bytes.Buffer{}
	err = t.Execute(out, data)
	if err != nil {
		return fmt.Sprintf("ERROR: %v", err)
	}
	return out.String()
}

func getSyftBinaryLocationByOS(t testing.TB, goOS string) string {
	// note: for amd64 we need to update the snapshot location with the v1 suffix
	// see : https://goreleaser.com/customization/build/#why-is-there-a-_v1-suffix-on-amd64-builds
	archPath := runtime.GOARCH
	if runtime.GOARCH == "amd64" {
		archPath = fmt.Sprintf("%s_v1", archPath)
	}
	// note: there is a subtle - vs _ difference between these versions
	switch goOS {
	case "darwin", "linux":
		return path.Join(repoRoot(t), fmt.Sprintf("snapshot/%s-build_%s_%s/syft", goOS, goOS, archPath))
	default:
		t.Fatalf("unsupported OS: %s", runtime.GOOS)
	}
	return ""
}

func repoRoot(t testing.TB) string {
	t.Helper()
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("unable to find repo root dir: %+v", err)
	}
	absRepoRoot, err := filepath.Abs(strings.TrimSpace(string(root)))
	if err != nil {
		t.Fatal("unable to get abs path to repo root:", err)
	}
	return absRepoRoot
}

func testRetryIntervals(done <-chan struct{}) <-chan time.Duration {
	return exponentialBackoffDurations(250*time.Millisecond, 4*time.Second, 2, done)
}

func exponentialBackoffDurations(minDuration, maxDuration time.Duration, step float64, done <-chan struct{}) <-chan time.Duration {
	sleepDurations := make(chan time.Duration)
	go func() {
		defer close(sleepDurations)
	retryLoop:
		for attempt := 0; ; attempt++ {
			duration := exponentialBackoffDuration(minDuration, maxDuration, step, attempt)

			select {
			case sleepDurations <- duration:
				break
			case <-done:
				break retryLoop
			}

			if duration == maxDuration {
				break
			}
		}
	}()
	return sleepDurations
}

func exponentialBackoffDuration(minDuration, maxDuration time.Duration, step float64, attempt int) time.Duration {
	duration := time.Duration(float64(minDuration) * math.Pow(step, float64(attempt)))
	if duration < minDuration {
		return minDuration
	} else if duration > maxDuration {
		return maxDuration
	}
	return duration
}
