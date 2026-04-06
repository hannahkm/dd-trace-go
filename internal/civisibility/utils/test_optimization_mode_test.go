// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2026 Datadog, Inc.

package utils

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tinylib/msgp/msgp"

	"github.com/DataDog/dd-trace-go/v2/internal/civisibility/constants"
	"github.com/DataDog/dd-trace-go/v2/internal/log"
)

func TestCurrentTestOptimizationMode_DirectManifestPath(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	tmpDir := t.TempDir()
	manifestPath := filepath.Join(tmpDir, "manifest.txt")
	if err := os.WriteFile(manifestPath, []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	t.Setenv(constants.CIVisibilityManifestFilePath, manifestPath)

	mode := CurrentTestOptimizationMode()
	if !mode.ManifestEnabled {
		t.Fatalf("expected manifest mode enabled")
	}
	if mode.ManifestPath != manifestPath {
		t.Fatalf("unexpected manifest path: %s", mode.ManifestPath)
	}
}

func TestCurrentTestOptimizationMode_RunfilesDirResolution(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	runfilesDir := t.TempDir()
	manifestRel := filepath.Join("workspace", ".testoptimization", "manifest.txt")
	manifestPath := filepath.Join(runfilesDir, manifestRel)
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	if err := os.WriteFile(manifestPath, []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	t.Setenv(constants.CIVisibilityManifestFilePath, manifestRel)
	t.Setenv("RUNFILES_DIR", runfilesDir)

	mode := CurrentTestOptimizationMode()
	if !mode.ManifestEnabled {
		t.Fatalf("expected manifest mode enabled")
	}
	if mode.ManifestPath != manifestPath {
		t.Fatalf("unexpected manifest path: %s", mode.ManifestPath)
	}
}

func TestCurrentTestOptimizationMode_RunfilesManifestResolution(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	manifestPath := filepath.Join(t.TempDir(), "manifest.txt")
	if err := os.WriteFile(manifestPath, []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	manifestRel := "workspace/.testoptimization/manifest.txt"
	runfilesManifest := filepath.Join(t.TempDir(), "MANIFEST")
	if err := os.WriteFile(runfilesManifest, []byte(manifestRel+" "+manifestPath+"\n"), 0o644); err != nil {
		t.Fatalf("write runfiles manifest: %v", err)
	}

	t.Setenv(constants.CIVisibilityManifestFilePath, manifestRel)
	t.Setenv("RUNFILES_MANIFEST_FILE", runfilesManifest)

	mode := CurrentTestOptimizationMode()
	if !mode.ManifestEnabled {
		t.Fatalf("expected manifest mode enabled")
	}
	if mode.ManifestPath != manifestPath {
		t.Fatalf("unexpected manifest path: %s", mode.ManifestPath)
	}
}

func TestCurrentTestOptimizationMode_RunfilesManifestMissingEntryFallsBackToTestSrcDir(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	testSrcDir := t.TempDir()
	manifestRel := filepath.Join("workspace", ".testoptimization", "manifest.txt")
	manifestPath := filepath.Join(testSrcDir, manifestRel)
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	if err := os.WriteFile(manifestPath, []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	runfilesManifest := filepath.Join(t.TempDir(), "MANIFEST")
	runfilesBody := "malformed-line-without-space\nworkspace/other /tmp/other-manifest.txt\n"
	if err := os.WriteFile(runfilesManifest, []byte(runfilesBody), 0o644); err != nil {
		t.Fatalf("write runfiles manifest: %v", err)
	}

	t.Setenv(constants.CIVisibilityManifestFilePath, manifestRel)
	t.Setenv("RUNFILES_MANIFEST_FILE", runfilesManifest)
	t.Setenv("TEST_SRCDIR", testSrcDir)

	mode := CurrentTestOptimizationMode()
	if !mode.ManifestEnabled {
		t.Fatalf("expected manifest mode enabled")
	}
	if mode.ManifestPath != manifestPath {
		t.Fatalf("unexpected manifest path: %s", mode.ManifestPath)
	}
}

func TestCurrentTestOptimizationMode_TestSrcDirResolution(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	testSrcDir := t.TempDir()
	manifestRel := filepath.Join("workspace", ".testoptimization", "manifest.txt")
	manifestPath := filepath.Join(testSrcDir, manifestRel)
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	if err := os.WriteFile(manifestPath, []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	t.Setenv(constants.CIVisibilityManifestFilePath, manifestRel)
	t.Setenv("TEST_SRCDIR", testSrcDir)

	mode := CurrentTestOptimizationMode()
	if !mode.ManifestEnabled {
		t.Fatalf("expected manifest mode enabled")
	}
	if mode.ManifestPath != manifestPath {
		t.Fatalf("unexpected manifest path: %s", mode.ManifestPath)
	}
}

func TestCurrentTestOptimizationMode_InvalidManifestVersion(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	manifestPath := filepath.Join(t.TempDir(), "manifest.txt")
	if err := os.WriteFile(manifestPath, []byte("2\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	t.Setenv(constants.CIVisibilityManifestFilePath, manifestPath)

	mode := CurrentTestOptimizationMode()
	if mode.ManifestEnabled {
		t.Fatalf("expected manifest mode disabled for unsupported version")
	}
}

func TestCurrentTestOptimizationMode_MissingManifestDisablesMode(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	t.Setenv(constants.CIVisibilityManifestFilePath, filepath.Join(t.TempDir(), "missing-manifest.txt"))

	mode := CurrentTestOptimizationMode()
	if mode.ManifestEnabled {
		t.Fatalf("expected manifest mode disabled for missing manifest")
	}
}

func TestCurrentTestOptimizationMode_ManifestVersionUsesFirstNonEmptyLine(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	manifestPath := filepath.Join(t.TempDir(), "manifest.txt")
	if err := os.WriteFile(manifestPath, []byte("\n  \n1\n2\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	t.Setenv(constants.CIVisibilityManifestFilePath, manifestPath)

	mode := CurrentTestOptimizationMode()
	if !mode.ManifestEnabled {
		t.Fatalf("expected manifest mode enabled when first non-empty line is 1")
	}
}

func TestCurrentTestOptimizationMode_ManifestVersionAssignmentIsSupported(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	manifestPath := filepath.Join(t.TempDir(), "manifest.txt")
	if err := os.WriteFile(manifestPath, []byte("version=1\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	t.Setenv(constants.CIVisibilityManifestFilePath, manifestPath)

	mode := CurrentTestOptimizationMode()
	if !mode.ManifestEnabled {
		t.Fatalf("expected manifest mode enabled when version line is version=1")
	}
}

func TestCurrentTestOptimizationMode_ManifestVersionAssignmentWithSpacesIsSupported(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	manifestPath := filepath.Join(t.TempDir(), "manifest.txt")
	if err := os.WriteFile(manifestPath, []byte("version = 1\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	t.Setenv(constants.CIVisibilityManifestFilePath, manifestPath)

	mode := CurrentTestOptimizationMode()
	if !mode.ManifestEnabled {
		t.Fatalf("expected manifest mode enabled when version line is version = 1")
	}
}

func TestIsManifestModeEnabled(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	if IsManifestModeEnabled() {
		t.Fatal("expected manifest mode disabled by default")
	}

	ResetTestOptimizationModeForTesting()
	manifestPath := filepath.Join(t.TempDir(), "manifest.txt")
	if err := os.WriteFile(manifestPath, []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	t.Setenv(constants.CIVisibilityManifestFilePath, manifestPath)

	if !IsManifestModeEnabled() {
		t.Fatal("expected manifest mode helper to report enabled")
	}
	if IsPayloadFilesModeEnabled() {
		t.Fatal("expected payload-files mode helper to report disabled")
	}
	if IsGitCLIDisabled() {
		t.Fatal("expected git cli helper to report enabled outside payload-files mode")
	}
}

func TestPayloadFilesModeHelpers(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	t.Setenv(constants.CIVisibilityPayloadsInFiles, "true")
	t.Setenv(constants.CIVisibilityUndeclaredOutputsDir, t.TempDir())

	if IsManifestModeEnabled() {
		t.Fatal("expected manifest mode helper to report disabled")
	}
	if !IsPayloadFilesModeEnabled() {
		t.Fatal("expected payload-files mode helper to report enabled")
	}
	if !IsGitCLIDisabled() {
		t.Fatal("expected git cli helper to report disabled in payload-files mode")
	}
}

func TestCurrentTestOptimizationMode_PayloadFiles(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	outDir := t.TempDir()
	t.Setenv(constants.CIVisibilityPayloadsInFiles, "true")
	t.Setenv(constants.CIVisibilityUndeclaredOutputsDir, outDir)

	mode := CurrentTestOptimizationMode()
	if !mode.PayloadFilesEnabled {
		t.Fatalf("expected payload-files mode enabled")
	}
	expectedRoot := filepath.Join(outDir, "payloads")
	if mode.PayloadsRoot != expectedRoot {
		t.Fatalf("unexpected payload root: %s", mode.PayloadsRoot)
	}

	if err := WritePayloadFile("tests", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("write payload file: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(expectedRoot, "tests", "tests-*.json"))
	if err != nil {
		t.Fatalf("glob payload files: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected one tests payload file, got %d", len(matches))
	}
}

func TestCurrentTestOptimizationMode_LogsManifestResolutionAndPayloadFileWrite(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	recordLogger := new(log.RecordLogger)
	oldLevel := log.GetLevel()
	defer log.UseLogger(recordLogger)()
	log.SetLevel(log.LevelDebug)
	defer log.SetLevel(oldLevel)

	tmpDir := t.TempDir()
	outDir := t.TempDir()
	manifestPath := filepath.Join(tmpDir, "manifest.txt")
	if err := os.WriteFile(manifestPath, []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	t.Setenv(constants.CIVisibilityManifestFilePath, manifestPath)
	t.Setenv(constants.CIVisibilityPayloadsInFiles, "true")
	t.Setenv(constants.CIVisibilityUndeclaredOutputsDir, outDir)

	mode := CurrentTestOptimizationMode()
	if !mode.ManifestEnabled {
		t.Fatal("expected manifest mode enabled")
	}
	if !mode.PayloadFilesEnabled {
		t.Fatal("expected payload-file mode enabled")
	}

	if err := WritePayloadFile("tests", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("write payload file: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(outDir, "payloads", "tests", "tests-*.json"))
	if err != nil {
		t.Fatalf("glob payload files: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected one tests payload file, got %d", len(matches))
	}

	logs := recordLogger.Logs()
	if !containsTestOptimizationLogLine(logs, "resolving manifest path from") {
		t.Fatalf("expected manifest resolution log, got %v", logs)
	}
	if !containsTestOptimizationLogLine(logs, "resolved manifest directly") {
		t.Fatalf("expected direct manifest resolution log, got %v", logs)
	}
	if !containsTestOptimizationLogLine(logs, "reading ") {
		t.Fatalf("expected manifest read log, got %v", logs)
	}
	if !containsTestOptimizationLogLine(logs, "manifest version line \"1\" [parsed:\"1\" supported:true") {
		t.Fatalf("expected manifest version log, got %v", logs)
	}
	if !containsTestOptimizationLogLine(logs, "payload-file mode enabled") {
		t.Fatalf("expected payload-file mode log, got %v", logs)
	}
	if !containsTestOptimizationLogLine(logs, matches[0]) {
		t.Fatalf("expected absolute payload file path log %q, got %v", matches[0], logs)
	}
}

func TestCurrentTestOptimizationMode_LogsManifestVersionAssignmentParsing(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	recordLogger := new(log.RecordLogger)
	oldLevel := log.GetLevel()
	defer log.UseLogger(recordLogger)()
	log.SetLevel(log.LevelDebug)
	defer log.SetLevel(oldLevel)

	manifestPath := filepath.Join(t.TempDir(), "manifest.txt")
	if err := os.WriteFile(manifestPath, []byte("version=1\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	t.Setenv(constants.CIVisibilityManifestFilePath, manifestPath)

	mode := CurrentTestOptimizationMode()
	if !mode.ManifestEnabled {
		t.Fatal("expected manifest mode enabled")
	}

	logs := recordLogger.Logs()
	if !containsTestOptimizationLogLine(logs, "manifest version line \"version=1\" [parsed:\"1\" supported:true") {
		t.Fatalf("expected manifest version assignment log, got %v", logs)
	}
}

func TestWritePayloadFileDisabledMode(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	err := WritePayloadFile("tests", []byte(`{"ok":true}`))
	if err == nil {
		t.Fatal("expected payload file mode disabled error")
	}
}

func TestWritePayloadFileInvalidKind(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	t.Setenv(constants.CIVisibilityPayloadsInFiles, "true")
	t.Setenv(constants.CIVisibilityUndeclaredOutputsDir, t.TempDir())

	if err := WritePayloadFile("unknown", []byte(`{"ok":true}`)); err == nil {
		t.Fatal("expected unsupported payload file kind error")
	}
}

func TestWritePayloadFileMissingOutputDir(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	t.Setenv(constants.CIVisibilityPayloadsInFiles, "true")

	err := WritePayloadFile("tests", []byte(`{"ok":true}`))
	if err == nil {
		t.Fatal("expected missing output dir error")
	}
	if !strings.Contains(err.Error(), constants.CIVisibilityUndeclaredOutputsDir) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCurrentTestOptimizationMode_EmptyManifestDisablesMode(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	manifestPath := filepath.Join(t.TempDir(), "manifest.txt")
	if err := os.WriteFile(manifestPath, nil, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	t.Setenv(constants.CIVisibilityManifestFilePath, manifestPath)

	if mode := CurrentTestOptimizationMode(); mode.ManifestEnabled {
		t.Fatal("expected empty manifest to disable manifest mode")
	}
}

func TestCurrentTestOptimizationMode_InvalidManifestAssignmentDisablesMode(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	manifestPath := filepath.Join(t.TempDir(), "manifest.txt")
	if err := os.WriteFile(manifestPath, []byte("version = nope\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	t.Setenv(constants.CIVisibilityManifestFilePath, manifestPath)

	if mode := CurrentTestOptimizationMode(); mode.ManifestEnabled {
		t.Fatal("expected invalid version assignment to disable manifest mode")
	}
}

func TestCacheHTTPFile(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	manifestDir := t.TempDir()
	manifestPath := filepath.Join(manifestDir, "manifest.txt")
	if err := os.WriteFile(manifestPath, []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	t.Setenv(constants.CIVisibilityManifestFilePath, manifestPath)

	cacheFile, ok := CacheHTTPFile("settings.json")
	if !ok {
		t.Fatalf("expected cache file resolution to be enabled")
	}
	expected := filepath.Join(manifestDir, "cache", "http", "settings.json")
	if cacheFile != expected {
		t.Fatalf("unexpected cache file path: %s", cacheFile)
	}
}

func TestCacheHTTPFileDisabledOrBlankName(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	if cacheFile, ok := CacheHTTPFile("settings.json"); ok || cacheFile != "" {
		t.Fatalf("expected cache resolution disabled without manifest mode, got %q %t", cacheFile, ok)
	}

	ResetTestOptimizationModeForTesting()
	manifestDir := t.TempDir()
	manifestPath := filepath.Join(manifestDir, "manifest.txt")
	if err := os.WriteFile(manifestPath, []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	t.Setenv(constants.CIVisibilityManifestFilePath, manifestPath)

	if cacheFile, ok := CacheHTTPFile("   "); ok || cacheFile != "" {
		t.Fatalf("expected blank cache file name to be rejected, got %q %t", cacheFile, ok)
	}
}

func TestMsgpackToJSON(t *testing.T) {
	payload := msgp.AppendMapHeader(nil, 1)
	payload = msgp.AppendString(payload, "k")
	payload = msgp.AppendString(payload, "v")

	jsonPayload, err := MsgpackToJSON(payload)
	if err != nil {
		t.Fatalf("msgpack to json failed: %v", err)
	}
	if !strings.Contains(string(jsonPayload), "\"k\"") || !strings.Contains(string(jsonPayload), "\"v\"") {
		t.Fatalf("unexpected json payload: %s", string(jsonPayload))
	}
}

func TestMsgpackToJSONRejectsEmptyPayload(t *testing.T) {
	jsonPayload, err := MsgpackToJSON(nil)
	if err == nil {
		t.Fatal("expected empty payload error")
	}
	if jsonPayload != nil {
		t.Fatalf("expected nil json payload, got %q", string(jsonPayload))
	}
}

func TestMsgpackToJSONRejectsInvalidPayload(t *testing.T) {
	jsonPayload, err := MsgpackToJSON([]byte{0xc1})
	if err == nil {
		t.Fatal("expected invalid payload error")
	}
	if jsonPayload != nil {
		t.Fatalf("expected nil json payload, got %q", string(jsonPayload))
	}
}

func TestIsManifestVersionSupportedRejectsManifestWithoutVersionLine(t *testing.T) {
	manifestPath := filepath.Join(t.TempDir(), "manifest.txt")
	if err := os.WriteFile(manifestPath, []byte("\n \n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	if isManifestVersionSupported(manifestPath) {
		t.Fatal("expected manifest without version line to be unsupported")
	}
}

func TestParseManifestVersionPreservesNonVersionAssignments(t *testing.T) {
	if version := parseManifestVersion("manifest = 1"); version != "manifest = 1" {
		t.Fatalf("expected non-version assignment to be preserved, got %q", version)
	}
}

func TestIsGitCLIDisabled(t *testing.T) {
	ResetTestOptimizationModeForTesting()
	t.Cleanup(ResetTestOptimizationModeForTesting)

	if IsGitCLIDisabled() {
		t.Fatal("expected git cli enabled by default")
	}

	ResetTestOptimizationModeForTesting()
	t.Setenv(constants.CIVisibilityPayloadsInFiles, "true")
	t.Setenv(constants.CIVisibilityUndeclaredOutputsDir, t.TempDir())

	if !IsGitCLIDisabled() {
		t.Fatal("expected git cli disabled in payload-files mode")
	}
}

func containsTestOptimizationLogLine(lines []string, want string) bool {
	for _, line := range lines {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}
