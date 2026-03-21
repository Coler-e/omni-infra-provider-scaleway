// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package main is the root cmd of the provider script.
package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/scaleway/scaleway-sdk-go/scw"
	"github.com/siderolabs/omni/client/pkg/client"
	"github.com/siderolabs/omni/client/pkg/infra"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/siderolabs/omni-infra-provider-scaleway/internal/pkg/provider"
	"github.com/siderolabs/omni-infra-provider-scaleway/internal/pkg/provider/data"
	"github.com/siderolabs/omni-infra-provider-scaleway/internal/pkg/provider/meta"
)

//go:embed data/icon.svg
var icon []byte

// rootCmd represents the base command when called without any subcommands.
var rootCmd = &cobra.Command{
	Use:          "omni-infra-provider-scaleway",
	Short:        "Scaleway Omni infrastructure provider",
	Long:         `Connects to Omni as an infra provider and manages instances in Scaleway`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		loggerConfig := zap.NewProductionConfig()

		logger, err := loggerConfig.Build(
			zap.AddStacktrace(zapcore.ErrorLevel),
		)
		if err != nil {
			return fmt.Errorf("failed to create logger: %w", err)
		}

		if cfg.omniAPIEndpoint == "" {
			return fmt.Errorf("omni-api-endpoint flag is not set")
		}

		if cfg.scalewayAccessKey == "" {
			return fmt.Errorf("scaleway-access-key is required (set --scaleway-access-key or SCW_ACCESS_KEY)")
		}

		if cfg.scalewaySecretKey == "" {
			return fmt.Errorf("scaleway-secret-key is required (set --scaleway-secret-key or SCW_SECRET_KEY)")
		}

		if cfg.scalewayProjectID == "" {
			return fmt.Errorf("scaleway-project-id is required (set --scaleway-project-id or SCW_DEFAULT_PROJECT_ID)")
		}

		scwClient, err := scw.NewClient(
			scw.WithAuth(cfg.scalewayAccessKey, cfg.scalewaySecretKey),
			scw.WithDefaultProjectID(cfg.scalewayProjectID),
		)
		if err != nil {
			return fmt.Errorf("failed to create Scaleway client: %w", err)
		}

		provisioner := provider.NewProvisioner(scwClient, cfg.scalewayDefaultZone)

		ip, err := infra.NewProvider(meta.ProviderID, provisioner, infra.ProviderConfig{
			Name:        cfg.providerName,
			Description: cfg.providerDescription,
			Icon:        base64.RawStdEncoding.EncodeToString(icon),
			Schema:      string(data.Schema),
		})
		if err != nil {
			return fmt.Errorf("failed to create infra provider: %w", err)
		}

		logger.Info("starting Scaleway infra provider")

		clientOptions := []client.Option{
			client.WithInsecureSkipTLSVerify(cfg.insecureSkipVerify),
		}

		if cfg.serviceAccountKey != "" {
			clientOptions = append(clientOptions, client.WithServiceAccount(cfg.serviceAccountKey))
		}

		return ip.Run(cmd.Context(), logger,
			infra.WithOmniEndpoint(cfg.omniAPIEndpoint),
			infra.WithClientOptions(clientOptions...),
			infra.WithEncodeRequestIDsIntoTokens(),
		)
	},
}

var cfg struct {
	omniAPIEndpoint     string
	serviceAccountKey   string
	providerName        string
	providerDescription string
	scalewayAccessKey   string
	scalewaySecretKey   string
	scalewayProjectID   string
	scalewayDefaultZone string
	insecureSkipVerify  bool
}

func main() {
	if err := app(); err != nil {
		os.Exit(1)
	}
}

func app() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	defer cancel()

	return rootCmd.ExecuteContext(ctx)
}

func init() {
	rootCmd.Flags().StringVar(&cfg.omniAPIEndpoint, "omni-api-endpoint", os.Getenv("OMNI_ENDPOINT"),
		"the endpoint of the Omni API, if not set, defaults to OMNI_ENDPOINT env var.")
	rootCmd.Flags().StringVar(&meta.ProviderID, "id", meta.ProviderID,
		"the id of the infra provider, it is used to match the resources with the infra provider label.")
	rootCmd.Flags().StringVar(&cfg.serviceAccountKey, "omni-service-account-key", os.Getenv("OMNI_SERVICE_ACCOUNT_KEY"),
		"Omni service account key, if not set, defaults to OMNI_SERVICE_ACCOUNT_KEY.")
	rootCmd.Flags().StringVar(&cfg.providerName, "provider-name", "Scaleway", "provider name as it appears in Omni")
	rootCmd.Flags().StringVar(&cfg.providerDescription, "provider-description", "Scaleway infrastructure provider",
		"provider description as it appears in Omni")
	rootCmd.Flags().StringVar(&cfg.scalewayAccessKey, "scaleway-access-key", os.Getenv("SCW_ACCESS_KEY"),
		"Scaleway access key, if not set, defaults to SCW_ACCESS_KEY env var.")
	rootCmd.Flags().StringVar(&cfg.scalewaySecretKey, "scaleway-secret-key", os.Getenv("SCW_SECRET_KEY"),
		"Scaleway secret key, if not set, defaults to SCW_SECRET_KEY env var.")
	rootCmd.Flags().StringVar(&cfg.scalewayProjectID, "scaleway-project-id", os.Getenv("SCW_DEFAULT_PROJECT_ID"),
		"Scaleway project ID, if not set, defaults to SCW_DEFAULT_PROJECT_ID env var.")
	rootCmd.Flags().StringVar(&cfg.scalewayDefaultZone, "scaleway-default-zone", os.Getenv("SCW_DEFAULT_ZONE"),
		"Default Scaleway zone (e.g. fr-par-1). Can be overridden per machine class. Defaults to SCW_DEFAULT_ZONE env var.")
	rootCmd.Flags().BoolVar(&cfg.insecureSkipVerify, "insecure-skip-verify", false,
		"ignores untrusted certs on Omni side")
}
