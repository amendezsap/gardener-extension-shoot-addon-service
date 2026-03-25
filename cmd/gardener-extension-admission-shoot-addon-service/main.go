package main

import (
	"os"

	"github.com/gardener/gardener/pkg/logger"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	"github.com/amendezsap/gardener-extension-shoot-addon-service/cmd/gardener-extension-admission-shoot-addon-service/app"
)

func main() {
	logf.SetLogger(logger.MustNewZapLogger(logger.InfoLevel, logger.FormatJSON))

	if err := app.NewControllerManagerCommand(signals.SetupSignalHandler()).Execute(); err != nil {
		logf.Log.Error(err, "Error executing the main controller command")
		os.Exit(1)
	}
}
