package config

import (
	"encoding/json"
	"os"
	"time"

	"sigs.k8s.io/yaml"
)

// Duration is a wrapper around time.Duration that supports YAML/JSON string parsing
type Duration time.Duration

// UnmarshalJSON implements json.Unmarshaler for Duration
func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		// Try unmarshaling as a number (nanoseconds)
		var ns int64
		if numErr := json.Unmarshal(data, &ns); numErr != nil {
			return err
		}
		*d = Duration(ns)
		return nil
	}

	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// Duration returns the time.Duration value
func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// Config holds the autoscaler configuration
type Config struct {
	// OmniEndpoint is the Omni API endpoint URL
	OmniEndpoint string `json:"omniEndpoint"`
	// ClusterName is the name of the cluster in Omni
	ClusterName string `json:"clusterName"`
	// MachineSets defines the scaling configuration per machine set
	MachineSets []MachineSetConfig `json:"machineSets"`
	// Cooldowns defines the cooldown periods for scaling operations
	Cooldowns CooldownConfig `json:"cooldowns"`
	// SyncInterval is how often to check for scaling needs
	SyncInterval Duration `json:"syncInterval"`
	// IgnoredNamespaces lists namespaces to ignore when counting pending pods
	// This prevents feedback loops from system pods on new nodes
	IgnoredNamespaces []string `json:"ignoredNamespaces"`
}

// MachineSetConfig defines scaling parameters for a single machine set
type MachineSetConfig struct {
	// Name is the machine set name in Omni
	Name string `json:"name"`
	// MinSize is the minimum number of machines
	MinSize int `json:"minSize"`
	// MaxSize is the maximum number of machines
	MaxSize int `json:"maxSize"`
	// ScaleUpPendingPods triggers scale-up when this many pods are pending
	ScaleUpPendingPods int `json:"scaleUpPendingPods"`
	// ScaleDownUtilization triggers scale-down when node utilization is below this (legacy, used as fallback)
	ScaleDownUtilization float64 `json:"scaleDownUtilization"`
	// TargetUtilization is the desired cluster utilization (0.0-1.0)
	// Scale up proactively when utilization exceeds this, scale down when below
	TargetUtilization float64 `json:"targetUtilization"`
	// ScaleUpThreshold triggers proactive scale-up when utilization exceeds this (0.0-1.0)
	// Defaults to TargetUtilization + 0.1 if not set
	ScaleUpThreshold float64 `json:"scaleUpThreshold"`
	// ScaleDownThreshold triggers scale-down evaluation when utilization is below this (0.0-1.0)
	// Defaults to TargetUtilization - 0.15 if not set
	ScaleDownThreshold float64 `json:"scaleDownThreshold"`
	// SafeToEvictBuffer is extra capacity (0.0-1.0) required on remaining nodes before scale-down
	// E.g., 0.1 means nodes must have 10% extra capacity after consolidation
	SafeToEvictBuffer float64 `json:"safeToEvictBuffer"`
}

// CooldownConfig defines cooldown periods between scaling operations
type CooldownConfig struct {
	// ScaleUp is the cooldown after a scale-up operation
	ScaleUp Duration `json:"scaleUp"`
	// ScaleDown is the cooldown after a scale-down operation
	ScaleDown Duration `json:"scaleDown"`
}

// LoadFromFile loads configuration from a YAML file
func LoadFromFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	// Set defaults
	if cfg.SyncInterval == 0 {
		cfg.SyncInterval = Duration(30 * time.Second)
	}
	if cfg.Cooldowns.ScaleUp == 0 {
		cfg.Cooldowns.ScaleUp = Duration(2 * time.Minute)
	}
	if cfg.Cooldowns.ScaleDown == 0 {
		cfg.Cooldowns.ScaleDown = Duration(10 * time.Minute)
	}
	if len(cfg.IgnoredNamespaces) == 0 {
		cfg.IgnoredNamespaces = []string{
			"kube-system",
			"kube-public",
			"kube-node-lease",
			"flux-system",
		}
	}

	// Set defaults for machine set configs
	for i := range cfg.MachineSets {
		ms := &cfg.MachineSets[i]
		if ms.TargetUtilization == 0 {
			ms.TargetUtilization = 0.5 // Default 50% target utilization
		}
		if ms.ScaleUpThreshold == 0 {
			ms.ScaleUpThreshold = ms.TargetUtilization + 0.15 // Default: target + 15%
		}
		if ms.ScaleDownThreshold == 0 {
			// Use legacy ScaleDownUtilization if set, otherwise target - 15%
			if ms.ScaleDownUtilization > 0 {
				ms.ScaleDownThreshold = ms.ScaleDownUtilization
			} else {
				ms.ScaleDownThreshold = ms.TargetUtilization - 0.15
			}
		}
		if ms.SafeToEvictBuffer == 0 {
			ms.SafeToEvictBuffer = 0.1 // Default 10% buffer
		}
	}

	return &cfg, nil
}

// GetMachineSetConfig returns the config for a specific machine set
func (c *Config) GetMachineSetConfig(name string) *MachineSetConfig {
	for i := range c.MachineSets {
		if c.MachineSets[i].Name == name {
			return &c.MachineSets[i]
		}
	}
	return nil
}
