package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

func TestEndpointConfigurationPrecedence(t *testing.T) {
	tests := []struct {
		name         string
		key          string
		flag         string
		defaultValue string
		environment  string
		configValue  string
		envValue     string
		flagValue    string
	}{
		{
			name:         "Fast-Path endpoint",
			key:          "endpoint",
			flag:         "endpoint",
			defaultValue: "localhost:9090",
			environment:  fastPathEndpointEnv,
			configValue:  "config-fastpath:19090",
			envValue:     "env-fastpath:19090",
			flagValue:    "flag-fastpath:29090",
		},
		{
			name:        "Sandbox Proxy endpoint",
			key:         "proxy-endpoint",
			flag:        "proxy-endpoint",
			environment: sandboxProxyEndpointEnv,
			configValue: "http://config-proxy:18080",
			envValue:    "http://env-proxy:18080",
			flagValue:   "http://flag-proxy:28080",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(test.environment, "")

			flags := pflag.NewFlagSet("fastctl-test", pflag.ContinueOnError)
			flags.String("endpoint", "localhost:9090", "")
			flags.String("namespace", "fast-sandbox", "")
			flags.String("proxy-endpoint", "", "")
			config := viper.New()
			if err := bindConfigSources(config, flags); err != nil {
				t.Fatalf("bind config sources: %v", err)
			}

			if got := config.GetString(test.key); got != test.defaultValue {
				t.Fatalf("default value = %q, want %q", got, test.defaultValue)
			}
			config.SetConfigType("json")
			configDocument := `{"` + test.key + `":"` + test.configValue + `"}`
			if err := config.ReadConfig(strings.NewReader(configDocument)); err != nil {
				t.Fatalf("read config: %v", err)
			}
			if got := config.GetString(test.key); got != test.configValue {
				t.Fatalf("config file value = %q, want %q", got, test.configValue)
			}
			t.Setenv(test.environment, test.envValue)
			if got := config.GetString(test.key); got != test.envValue {
				t.Fatalf("environment value = %q, want %q", got, test.envValue)
			}
			if err := flags.Set(test.flag, test.flagValue); err != nil {
				t.Fatalf("set --%s: %v", test.flag, err)
			}
			if got := config.GetString(test.key); got != test.flagValue {
				t.Fatalf("flag value = %q, want %q", got, test.flagValue)
			}
		})
	}
}
