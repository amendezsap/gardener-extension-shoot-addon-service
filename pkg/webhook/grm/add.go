package grm

import (
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	extensionswebhook "github.com/gardener/gardener/extensions/pkg/webhook"
)

func AddToManager(mgr manager.Manager) (*extensionswebhook.Webhook, error) {
	failurePolicy := admissionregistrationv1.Ignore
	wh, err := extensionswebhook.New(mgr, extensionswebhook.Args{
		Provider: WebhookName,
		Name:     WebhookName,
		Path:     "/mutate-configmaps",
		Target:   extensionswebhook.TargetSeed,
		Mutators: map[extensionswebhook.Mutator][]extensionswebhook.Type{
			NewMutator(): {{Obj: &corev1.ConfigMap{}}},
		},
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"gardener.cloud/role": "shoot"},
		},
	})
	if err != nil {
		return nil, err
	}
	wh.FailurePolicy = &failurePolicy
	wh.ObjectSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"resources.gardener.cloud/garbage-collectable-reference": "true",
		},
	}
	return wh, nil
}
