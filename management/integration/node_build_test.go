package integration_test

import (
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
)

type nodeBuildInfo struct {
	GoVersion  string `json:"go_version"`
	SHA256     string `json:"sha256"`
	Protection string `json:"protection"`
}

// A build record is provenance from our builder, not a signature or a substitute
// for verifying the release manifest. Its hash binds the fixture to the exact
// transformed executable; an arbitrary missing build-info section is rejected.
func readNodeBuild(binary, recordPath string) (nodeBuildInfo, error) {
	var result nodeBuildInfo
	f, err := os.Open(binary)
	if err != nil {
		return result, err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	f.Close()
	if err != nil {
		return result, err
	}
	result.SHA256 = hex.EncodeToString(h.Sum(nil))
	info, buildErr := buildinfo.ReadFile(binary)
	if buildErr == nil {
		for _, setting := range info.Settings {
			if setting.Key == "-race" && setting.Value == "true" {
				return result, fmt.Errorf("resource gates require a Node built without -race")
			}
		}
		if strings.HasPrefix(info.GoVersion, "go1.") {
			result.GoVersion, result.Protection = info.GoVersion, "plain"
		}
	}
	if recordPath == "" {
		if result.GoVersion == "" {
			return result, fmt.Errorf("missing Go metadata: supply the verified BUILD.json via TACK_NODE_BUILD_RECORD")
		}
		return result, nil
	}
	r, err := os.Open(recordPath)
	if err != nil {
		return result, err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, 65537))
	if err != nil || len(data) > 65536 {
		return result, fmt.Errorf("invalid or oversized Node build record")
	}
	var record struct {
		Format, Component, SHA256, OS, Arch, Protection string
		GoVersion                                       string `json:"go_version"`
		Race                                            *bool
		CGO                                             *bool `json:"cgo_enabled"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return result, err
	}
	if record.Format != "tack-build-v1" || record.Component != "tack-node" || record.SHA256 != result.SHA256 ||
		record.OS != runtime.GOOS || record.Arch != runtime.GOARCH || record.Race == nil || *record.Race ||
		record.CGO == nil || *record.CGO || !strings.HasPrefix(record.GoVersion, "go1.") {
		return result, fmt.Errorf("Node build record does not match this non-race executable/host")
	}
	if record.Protection != "plain" && record.Protection != "obfuscated" && record.Protection != "packed" {
		return result, fmt.Errorf("unknown Node protection profile")
	}
	if (result.GoVersion == "" && record.Protection == "plain") ||
		(result.GoVersion != "" && (record.GoVersion != result.GoVersion || record.Protection != "plain")) {
		return result, fmt.Errorf("Node build record contradicts executable metadata")
	}
	result.GoVersion, result.Protection = record.GoVersion, record.Protection
	return result, nil
}

func nodeBuild(t *testing.T, binary string) nodeBuildInfo {
	t.Helper()
	info, err := readNodeBuild(binary, os.Getenv("TACK_NODE_BUILD_RECORD"))
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestNodeBuildRecordRejectsUnboundOrUninstrumentedClaims(t *testing.T) {
	binary := t.TempDir() + "/node"
	if err := os.WriteFile(binary, []byte("opaque transformed fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readNodeBuild(binary, ""); err == nil {
		t.Fatal("accepted unknown binary without provenance")
	}
	h := sha256.Sum256([]byte("opaque transformed fixture"))
	valid := map[string]any{"format": "tack-build-v1", "component": "tack-node", "sha256": hex.EncodeToString(h[:]),
		"os": runtime.GOOS, "arch": runtime.GOARCH, "go_version": "go1.23.12", "race": false, "cgo_enabled": false, "protection": "packed"}
	path := t.TempDir() + "/BUILD.json"
	write := func(m map[string]any) {
		b, _ := json.Marshal(m)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(valid)
	if _, err := readNodeBuild(binary, path); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]any{"sha256": "different bytes", "component": "tack", "arch": "other",
		"race": true, "cgo_enabled": true, "go_version": "", "protection": "plain", "format": "unknown"} {
		t.Run(key, func(t *testing.T) {
			copy := make(map[string]any)
			for k, v := range valid {
				copy[k] = v
			}
			copy[key] = value
			write(copy)
			if _, err := readNodeBuild(binary, path); err == nil {
				t.Fatal("accepted inconsistent build record")
			}
		})
	}
	delete(valid, "race")
	write(valid)
	if _, err := readNodeBuild(binary, path); err == nil {
		t.Fatal("accepted missing race field")
	}
}
