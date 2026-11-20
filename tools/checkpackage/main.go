// Command checkpackage verifies a signed release and, by default, starts the
// verified executable through the same go-plugin protocol used by Sub2API.
// It performs no upstream requests and requires no account credentials.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/feeeei/sub2api-plugin-framework/hostemu"
	"github.com/feeeei/sub2api-plugin-framework/manifest"
	"github.com/feeeei/sub2api-plugin-framework/packager"
)

const (
	trustedKeyID = "vipox-zstd-20261104"
	hostVersion  = "0.2.19"
	platform     = "linux-amd64"
)

type selftestReport struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

type report struct {
	Signature      string                  `json:"signature"`
	Compatibility  *manifest.Compatibility `json:"compatibility"`
	SHA256         string                  `json:"sha256"`
	Files          int                     `json:"files"`
	RuntimeStartup string                  `json:"runtime_startup"`
	Selftest       *selftestReport         `json:"selftest"`
	Error          string                  `json:"error,omitempty"`
}

func main() {
	result, err := run(os.Args[1:])
	if err != nil {
		result.Error = err.Error()
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	if encodeErr := encoder.Encode(result); encodeErr != nil {
		fmt.Fprintln(os.Stderr, "cannot write validation report:", encodeErr)
		os.Exit(1)
	}
	if err != nil {
		os.Exit(1)
	}
}

func run(args []string) (report, error) {
	result := report{Signature: "not_verified", RuntimeStartup: "not_run"}
	flags := flag.NewFlagSet("checkpackage", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	packagePath := flags.String("package", "", "signed .s2plugin package path")
	publicKeyPath := flags.String("public-key", "", "Base64 Ed25519 public-key file")
	targetHost := flags.String("host-version", hostVersion, "Sub2API version for the manifest compatibility check")
	verifyOnly := flags.Bool("verify-only", false, "verify package without extracting or starting its executable")
	if err := flags.Parse(args); err != nil {
		return result, err
	}
	// Either named flags or exactly two positional paths are accepted.
	if flags.NArg() != 0 {
		if flags.NArg() != 2 || *packagePath != "" || *publicKeyPath != "" {
			return result, errors.New("usage: checkpackage [--verify-only] --package FILE.s2plugin --public-key FILE.public (or two positional paths)")
		}
		*packagePath, *publicKeyPath = flags.Arg(0), flags.Arg(1)
	}
	if *packagePath == "" || *publicKeyPath == "" {
		return result, errors.New("both package path and public-key file are required")
	}

	publicKey, err := readPublicKey(*publicKeyPath)
	if err != nil {
		return result, err
	}
	opts := packager.VerifyOptions{
		TrustedPublishers: map[string]string{trustedKeyID: publicKey},
		AllowUnsigned:     false,
		HostVersion:       *targetHost,
		Platform:          platform,
	}
	verified, err := packager.Verify(*packagePath, opts)
	if err != nil {
		result.Signature = "rejected"
		return result, fmt.Errorf("package verification failed: %w", err)
	}
	result.Signature = verified.SignatureStatus
	result.Compatibility = verified.Compatibility
	result.SHA256 = verified.ArtifactSHA256
	result.Files = len(verified.Files)
	if verified.SignatureStatus != packager.SignatureTrusted || verified.KeyID != trustedKeyID {
		return result, errors.New("package was not signed by the expected trusted publisher")
	}
	if verified.Compatibility == nil || !verified.Compatibility.Compatible {
		return result, fmt.Errorf("package does not support Sub2API %s", *targetHost)
	}
	if *verifyOnly {
		result.RuntimeStartup = "skipped_verify_only"
		return result, nil
	}
	if runtime.GOOS+"-"+runtime.GOARCH != platform {
		return result, fmt.Errorf("runtime validation must run on %s; use --verify-only for archive verification on another platform", platform)
	}

	// Short private paths avoid Unix-domain socket path limits. Extract only
	// into a fresh directory; no existing deployment files are read or replaced.
	tempRoot, err := os.MkdirTemp("/tmp", "s2chk-")
	if err != nil {
		return result, fmt.Errorf("create private validation directory: %w", err)
	}
	defer os.RemoveAll(tempRoot)
	extractRoot := filepath.Join(tempRoot, "package")
	extracted, err := packager.Extract(*packagePath, extractRoot, opts)
	if err != nil {
		return result, fmt.Errorf("verified package extraction failed: %w", err)
	}
	if extracted.ArtifactSHA256 != verified.ArtifactSHA256 {
		return result, errors.New("package changed between verification and extraction")
	}
	socketDir := filepath.Join(tempRoot, "sock")
	if err := os.Mkdir(socketDir, 0o700); err != nil {
		return result, fmt.Errorf("create private socket directory: %w", err)
	}
	binaryPath, err := packager.RuntimeBinaryPath(extractRoot, extracted.Manifest, "linux", "amd64")
	if err != nil {
		return result, err
	}
	runtimeEntry := extracted.Manifest.Runtimes[platform]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := hostemu.Launch(ctx, hostemu.LaunchOptions{
		BinaryPath:      binaryPath,
		SHA256:          extracted.Manifest.Files[runtimeEntry.Path],
		StartTimeout:    15 * time.Second,
		SocketDir:       socketDir,
		ExpectedID:      extracted.Manifest.ID,
		ExpectedVersion: extracted.Manifest.Version,
		PassHostEnv:     false,
	})
	if err != nil {
		result.RuntimeStartup = "failed"
		return result, fmt.Errorf("verified plugin process could not start: %w", err)
	}
	defer session.Close()
	actualCapabilities := session.StartupInfo.GetCapabilities()
	wantedCapabilities := extracted.Manifest.CapabilityIDs()
	if !sameStrings(actualCapabilities, wantedCapabilities) {
		result.RuntimeStartup = "failed"
		return result, errors.New("runtime capabilities differ from the signed manifest")
	}
	if _, err := session.ValidateAndApplyConfig(ctx, []byte(`{}`)); err != nil {
		result.RuntimeStartup = "failed"
		return result, fmt.Errorf("default configuration validation/application failed: %w", err)
	}
	health, err := session.Health(ctx)
	if err != nil || health == nil || !health.GetHealthy() {
		result.RuntimeStartup = "failed"
		if err != nil {
			return result, fmt.Errorf("configured process health check failed: %w", err)
		}
		return result, errors.New("configured process is not healthy")
	}
	result.RuntimeStartup = "passed"
	selftest, err := session.TestConfig(ctx, []byte(`{}`))
	if err != nil {
		return result, fmt.Errorf("local compression self-test RPC failed: %w", err)
	}
	if selftest == nil {
		return result, errors.New("local compression self-test returned no result")
	}
	result.Selftest = &selftestReport{Success: selftest.GetSuccess(), Message: selftest.GetMessage()}
	if !selftest.GetSuccess() {
		return result, errors.New("local compression self-test failed")
	}
	return result, nil
}

func readPublicKey(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open public-key file: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", fmt.Errorf("read public-key file: %w", err)
	}
	if len(raw) > 4096 {
		return "", errors.New("public-key file exceeds 4096 bytes")
	}
	encoded := strings.TrimSpace(string(raw))
	if _, err := packager.ParsePublicKey(encoded); err != nil {
		return "", errors.New("public-key file must contain one Base64 Ed25519 public key")
	}
	return encoded, nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, value := range a {
		counts[value]++
	}
	for _, value := range b {
		counts[value]--
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}
