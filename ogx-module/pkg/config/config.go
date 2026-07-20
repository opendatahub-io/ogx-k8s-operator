package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

const (
	KeyManifestsPath      = "manifests-path"
	KeyApplicationsNS     = "applications-namespace"
	KeyPlatformName       = "platform-name"
	KeyPlatformVersion    = "platform-version"
	KeyMetricsBindAddr    = "controller.metrics.bind-address"
	KeyHealthBindAddr     = "controller.health.bind-address"
	KeyLeaderElectEnabled = "controller.leader-election.enabled"
	KeyLeaderElectID      = "controller.leader-election.id"

	DefaultManifestsPath      = "config/manifests"
	DefaultApplicationsNS     = "opendatahub"
	DefaultPlatformName       = "OpenDataHub"
	DefaultPlatformVersion    = "unknown"
	DefaultMetricsBindAddr    = ":8080"
	DefaultHealthBindAddr     = ":8081"
	DefaultLeaderElectEnabled = true
	DefaultLeaderElectID      = "opendatahub-ogx-operator-lock"

	// ConfigPathEnvVar points to the directory where the projected module
	// configuration ConfigMap is mounted.
	ConfigPathEnvVar = "ODH_MODULE_OPERATOR_CONFIGURATION_PATH"

	// EnvPrefix is the prefix used for environment variable overrides.
	EnvPrefix = "ODH_MODULE_OPERATOR"
)

var structuredExtensions = map[string]bool{
	"yaml": true,
	"yml":  true,
	"json": true,
}

// Config contains the runtime settings for the OGX module controller.
//
// Values are loaded from defaults, then projected config files, then
// ODH_MODULE_OPERATOR_* environment variables.
type Config struct {
	ManifestsPath         string           `mapstructure:"manifests-path"`
	ApplicationsNamespace string           `mapstructure:"applications-namespace"`
	PlatformName          string           `mapstructure:"platform-name"`
	PlatformVersion       string           `mapstructure:"platform-version"`
	Controller            ControllerConfig `mapstructure:"controller"`
}

type ControllerConfig struct {
	Metrics        MetricsConfig        `mapstructure:"metrics"`
	Health         HealthConfig         `mapstructure:"health"`
	LeaderElection LeaderElectionConfig `mapstructure:"leader-election"`
}

type MetricsConfig struct {
	BindAddress string `mapstructure:"bind-address"`
}

type HealthConfig struct {
	BindAddress string `mapstructure:"bind-address"`
}

type LeaderElectionConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	ID      string `mapstructure:"id"`
}

// Load reads config from the mounted config path (if present) plus env vars.
func Load() (*Config, error) {
	var configFS fs.FS

	if configPath := os.Getenv(ConfigPathEnvVar); configPath != "" {
		configFS = os.DirFS(configPath)
	}

	return LoadFromFS(configFS)
}

// LoadFromFS reads config from fsys plus environment variables.
func LoadFromFS(fsys fs.FS) (*Config, error) {
	v := viper.New()
	setDefaults(v)

	// The ODH operator injects "platformVersion" (camelCase) into the
	// platform ConfigMap. Viper lowercases it to "platformversion". This
	// alias maps it to the canonical kebab-case key so it populates
	// Config.PlatformVersion regardless of which key name was used.
	v.RegisterAlias("platformversion", KeyPlatformVersion)

	if fsys != nil {
		if err := loadFromFS(v, fsys); err != nil {
			return nil, fmt.Errorf("failed to load config from filesystem: %w", err)
		}
	}

	if err := bindEnv(v); err != nil {
		return nil, fmt.Errorf("failed to bind env vars: %w", err)
	}

	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	return cfg, nil
}

func setDefaults(v *viper.Viper) {
	applicationsNamespace := os.Getenv("APPLICATIONS_NAMESPACE")
	if applicationsNamespace == "" {
		applicationsNamespace = os.Getenv("RHAI_APPLICATIONS_NAMESPACE")
	}
	if applicationsNamespace == "" {
		applicationsNamespace = DefaultApplicationsNS
	}

	v.SetDefault(KeyManifestsPath, DefaultManifestsPath)
	v.SetDefault(KeyApplicationsNS, applicationsNamespace)
	v.SetDefault(KeyPlatformName, DefaultPlatformName)
	v.SetDefault(KeyPlatformVersion, DefaultPlatformVersion)
	v.SetDefault(KeyMetricsBindAddr, DefaultMetricsBindAddr)
	v.SetDefault(KeyHealthBindAddr, DefaultHealthBindAddr)
	v.SetDefault(KeyLeaderElectEnabled, DefaultLeaderElectEnabled)
	v.SetDefault(KeyLeaderElectID, DefaultLeaderElectID)
}

func bindEnv(v *viper.Viper) error {
	v.SetEnvPrefix(EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	v.AutomaticEnv()

	for _, key := range v.AllKeys() {
		if err := v.BindEnv(key); err != nil {
			return fmt.Errorf("failed to bind env for key %s: %w", key, err)
		}
	}

	return nil
}

func loadFromFS(v *viper.Viper, fsys fs.FS) error {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return fmt.Errorf("failed to read config directory: %w", err)
	}

	tmp := viper.New()

	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		data, err := fs.ReadFile(fsys, entry.Name())
		if err != nil {
			continue
		}

		ext := strings.TrimPrefix(filepath.Ext(entry.Name()), ".")
		if structuredExtensions[ext] {
			if err := mergeStructuredFile(tmp, entry.Name(), ext, data); err != nil {
				return err
			}
		} else {
			tmp.Set(entry.Name(), strings.TrimSpace(string(data)))
		}
	}

	if err := v.MergeConfigMap(tmp.AllSettings()); err != nil {
		return fmt.Errorf("failed to merge config from filesystem: %w", err)
	}

	return nil
}

func mergeStructuredFile(v *viper.Viper, name, ext string, data []byte) error {
	fv := viper.New()
	fv.SetConfigType(ext)

	if err := fv.ReadConfig(strings.NewReader(string(data))); err != nil {
		return fmt.Errorf("failed to parse config file %s: %w", name, err)
	}

	if err := v.MergeConfigMap(fv.AllSettings()); err != nil {
		return fmt.Errorf("failed to merge config from %s: %w", name, err)
	}

	return nil
}
