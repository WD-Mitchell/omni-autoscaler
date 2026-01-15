package config

import (
	"os"
	"time"

	"sigs.k8s.io/yaml"
)

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
	SyncInterval time.Duration `json:"syncInterval"`
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
	// ScaleDownUtilization triggers scale-down when node utilization is below this
	ScaleDownUtilization float64 `json:"scaleDownUtilization"`
}

// CooldownConfig defines cooldown periods between scaling operations
type CooldownConfig struct {
	// ScaleUp is the cooldown after a scale-up operation
	ScaleUp time.Duration `json:"scaleUp"`
	// ScaleDown is the cooldown after a scale-down operation
	ScaleDown time.Duration `json:"scaleDown"`
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
		cfg.SyncInterval = 30 * time.Second
	}
	if cfg.Cooldowns.ScaleUp == 0 {
		cfg.Cooldowns.ScaleUp = 2 * time.Minute
	}
	if cfg.Cooldowns.ScaleDown == 0 {
		cfg.Cooldowns.ScaleDown = 10 * time.Minute
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
