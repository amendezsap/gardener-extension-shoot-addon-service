package app

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	extensionscontroller "github.com/gardener/gardener/extensions/pkg/controller"

	addonctrl "github.com/amendezsap/gardener-extension-shoot-addon-service/pkg/controller/addon"
)

var leaderElect bool

func NewControllerManagerCommand(ctx context.Context, log logr.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gardener-extension-shoot-addon-service",
		Short: "Gardener extension that deploys user-defined Helm chart addons to shoot clusters",
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(ctx, log)
		},
	}
	cmd.Flags().BoolVar(&leaderElect, "leader-elect", true, "Enable leader election")
	return cmd
}

func run(ctx context.Context, log logr.Logger) error {
	log.Info("Starting gardener-extension-shoot-addon-service")

	mgr, err := manager.New(ctrl.GetConfigOrDie(), manager.Options{
		LeaderElection:         leaderElect,
		LeaderElectionID:       "gardener-extension-shoot-addon-service",
		HealthProbeBindAddress: ":8081",
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("add readyz: %w", err)
	}

	if err := extensionscontroller.AddToScheme(mgr.GetScheme()); err != nil {
		return fmt.Errorf("add schemes: %w", err)
	}

	// Addon controller (reads manifest, deploys charts as ManagedResources)
	if err := addonctrl.AddToManager(ctx, mgr); err != nil {
		return fmt.Errorf("add addon controller: %w", err)
	}

	log.Info("Starting manager")
	return mgr.Start(ctx)
}
