package main

import (
	"os"

	"github.com/gardener/gardener/pkg/logger"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	"github.com/amendezsap/gardener-extension-shoot-addon-service/cmd/gardener-extension-shoot-addon-service/app"
)

func main() {
	zapLogger := logger.MustNewZapLogger(logger.InfoLevel, logger.FormatJSON)
	log.SetLogger(zapLogger)

	ctx := signals.SetupSignalHandler()
	if err := app.NewControllerManagerCommand(ctx, zapLogger).Execute(); err != nil {
		zapLogger.Error(err, "error running controller manager")
		os.Exit(1)
	}
}
