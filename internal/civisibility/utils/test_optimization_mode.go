// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2026 Datadog, Inc.

package utils

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tinylib/msgp/msgp"

	"github.com/DataDog/dd-trace-go/v2/internal/civisibility/constants"
	"github.com/DataDog/dd-trace-go/v2/internal/env"
	logger "github.com/DataDog/dd-trace-go/v2/internal/log"
)

type (
	// TestOptimizationMode stores the process-level mode settings for Bazel-compatible Test Optimization flows.
	TestOptimizationMode struct {
		ManifestEnabled     bool
		PayloadFilesEnabled bool
		ManifestPath        string
		ManifestDir         string
		PayloadsRoot        string
	}
)

var (
	testOptimizationModeMu   sync.Mutex
	testOptimizationModeOnce sync.Once
	currentTestOptimization  TestOptimizationMode
	payloadFileCounter       uint64 = 0
)

// CurrentTestOptimizationMode returns the resolved process-wide Test Optimization mode.
func CurrentTestOptimizationMode() TestOptimizationMode {
	testOptimizationModeMu.Lock()
	defer testOptimizationModeMu.Unlock()

	testOptimizationModeOnce.Do(func() {
		currentTestOptimization = resolveTestOptimizationMode()
	})

	return currentTestOptimization
}

// IsManifestModeEnabled returns true when a compatible manifest has been resolved.
func IsManifestModeEnabled() bool {
	return CurrentTestOptimizationMode().ManifestEnabled
}

// IsPayloadFilesModeEnabled returns true when payload-file mode is enabled through environment variables.
func IsPayloadFilesModeEnabled() bool {
	return CurrentTestOptimizationMode().PayloadFilesEnabled
}

// CacheHTTPFile returns the expected cache/http file path in manifest mode.
func CacheHTTPFile(name string) (string, bool) {
	mode := CurrentTestOptimizationMode()
	if !mode.ManifestEnabled || strings.TrimSpace(name) == "" {
		return "", false
	}
	cacheFile := filepath.Join(mode.ManifestDir, "cache", "http", name)
	logger.Debug("civisibility: manifest mode cache file resolved [name:%s path:%s]", name, absolutePathForLog(cacheFile))
	return cacheFile, true
}

// MsgpackToJSON converts any MessagePack payload into JSON bytes.
func MsgpackToJSON(msgpackPayload []byte) ([]byte, error) {
	if len(msgpackPayload) == 0 {
		return nil, errors.New("msgpack payload is empty")
	}

	var jsonBuf bytes.Buffer
	if _, err := msgp.CopyToJSON(&jsonBuf, bytes.NewReader(msgpackPayload)); err != nil {
		return nil, fmt.Errorf("converting msgpack to json: %w", err)
	}
	return jsonBuf.Bytes(), nil
}

// WritePayloadFile writes payload JSON in Bazel undeclared outputs.
func WritePayloadFile(kind string, jsonPayload []byte) error {
	if kind != "tests" && kind != "coverage" {
		logger.Debug("civisibility: refusing to write unsupported payload file kind %q", kind)
		return fmt.Errorf("unsupported payload file kind %q", kind)
	}

	mode := CurrentTestOptimizationMode()
	if !mode.PayloadFilesEnabled {
		logger.Debug("civisibility: payload-file mode disabled; refusing to write %s payload file", kind)
		return errors.New("payload file mode is disabled")
	}
	if mode.PayloadsRoot == "" {
		logger.Debug("civisibility: payload-file mode enabled for %s payloads but %s is empty", kind, constants.CIVisibilityUndeclaredOutputsDir)
		return fmt.Errorf("%s is required when %s is enabled", constants.CIVisibilityUndeclaredOutputsDir, constants.CIVisibilityPayloadsInFiles)
	}

	outDir := filepath.Join(mode.PayloadsRoot, kind)
	absoluteOutDir := absolutePathForLog(outDir)
	logger.Debug("civisibility: ensuring %s payload output directory exists at %s", kind, absoluteOutDir)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		logger.Debug("civisibility: failed to create %s payload output directory %s: %v", kind, absoluteOutDir, err)
		return fmt.Errorf("creating payload output dir: %w", err)
	}

	seq := atomic.AddUint64(&payloadFileCounter, 1)
	fileName := fmt.Sprintf("%s-%d-%d-%d.json", kind, time.Now().UnixNano(), os.Getpid(), seq)
	filePath := filepath.Join(outDir, fileName)
	absoluteFilePath := absolutePathForLog(filePath)
	logger.Debug("civisibility: writing %s payload file to %s", kind, absoluteFilePath)

	if err := os.WriteFile(filePath, jsonPayload, 0o644); err != nil {
		logger.Debug("civisibility: failed writing %s payload file to %s: %v", kind, absoluteFilePath, err)
		return fmt.Errorf("writing payload file: %w", err)
	}
	logger.Debug("civisibility: wrote %s payload file to %s", kind, absoluteFilePath)
	return nil
}

func resolveTestOptimizationMode() TestOptimizationMode {
	mode := TestOptimizationMode{}

	manifestRloc := strings.TrimSpace(env.Get(constants.CIVisibilityManifestFilePath))
	payloadFilesEnv := strings.TrimSpace(env.Get(constants.CIVisibilityPayloadsInFiles))
	undeclaredOutputsDir := strings.TrimSpace(env.Get(constants.CIVisibilityUndeclaredOutputsDir))
	logger.Debug("civisibility: resolving test optimization mode [manifest_env:%q payload_files_env:%q undeclared_outputs_dir:%q]",
		manifestRloc, payloadFilesEnv, undeclaredOutputsDir)

	if manifestRloc != "" {
		logger.Debug("civisibility: resolving manifest path from %q", manifestRloc)
		if manifestPath, ok := resolveManifestPath(manifestRloc); ok {
			mode.ManifestPath = manifestPath
			mode.ManifestDir = filepath.Dir(manifestPath)
			mode.ManifestEnabled = isManifestVersionSupported(manifestPath)
			logger.Debug("civisibility: resolved manifest path [path:%s manifest_enabled:%t]", absolutePathForLog(manifestPath), mode.ManifestEnabled)
		} else {
			logger.Debug("civisibility: could not resolve manifest path from %q", manifestRloc)
		}
	}

	mode.PayloadFilesEnabled = parseBoolEnv(payloadFilesEnv)
	if mode.PayloadFilesEnabled {
		if undeclaredOutputsDir != "" {
			mode.PayloadsRoot = filepath.Join(undeclaredOutputsDir, "payloads")
			logger.Debug("civisibility: payload-file mode enabled [payload_root:%s]", absolutePathForLog(mode.PayloadsRoot))
		} else {
			logger.Debug("civisibility: payload-file mode enabled but %s is empty", constants.CIVisibilityUndeclaredOutputsDir)
		}
	} else if payloadFilesEnv != "" {
		logger.Debug("civisibility: payload-file mode disabled after parsing value %q", payloadFilesEnv)
	}

	logger.Debug("civisibility: test optimization mode resolved [manifest_enabled:%t payload_files_enabled:%t manifest:%s payload_root:%s]",
		mode.ManifestEnabled, mode.PayloadFilesEnabled, absolutePathForLog(mode.ManifestPath), absolutePathForLog(mode.PayloadsRoot))
	return mode
}

func parseBoolEnv(raw string) bool {
	parsed, err := strconv.ParseBool(raw)
	return err == nil && parsed
}

func resolveManifestPath(p string) (string, bool) {
	if normalized, ok := existingFilePath(p); ok {
		logger.Debug("civisibility: resolved manifest path directly from %q to %s", p, absolutePathForLog(normalized))
		return normalized, true
	}

	if runfilesDir := strings.TrimSpace(env.Get("RUNFILES_DIR")); runfilesDir != "" {
		candidate := filepath.Join(runfilesDir, p)
		logger.Debug("civisibility: attempting manifest resolution via RUNFILES_DIR [dir:%s candidate:%s]", absolutePathForLog(runfilesDir), absolutePathForLog(candidate))
		if normalized, ok := existingFilePath(candidate); ok {
			logger.Debug("civisibility: resolved manifest path via RUNFILES_DIR to %s", absolutePathForLog(normalized))
			return normalized, true
		}
	}

	if runfilesManifest := strings.TrimSpace(env.Get("RUNFILES_MANIFEST_FILE")); runfilesManifest != "" {
		logger.Debug("civisibility: attempting manifest resolution via RUNFILES_MANIFEST_FILE [manifest:%s rlocation:%s]",
			absolutePathForLog(runfilesManifest), p)
		if candidate, ok := resolveRunfilesManifestEntry(runfilesManifest, p); ok {
			if normalized, exists := existingFilePath(candidate); exists {
				logger.Debug("civisibility: resolved manifest path via RUNFILES_MANIFEST_FILE to %s", absolutePathForLog(normalized))
				return normalized, true
			}
		}
	}

	if testSrcDir := strings.TrimSpace(env.Get("TEST_SRCDIR")); testSrcDir != "" {
		candidate := filepath.Join(testSrcDir, p)
		logger.Debug("civisibility: attempting manifest resolution via TEST_SRCDIR [dir:%s candidate:%s]", absolutePathForLog(testSrcDir), absolutePathForLog(candidate))
		if normalized, ok := existingFilePath(candidate); ok {
			logger.Debug("civisibility: resolved manifest path via TEST_SRCDIR to %s", absolutePathForLog(normalized))
			return normalized, true
		}
	}

	logger.Debug("civisibility: manifest path %q could not be resolved from direct path, RUNFILES_DIR, RUNFILES_MANIFEST_FILE, or TEST_SRCDIR", p)
	return "", false
}

func existingFilePath(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path, true
	}
	return abs, true
}

func resolveRunfilesManifestEntry(manifestFilePath string, rlocation string) (string, bool) {
	logger.Debug("civisibility: reading runfiles manifest %s for rlocation %s", absolutePathForLog(manifestFilePath), rlocation)
	file, err := os.Open(manifestFilePath)
	if err != nil {
		logger.Debug("civisibility: failed to open runfiles manifest %s: %v", absolutePathForLog(manifestFilePath), err)
		return "", false
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		splitAt := strings.IndexByte(line, ' ')
		if splitAt <= 0 {
			continue
		}
		if line[:splitAt] == rlocation {
			resolvedPath := strings.TrimSpace(line[splitAt+1:])
			logger.Debug("civisibility: runfiles manifest resolved rlocation %s to %s", rlocation, absolutePathForLog(resolvedPath))
			return resolvedPath, true
		}
	}
	if err := scanner.Err(); err != nil {
		logger.Debug("civisibility: failed while scanning runfiles manifest %s: %v", absolutePathForLog(manifestFilePath), err)
		return "", false
	}
	logger.Debug("civisibility: runfiles manifest %s did not contain rlocation %s", absolutePathForLog(manifestFilePath), rlocation)
	return "", false
}

func isManifestVersionSupported(manifestPath string) bool {
	logger.Debug("civisibility: reading manifest file %s", absolutePathForLog(manifestPath))
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		logger.Debug("civisibility: failed to read manifest file %s: %v", absolutePathForLog(manifestPath), err)
		return false
	}

	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		version := strings.TrimSpace(scanner.Text())
		if version == "" {
			continue
		}
		supported := version == "1"
		logger.Debug("civisibility: manifest file %s declared version %q [supported:%t]", absolutePathForLog(manifestPath), version, supported)
		return supported
	}
	if err := scanner.Err(); err != nil {
		logger.Debug("civisibility: failed while scanning manifest file %s: %v", absolutePathForLog(manifestPath), err)
		return false
	}
	logger.Debug("civisibility: manifest file %s did not contain a version line", absolutePathForLog(manifestPath))
	return false
}

func absolutePathForLog(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

// ResetTestOptimizationModeForTesting resets cached mode state.
// This helper is intended for tests that set per-test environment combinations.
func ResetTestOptimizationModeForTesting() {
	testOptimizationModeMu.Lock()
	defer testOptimizationModeMu.Unlock()
	testOptimizationModeOnce = sync.Once{}
	currentTestOptimization = TestOptimizationMode{}
	atomic.StoreUint64(&payloadFileCounter, 0)
}
