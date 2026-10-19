package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"oras.land/oras-go/v2/registry/remote/auth"

	"fast-sandbox/internal/ocitemplate"
)

func newPushCommand() *cobra.Command {
	var dir, destination, metadata, authFile string
	cmd := &cobra.Command{Use: "push", Short: "Publish a built template pair and metadata to an OCI registry", RunE: func(cmd *cobra.Command, _ []string) error {
		raw, err := os.ReadFile(authFile)
		if err != nil {
			return err
		}
		cfg, err := parseDockerConfig(string(raw))
		if err != nil {
			return err
		}
		ref, err := parseRegistryReference(destination)
		if err != nil {
			return err
		}
		payload, err := os.ReadFile(metadata)
		if err != nil {
			return err
		}
		credential, _, err := credentialFor(cfg, ref)
		if err != nil {
			return err
		}
		manifestRef, digest, err := ocitemplate.Push(cmd.Context(), dir, destination, payload, func(_ context.Context, host string) (auth.Credential, error) {
			if host != ref.Registry {
				return auth.EmptyCredential, nil
			}
			return credential, nil
		})
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"manifestRef": manifestRef, "artifactDigest": digest})
	}}
	f := cmd.Flags()
	f.StringVar(&dir, "artifact-dir", "", "local build artifact directory")
	f.StringVar(&destination, "destination", "", "registry/repository:tag")
	f.StringVar(&metadata, "metadata-file", "", "template metadata JSON")
	f.StringVar(&authFile, "auth-file", "", "Docker authentication JSON")
	for _, name := range []string{"artifact-dir", "destination", "metadata-file", "auth-file"} {
		if err := cmd.MarkFlagRequired(name); err != nil {
			panic(fmt.Sprint(err))
		}
	}
	return cmd
}
