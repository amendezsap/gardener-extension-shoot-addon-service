package cmd

import (
	controllercmd "github.com/gardener/gardener/extensions/pkg/controller/cmd"
	addonctrl "github.com/amendezsap/gardener-extension-shoot-addon-service/pkg/controller/addon"
)

func ControllerSwitches() *controllercmd.SwitchOptions {
	return controllercmd.NewSwitchOptions(
		controllercmd.Switch(addonctrl.ControllerName, addonctrl.AddToManager),
	)
}
