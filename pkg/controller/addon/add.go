package addon

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/gardener/gardener/extensions/pkg/controller/extension"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
)

const (
	Type           = "shoot-addon-service"
	ControllerName = "shoot-addon-service"
)

var DefaultAddOptions = AddOptions{}

type AddOptions struct {
	Controller                controller.Options
	IgnoreOperationAnnotation bool
	ExtensionClasses          []extensionsv1alpha1.ExtensionClass
}

func AddToManager(ctx context.Context, mgr manager.Manager) error {
	return AddToManagerWithOptions(ctx, mgr, DefaultAddOptions)
}

func AddToManagerWithOptions(ctx context.Context, mgr manager.Manager, opts AddOptions) error {
	return extension.Add(mgr, extension.AddArgs{
		Actuator:          NewActuator(mgr),
		ControllerOptions: opts.Controller,
		Name:              ControllerName,
		FinalizerSuffix:   Type,
		Resync:            60 * time.Minute,
		Predicates:        extension.DefaultPredicates(ctx, mgr, opts.IgnoreOperationAnnotation),
		Type:              Type,
		ExtensionClasses:  opts.ExtensionClasses,
	})
}
