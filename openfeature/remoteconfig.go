// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025 Datadog, Inc.

package openfeature

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	rc "github.com/DataDog/datadog-agent/pkg/remoteconfig/state"

	"github.com/DataDog/dd-trace-go/v2/internal/log"
	internalffe "github.com/DataDog/dd-trace-go/v2/internal/openfeature"
	"github.com/DataDog/dd-trace-go/v2/internal/remoteconfig"
)

func startWithRemoteConfig(config ProviderConfig) (*DatadogProvider, error) {
	provider := newDatadogProvider(config)

	// Subscribe via the internal package, which serializes with tracer subscription
	// and starts RC only if needed (slow path).
	tracerOwnsSubscription, err := internalffe.SubscribeProvider(provider.rcCallback)
	if err != nil {
		return nil, fmt.Errorf("failed to subscribe to Remote Config: %w", err)
	}

	if !tracerOwnsSubscription {
		log.Debug("openfeature: successfully subscribed to Remote Config updates")
		return provider, nil
	}
	if !attachProvider(provider) {
		// This shouldn't happen since SubscribeProvider just told us tracer subscribed.
		return nil, fmt.Errorf("failed to attach to tracer's RC subscription")
	}
	log.Debug("openfeature: attached to tracer's RC subscription")
	return provider, nil
}

func (p *DatadogProvider) rcCallback(update remoteconfig.ProductUpdate) map[string]rc.ApplyStatus {
	statuses := make(map[string]rc.ApplyStatus, len(update))
	parsed := make(map[string]*universalFlagsConfiguration, len(update))

	// Process each configuration file in the update
	for path, data := range update {
		config, status := processConfigUpdate(path, data)
		statuses[path] = status
		if status.State == rc.ApplyStateAcknowledged {
			parsed[path] = config
		}
	}

	if len(parsed) > 0 {
		p.applyConfigUpdate(parsed)
	}

	return statuses
}

// processConfigUpdate parses and validates a single configuration update.
// Returns the parsed config (nil for deletions) and the apply status.
func processConfigUpdate(path string, data []byte) (*universalFlagsConfiguration, rc.ApplyStatus) {
	if data == nil {
		log.Debug("openfeature: remote config: removing configuration %q", path)
		return nil, rc.ApplyStatus{State: rc.ApplyStateAcknowledged}
	}

	// Parse the configuration
	log.Debug("openfeature: remote config: processing configuration update %q", path)

	var config universalFlagsConfiguration
	if err := json.Unmarshal(data, &config); err != nil {
		log.Error("openfeature: remote config: failed to unmarshal configuration %q: %v", path, err.Error())
		return nil, rc.ApplyStatus{
			State: rc.ApplyStateError,
			Error: fmt.Sprintf("failed to unmarshal configuration: %v", err),
		}
	}

	// Validate the configuration
	err := validateConfiguration(&config)
	if err != nil {
		log.Error("openfeature: remote config: invalid configuration %q: %v", path, err.Error())
		return nil, rc.ApplyStatus{
			State: rc.ApplyStateError,
			Error: fmt.Sprintf("invalid configuration: %v", err),
		}
	}

	log.Debug("openfeature: remote config: successfully parsed configuration %q with %d flags", path, len(config.Flags))
	return &config, rc.ApplyStatus{State: rc.ApplyStateAcknowledged}
}

// applyConfigUpdate atomically applies a batch of parsed config updates.
// Each entry maps an RC path to a parsed config (nil means deletion).
func (p *DatadogProvider) applyConfigUpdate(update map[string]*universalFlagsConfiguration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for path, config := range update {
		if config == nil {
			delete(p.configState, path)
		} else {
			p.configState[path] = config
		}
	}
	p.configuration = p.mergeConfigurations()
	p.configChange.Broadcast()
}

// mergeConfigurations rebuilds the unified configuration from all entries in configState.
// Only Format and Flags are used by the evaluation engine; other metadata fields are dropped.
// If two configs share a flag key, last-write-wins (nondeterministic map iteration order).
// Must be called with p.mu held.
func (p *DatadogProvider) mergeConfigurations() *universalFlagsConfiguration {
	if len(p.configState) == 0 {
		return nil
	}
	merged := &universalFlagsConfiguration{
		Format: "SERVER",
		Flags:  make(map[string]*flag),
	}
	for _, config := range p.configState {
		maps.Copy(merged.Flags, config.Flags)
	}
	return merged
}

// validateConfiguration performs basic validation on a serverConfiguration.
func validateConfiguration(config *universalFlagsConfiguration) error {
	if config == nil {
		return fmt.Errorf("configuration is nil")
	}

	if config.Format != "SERVER" {
		return fmt.Errorf("unsupported format %q, expected SERVER (Is the remote config payload the right format ?)", config.Format)
	}

	hasFlags := len(config.Flags) > 0

	// Validate each flag and delete invalid ones from the map
	// Collect errors for reporting
	errs := make([]error, 0, len(config.Flags))
	maps.DeleteFunc(config.Flags, func(flagKey string, flag *flag) bool {
		err := validateFlag(flagKey, flag)
		errs = append(errs, err)
		return err != nil
	})

	if hasFlags && len(config.Flags) == 0 {
		errs = append(errs, errors.New("all flags are invalid"))
	}

	return errors.Join(errs...)
}

func validateFlag(flagKey string, flag *flag) error {
	if flag == nil {
		return fmt.Errorf("flag %q is nil", flagKey)
	}

	if flag.Key != flagKey {
		return fmt.Errorf("flag key mismatch: map key %q != flag.Key %q", flagKey, flag.Key)
	}

	// Validate variation type
	switch flag.VariationType {
	case valueTypeBoolean, valueTypeString, valueTypeInteger, valueTypeNumeric, valueTypeJSON:
		// Valid types
	default:
		return fmt.Errorf("flag %q has invalid variation type %q", flagKey, flag.VariationType)
	}

	for i, allocation := range flag.Allocations {
		if allocation == nil {
			return fmt.Errorf("flag %q allocation %d is nil", flagKey, i)
		}

		for j, split := range allocation.Splits {
			if split == nil {
				return fmt.Errorf("flag %q allocation %d split %d is nil", flagKey, i, j)
			}

			for _, shard := range split.Shards {
				if shard.TotalShards < 0 {
					return fmt.Errorf("flag %q allocation %d split %d has shard with non-positive TotalShards %d",
						flagKey, i, j, shard.TotalShards)
				}
			}

			if _, exists := flag.Variations[split.VariationKey]; !exists {
				return fmt.Errorf("flag %q allocation %d split %d references non-existent variation %q",
					flagKey, i, j, split.VariationKey)
			}
		}

		for _, rule := range allocation.Rules {
			if rule == nil {
				return fmt.Errorf("flag %q allocation %d has nil rule", flagKey, i)
			}

			for _, condition := range rule.Conditions {
				if condition == nil {
					return fmt.Errorf("flag %q allocation %d rule has nil condition", flagKey, i)
				}

				if condition.Operator == operatorMatches || condition.Operator == operatorNotMatches {
					regex, ok := condition.Value.(string)
					if !ok {
						return fmt.Errorf("flag %q allocation %d rule has condition with operator %q that requires string value",
							flagKey, i, condition.Operator)
					}

					if _, err := loadRegex(regex); err != nil {
						return fmt.Errorf("flag %q allocation %d rule has condition with invalid regex %q: %v",
							flagKey, i, regex, err)
					}
				}
			}
		}
	}
	return nil
}

// stopRemoteConfig unsubscribes from Remote Config updates.
// This should be called when shutting down the application or when
// the OpenFeature provider is no longer needed.
//
// Note: In the slow path, this package discards the subscription token from
// Subscribe(), so we cannot call Unsubscribe(). Instead we unregister the
// capability which stops updates. In the fast path (tracer subscribed),
// the subscription is owned by the tracer.
func stopRemoteConfig() error {
	log.Debug("openfeature: unregistered from Remote Config")
	_ = remoteconfig.UnregisterCapability(remoteconfig.FFEFlagEvaluation)
	return nil
}
