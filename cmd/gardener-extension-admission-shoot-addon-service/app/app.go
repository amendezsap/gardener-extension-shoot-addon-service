package app

import (
	"context"
	"fmt"
	"os"

	extensionscmdcontroller "github.com/gardener/gardener/extensions/pkg/controller/cmd"
	extensionswebhook "github.com/gardener/gardener/extensions/pkg/webhook"
	extensionscmdwebhook "github.com/gardener/gardener/extensions/pkg/webhook/cmd"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	grmwebhook "github.com/amendezsap/gardener-extension-shoot-addon-service/pkg/webhook/grm"
)

// Name is used by the Gardener webhook framework to generate resource names:
//   Service:    "gardener-extension-<Name>"
//   Cert label: "ca-<Name>-webhook-bundle"
//
// The cert label is a Kubernetes label value (max 63 chars).
// Formula: "ca-" + Name + "-webhook-bundle" = len(Name) + 18
// Max Name length: 45 chars. Current: 30 chars.
//
// This MUST match admission.webhookName in the Helm chart values.yaml.
const Name = "extension-admission-shoot-addon"

func NewControllerManagerCommand(ctx context.Context) *cobra.Command {
	var (
		restOpts = &extensionscmdcontroller.RESTOptions{}
		mgrOpts  = &extensionscmdcontroller.ManagerOptions{
			LeaderElection:          true,
			LeaderElectionID:        extensionscmdcontroller.LeaderElectionNameID(Name),
			LeaderElectionNamespace: os.Getenv("LEADER_ELECTION_NAMESPACE"),
			WebhookServerPort:       10250,
			HealthBindAddress:       ":8081",
			WebhookCertDir:          "/tmp/admission-shoot-addon-service-cert",
		}
		generalOptions = &extensionscmdcontroller.GeneralOptions{}
		// options for the webhook server
		webhookServerOptions = &extensionscmdwebhook.ServerOptions{
			Namespace: os.Getenv("WEBHOOK_CONFIG_NAMESPACE"),
		}
		webhookSwitches = extensionscmdwebhook.NewSwitchOptions(
			extensionscmdwebhook.Switch(grmwebhook.WebhookName, func(mgr manager.Manager) (*extensionswebhook.Webhook, error) {
				return grmwebhook.AddToManager(mgr)
			}),
		)
		webhookOptions = extensionscmdwebhook.NewAddToManagerOptions(
			Name,
			"",
			nil,
			generalOptions,
			webhookServerOptions,
			webhookSwitches,
		)

		aggOption = extensionscmdcontroller.NewOptionAggregator(
			restOpts,
			mgrOpts,
			generalOptions,
			webhookOptions,
		)
	)

	cmd := &cobra.Command{
		Use: "admission-shoot-addon-service",

		RunE: func(_ *cobra.Command, _ []string) error {
			log := logf.Log.WithName(Name)
			log.Info("Starting " + Name)

			if err := aggOption.Complete(); err != nil {
				return fmt.Errorf("error completing options: %w", err)
			}

			managerOptions := mgrOpts.Completed().Options()

			// Restrict secret cache to the webhook namespace to avoid
			// cluster-wide list/watch permissions.
			webhookNS := webhookOptions.Server.Completed().Namespace
			if webhookNS == "" {
				webhookNS = os.Getenv("LEADER_ELECTION_NAMESPACE")
			}
			if webhookNS == "" {
				webhookNS = "garden"
			}
			managerOptions.Cache = cache.Options{
				ByObject: map[client.Object]cache.ByObject{
					&corev1.Secret{}: {Namespaces: map[string]cache.Config{webhookNS: {}}},
				},
			}

			mgr, err := manager.New(restOpts.Completed().Config, managerOptions)
			if err != nil {
				return fmt.Errorf("could not instantiate manager: %w", err)
			}

			log.Info("Setting up webhook server")
			if _, err := webhookOptions.Completed().AddToManager(ctx, mgr, nil); err != nil {
				return err
			}

			if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
				return fmt.Errorf("could not add healthcheck: %w", err)
			}
			if err := mgr.AddReadyzCheck("webhook-server", mgr.GetWebhookServer().StartedChecker()); err != nil {
				return fmt.Errorf("could not add readycheck of webhook to manager: %w", err)
			}

			return mgr.Start(ctx)
		},
	}
	aggOption.AddFlags(cmd.Flags())

	return cmd
}
