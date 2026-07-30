package cmd

import (
	fastpathv2 "fast-sandbox/api/proto/v2"
	"fast-sandbox/pkg/sandboxclient"

	"github.com/spf13/viper"
)

func newOpenSandboxExecd(client fastpathv2.FastPathServiceClient) *sandboxclient.OpenSandboxExecd {
	resolver := &sandboxclient.EndpointResolver{
		Control: client, DefaultNamespace: viper.GetString("namespace"), ProxyBaseURL: viper.GetString("proxy-endpoint"),
	}
	return &sandboxclient.OpenSandboxExecd{Resolver: resolver, ComponentName: openSandboxComponentName}
}

func sandboxReference(name string) sandboxclient.SandboxRef {
	return sandboxclient.SandboxRef{Name: name, Namespace: viper.GetString("namespace")}
}
