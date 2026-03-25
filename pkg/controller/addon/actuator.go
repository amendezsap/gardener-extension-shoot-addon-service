package addon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"gopkg.in/yaml.v3"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	extensionscontroller "github.com/gardener/gardener/extensions/pkg/controller"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/gardener/gardener/pkg/chartrenderer"
	"github.com/gardener/gardener/pkg/utils/managedresources"

	addonpkg "github.com/amendezsap/gardener-extension-shoot-addon-service/pkg/addon"
	"github.com/amendezsap/gardener-extension-shoot-addon-service/pkg/apis/config"
	awsutil "github.com/amendezsap/gardener-extension-shoot-addon-service/pkg/aws"
	"github.com/amendezsap/gardener-extension-shoot-addon-service/charts/embedded"
)

// actuator implements the extension.Actuator interface.
type actuator struct {
	client          client.Client
	restConfig      *rest.Config
	seedAddonsOnce  sync.Once
}

// NewActuator creates a new actuator.
func NewActuator(mgr manager.Manager) *actuator {
	return &actuator{
		client:     mgr.GetClient(),
		restConfig: mgr.GetConfig(),
	}
}

// shootMetadata holds extracted shoot info needed for reconciliation.
type shootMetadata struct {
	Name              string
	Namespace         string
	Project           string
	ControlNamespace  string
	Region            string
	ProviderType      string
	VpcID             string
	WorkerCIDRs       []string
	NodeRoleName      string
	NodeSecurityGroup string // from Infrastructure status
	Partition         string
	SeedName          string
}

// Reconcile creates/updates IAM policies, VPC endpoints, and deploys addon
// charts as ManagedResources.
func (a *actuator) Reconcile(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	cluster, err := extensionscontroller.GetCluster(ctx, a.client, ex.Namespace)
	if err != nil {
		return fmt.Errorf("failed to get cluster: %w", err)
	}

	meta, err := a.extractShootMetadata(cluster, ex.Namespace)
	if err != nil {
		return fmt.Errorf("failed to extract shoot metadata: %w", err)
	}

	// Read the addon manifest from the embedded FS
	manifest, err := addonpkg.ReadManifest(embedded.Addons)
	if err != nil {
		return fmt.Errorf("failed to read addon manifest: %w", err)
	}

	// Deploy seed-targeted addons once (first reconcile only).
	// Pass the shoot's provider type so provider-specific values are loaded.
	a.reconcileSeedAddons(ctx, log, ex.Namespace, meta.ProviderType)

	// Resolve per-shoot config from the Extension CR's providerConfig
	cfg, err := config.ResolveConfig(ex)
	if err != nil {
		return fmt.Errorf("failed to resolve config: %w", err)
	}

	prevStatus, err := config.GetPreviousStatus(ex)
	if err != nil {
		log.Error(err, "Failed to parse previous status, treating as empty")
		prevStatus = &config.ProviderStatus{}
	}

	log = log.WithValues("shoot", meta.Name, "namespace", meta.Namespace, "nodeRole", meta.NodeRoleName)

	// AWS-specific features (IAM policies, VPC endpoints) only apply to shoots
	// using provider-aws. Non-AWS shoots get chart deployment only.
	// Future: GCP IAM equivalent (Workload Identity, service account binding).
	var awsClient *awsutil.Client
	if meta.ProviderType == "aws" {
		creds, err := a.getCloudProviderCredentials(ctx, ex.Namespace)
		if err != nil {
			return fmt.Errorf("failed to get cloud provider credentials: %w", err)
		}
		awsClient, err = awsutil.NewClient(creds, meta.Region)
		if err != nil {
			return fmt.Errorf("failed to create AWS client: %w", err)
		}
	} else {
		log.Info("Non-AWS shoot — skipping IAM policies and VPC endpoint management", "provider", meta.ProviderType)
	}

	newStatus := &config.ProviderStatus{
		Addons: make(map[string]*config.AddonStatus),
	}

	// Deploy the target namespace as a shared ManagedResource (before addons).
	// This prevents conflicts when multiple addons target the same namespace.
	targetNS := manifest.DefaultNamespace
	nsData := map[string][]byte{
		"namespace.yaml": []byte(fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
  labels:
    pod-security.kubernetes.io/enforce: privileged
`, targetNS)),
	}
	if err := managedresources.CreateForShoot(ctx, a.client, ex.Namespace, "addon-namespace", "shoot-addon-service", false, nsData); err != nil {
		return fmt.Errorf("deploy target namespace: %w", err)
	}

	// Deploy registry secrets (shared across all addons)
	if len(manifest.RegistrySecrets) > 0 {
		secretData, err := a.renderRegistrySecrets(ctx, manifest, ex.Namespace)
		if err != nil {
			return fmt.Errorf("render registry secrets: %w", err)
		}
		if err := managedresources.CreateForShoot(ctx, a.client, ex.Namespace, "addon-registry-secrets", "shoot-addon-service", false, secretData); err != nil {
			return fmt.Errorf("deploy registry secrets: %w", err)
		}
	}

	// Global AWS: attach node-level IAM policies (always, regardless of addons)
	var currentGlobalPolicies []string
	if meta.ProviderType == "aws" && manifest.GlobalAWS != nil && len(manifest.GlobalAWS.IAMPolicies) > 0 {
		log.Info("Ensuring global IAM policies on node role", "nodeRole", meta.NodeRoleName)
		for _, policyName := range manifest.GlobalAWS.IAMPolicies {
			policyARN := fmt.Sprintf("arn:%s:iam::aws:policy/%s", meta.Partition, policyName)
			if err := awsClient.AttachRolePolicy(ctx, meta.NodeRoleName, policyARN); err != nil {
				return fmt.Errorf("failed to attach global policy %s: %w", policyARN, err)
			}
			log.Info("Global IAM policy attached", "policy", policyARN)
			currentGlobalPolicies = append(currentGlobalPolicies, policyName)
		}

		// Detach policies that were previously attached but removed from the manifest
		if prevStatus != nil && len(prevStatus.GlobalIAMPolicies) > 0 {
			currentSet := make(map[string]bool, len(currentGlobalPolicies))
			for _, p := range currentGlobalPolicies {
				currentSet[p] = true
			}
			for _, prevPolicy := range prevStatus.GlobalIAMPolicies {
				if !currentSet[prevPolicy] {
					policyARN := fmt.Sprintf("arn:%s:iam::aws:policy/%s", meta.Partition, prevPolicy)
					log.Info("Detaching removed global IAM policy", "policy", policyARN)
					if err := awsClient.DetachRolePolicy(ctx, meta.NodeRoleName, policyARN); err != nil {
						log.Error(err, "Failed to detach removed global IAM policy", "policy", policyARN)
						// Non-fatal — policy may have been manually removed already
					}
				}
			}
		}
	}

	// Global AWS: ensure VPC endpoints (VPC-level infrastructure, not addon-specific)
	if meta.ProviderType == "aws" && manifest.GlobalAWS != nil && len(manifest.GlobalAWS.VPCEndpoints) > 0 {
		if cfg.IsVPCEndpointEnabled() {
			if meta.VpcID != "" && meta.NodeSecurityGroup != "" {
				for _, vpceSpec := range manifest.GlobalAWS.VPCEndpoints {
					log.Info("Ensuring global VPC endpoint", "service", vpceSpec.Service, "vpc", meta.VpcID, "nodeSG", meta.NodeSecurityGroup)
					subnetIDs, err := awsClient.GetWorkerSubnetIDs(ctx, meta.VpcID, meta.WorkerCIDRs)
					if err != nil {
						return fmt.Errorf("failed to get worker subnets for VPCE %s: %w", vpceSpec.Service, err)
					}
					result, err := awsClient.EnsureCloudWatchVPCEndpoint(ctx, meta.VpcID, meta.Region, subnetIDs, meta.NodeSecurityGroup, meta.ControlNamespace)
					if err != nil {
						return fmt.Errorf("failed to ensure VPC endpoint %s: %w", vpceSpec.Service, err)
					}
					log.Info("Global VPC endpoint ensured", "service", vpceSpec.Service, "endpointID", result.EndpointID, "createdByUs", result.CreatedByUs)
					newStatus.VPCEndpoint = &config.VPCEndpointStatus{
						Enabled:             true,
						EndpointID:          result.EndpointID,
						VPCID:               meta.VpcID,
						NodeSecurityGroupID: meta.NodeSecurityGroup,
						CreatedByUs:         result.CreatedByUs,
					}
				}
			} else {
				log.Info("No VPC ID or node SG available, skipping VPC endpoints")
			}
		} else {
			// VPCE disabled — check if it was previously on (toggle detection)
			if prevStatus != nil && prevStatus.VPCEndpoint != nil && prevStatus.VPCEndpoint.Enabled {
				log.Info("VPC endpoint toggled OFF, cleaning up")
				prev := prevStatus.VPCEndpoint
				if prev.VPCID != "" && prev.NodeSecurityGroupID != "" {
					deleted, err := awsClient.CleanupVPCEndpoint(ctx, prev.VPCID, meta.Region, prev.NodeSecurityGroupID, meta.ControlNamespace)
					if err != nil {
						return fmt.Errorf("cleanup toggled-off VPC endpoint: %w", err)
					}
					if deleted {
						log.Info("VPC endpoint deleted after toggle off")
					} else {
						log.Info("VPC endpoint kept (other shoots use it)")
					}
				}
			}
		}
	}

	// Process each addon in the manifest (shoot-targeted only)
	for i := range manifest.Addons {
		addon := &manifest.Addons[i]

		if !addon.DeploysToShoot() {
			continue
		}

		// Check if this addon is enabled (manifest default + per-shoot override)
		if !cfg.IsAddonEnabled(addon.Name, addon.Enabled) {
			log.Info("Addon disabled, skipping", "addon", addon.Name)
			continue
		}

		log.Info("Reconciling addon", "addon", addon.Name)

		addonStatus := &config.AddonStatus{}

		newStatus.Addons[addon.Name] = addonStatus

		// Render the addon chart and deploy as ManagedResource
		ns := addon.GetNamespace(manifest.DefaultNamespace)
		mrName := addon.GetManagedResourceName()

		secretData, err := a.renderAddonChart(addon, meta, manifest)
		if err != nil {
			return fmt.Errorf("failed to render chart for addon %s: %w", addon.Name, err)
		}

		log.Info("Deploying addon ManagedResource", "addon", addon.Name, "managedResource", mrName, "targetNamespace", ns)
		if err := managedresources.CreateForShoot(ctx, a.client, ex.Namespace, mrName, "shoot-addon-service", false, secretData); err != nil {
			return fmt.Errorf("failed to deploy ManagedResource for addon %s: %w", addon.Name, err)
		}
	}

	// Clean up ManagedResources from previous naming schemes.
	for i := range manifest.Addons {
		addon := &manifest.Addons[i]
		currentName := addon.GetManagedResourceName()
		for _, oldName := range []string{"addon-" + addon.Name, "managed-resources-" + addon.Name} {
			if oldName != currentName {
				if err := managedresources.DeleteForShoot(ctx, a.client, ex.Namespace, oldName); err == nil {
					log.Info("Deleted old ManagedResource", "old", oldName, "new", currentName)
				}
			}
		}
	}

	// Track global IAM policies in status for stale policy detection
	newStatus.GlobalIAMPolicies = currentGlobalPolicies

	// Delete stale GRM ConfigMaps that are missing our namespaces.
	// If a stale ConfigMap is found and deleted, trigger a shoot reconcile
	// via the garden API (if available) and return an error to abort this
	// reconcile. Gardenlet retries the full reconcile from step 1.
	// On retry, no stale ConfigMap exists → step 6 creates fresh → webhook patches.
	if deleted := a.fixStaleGRMConfig(ctx, log, ex.Namespace); deleted {
		a.triggerShootReconcile(ctx, log, meta)
		return fmt.Errorf("deleted stale GRM ConfigMap — aborting reconcile so gardenlet retries from scratch and recreates ConfigMap at step 6 with webhook patching")
	}

	// Store provider status so next reconcile can detect changes
	statusRaw, err := config.MarshalProviderStatus(newStatus)
	if err != nil {
		return fmt.Errorf("failed to marshal provider status: %w", err)
	}
	patch := client.MergeFrom(ex.DeepCopy())
	ex.Status.ProviderStatus = statusRaw
	if err := a.client.Status().Patch(ctx, ex, patch); err != nil {
		log.Error(err, "Failed to update provider status -- non-fatal, will retry on next reconcile")
	}

	log.Info("Reconciliation complete")
	return nil
}

// Delete removes IAM policies, VPC endpoints, and managed resources for all addons.
func (a *actuator) Delete(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	cluster, err := extensionscontroller.GetCluster(ctx, a.client, ex.Namespace)
	if err != nil {
		return fmt.Errorf("failed to get cluster: %w", err)
	}

	meta, err := a.extractShootMetadata(cluster, ex.Namespace)
	if err != nil {
		return fmt.Errorf("failed to extract shoot metadata: %w", err)
	}

	manifest, err := addonpkg.ReadManifest(embedded.Addons)
	if err != nil {
		return fmt.Errorf("failed to read addon manifest: %w", err)
	}

	prevStatus, err := config.GetPreviousStatus(ex)
	if err != nil {
		log.Error(err, "Failed to parse previous status, treating as empty")
		prevStatus = &config.ProviderStatus{}
	}

	log = log.WithValues("shoot", meta.Name, "namespace", meta.Namespace)

	// Collect errors so all cleanup steps run even if some fail
	var errs []error

	// 1. Delete ManagedResources for all addons
	for i := range manifest.Addons {
		addon := &manifest.Addons[i]
		mrName := addon.GetManagedResourceName()
		log.Info("Deleting addon ManagedResource", "addon", addon.Name, "managedResource", mrName)
		if err := a.deleteManagedResource(ctx, ex.Namespace, mrName); err != nil {
			log.Error(err, "Failed to delete ManagedResource", "addon", addon.Name)
			errs = append(errs, err)
		}
	}

	// 1b. Delete addon-namespace ManagedResource
	log.Info("Deleting addon-namespace ManagedResource")
	if err := a.deleteManagedResource(ctx, ex.Namespace, "addon-namespace"); err != nil {
		log.Error(err, "Failed to delete addon-namespace ManagedResource")
		errs = append(errs, err)
	}

	// 1c. Delete registry secrets ManagedResource
	if len(manifest.RegistrySecrets) > 0 {
		log.Info("Deleting registry secrets ManagedResource")
		if err := a.deleteManagedResource(ctx, ex.Namespace, "addon-registry-secrets"); err != nil {
			log.Error(err, "Failed to delete registry secrets ManagedResource")
			errs = append(errs, err)
		}
	}

	// 2. Clean up AWS resources if this is an AWS shoot
	if meta.ProviderType == "aws" {
		creds, err := a.getCloudProviderCredentials(ctx, ex.Namespace)
		if err != nil {
			log.Error(err, "Failed to get cloud provider credentials, skipping AWS cleanup")
		} else {
			awsClient, err := awsutil.NewClient(creds, meta.Region)
			if err != nil {
				log.Error(err, "Failed to create AWS client")
			} else {
				// Clean up global VPC endpoints
				if prevStatus != nil && prevStatus.VPCEndpoint != nil && prevStatus.VPCEndpoint.VPCID != "" {
					log.Info("Cleaning up global VPC endpoint")
					vpcID := prevStatus.VPCEndpoint.VPCID
					nodeSG := prevStatus.VPCEndpoint.NodeSecurityGroupID
					if nodeSG == "" {
						nodeSG = meta.NodeSecurityGroup
					}
					if vpcID != "" && nodeSG != "" {
						deleted, err := awsClient.CleanupVPCEndpoint(ctx, vpcID, meta.Region, nodeSG, meta.ControlNamespace)
						if err != nil {
							log.Error(err, "Failed to clean up VPC endpoint")
							errs = append(errs, err)
						} else if deleted {
							log.Info("VPC endpoint deleted")
							if prevStatus.VPCEndpoint.EndpointID != "" {
								if err := awsClient.WaitForVPCEndpointDeletion(ctx, prevStatus.VPCEndpoint.EndpointID, 5*time.Minute); err != nil {
									log.Error(err, "Timeout waiting for VPC endpoint deletion")
									errs = append(errs, err)
								}
							}
						} else {
							log.Info("VPC endpoint kept (other shoots use it)")
						}
					}
				}

				// Detach global IAM policies (last — after VPC endpoints are cleaned up)
				if manifest.GlobalAWS != nil && len(manifest.GlobalAWS.IAMPolicies) > 0 {
					log.Info("Detaching global IAM policies from node role")
					for _, policyName := range manifest.GlobalAWS.IAMPolicies {
						policyARN := fmt.Sprintf("arn:%s:iam::aws:policy/%s", meta.Partition, policyName)
						if err := awsClient.DetachRolePolicy(ctx, meta.NodeRoleName, policyARN); err != nil {
							log.Error(err, "Failed to detach global IAM policy", "policy", policyARN)
							errs = append(errs, err)
						}
					}
				}
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("deletion encountered %d error(s), first: %w", len(errs), errs[0])
	}

	log.Info("Deletion complete")
	return nil
}

// ForceDelete delegates to Delete.
func (a *actuator) ForceDelete(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	return a.Delete(ctx, log, ex)
}

// Migrate delegates to Delete (resources will be re-created on the new seed).
func (a *actuator) Migrate(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	return a.Delete(ctx, log, ex)
}

// Restore delegates to Reconcile.
func (a *actuator) Restore(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	return a.Reconcile(ctx, log, ex)
}

// --------------------------------------------------------------------------
// Seed/runtime addon deployment
// --------------------------------------------------------------------------

// reconcileSeedAddons deploys addons with target "seed" or "global" to the
// seed/runtime cluster as seed-class ManagedResources. Runs once per
// controller lifetime (on the first shoot reconcile), since seed addons
// don't change per-shoot.
func (a *actuator) reconcileSeedAddons(ctx context.Context, log logr.Logger, namespace string, providerType string) {
	a.seedAddonsOnce.Do(func() {
		log.Info("Reconciling seed-targeted addons")

		manifest, err := addonpkg.ReadManifest(embedded.Addons)
		if err != nil {
			log.Error(err, "Failed to read addon manifest for seed addons")
			return
		}

		seedName := os.Getenv("SEED_NAME")
		region := os.Getenv("REGION")
		if region == "" {
			region = "us-gov-west-1"
		}

		// Build a synthetic shootMetadata for template expansion.
		// For seed addons, SeedName = this seed's name (from env).
		// ProviderType is from the first shoot reconcile — ensures provider-specific
		// values (e.g., values.aws.yaml) are loaded during chart rendering.
		meta := &shootMetadata{
			Name:         seedName,
			SeedName:     seedName,
			Region:       region,
			ProviderType: providerType,
		}

		// Deploy target namespace
		targetNS := manifest.DefaultNamespace
		nsData := map[string][]byte{
			"namespace.yaml": []byte(fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
  labels:
    pod-security.kubernetes.io/enforce: privileged
`, targetNS)),
		}
		if err := managedresources.CreateForSeed(ctx, a.client, namespace, "seed-addon-namespace", false, nsData); err != nil {
			log.Error(err, "Failed to deploy seed addon namespace")
			return
		}

		for i := range manifest.Addons {
			addon := &manifest.Addons[i]
			if !addon.DeploysToSeed() || !addon.Enabled {
				continue
			}

			mrName := "seed-" + addon.GetManagedResourceName()

			secretData, err := a.renderAddonChart(addon, meta, manifest)
			if err != nil {
				log.Error(err, "Failed to render seed addon chart", "addon", addon.Name)
				continue
			}

			log.Info("Deploying seed addon ManagedResource", "addon", addon.Name, "managedResource", mrName)
			if err := managedresources.CreateForSeed(ctx, a.client, namespace, mrName, false, secretData); err != nil {
				log.Error(err, "Failed to deploy seed addon ManagedResource", "addon", addon.Name)
			}
		}

		log.Info("Seed addon reconciliation complete")
	})
}

// --------------------------------------------------------------------------
// GRM ConfigMap stale detection and auto-fix
// --------------------------------------------------------------------------

var grmConfigMapRegex = regexp.MustCompile(`^gardener-resource-manager-[a-f0-9]{8}$`)

// fixStaleGRMConfig detects stale GRM ConfigMaps or Deployments and fixes them.
// This runs on every reconcile.
//
// Two cases are handled:
//  1. Stale ConfigMap: missing our required namespaces in
//     targetClientConnection.namespaces → delete CM + Deployment so gardenlet
//     recreates both and the webhook injects the namespaces.
//  2. Stale Deployment: CM is correct but GRM pods pre-date the CM, meaning
//     they started with a cached old ConfigMap → delete Deployment only so
//     gardenlet recreates it with pods that read the correct CM.
//
// In both cases, the caller should abort the current reconcile (return error)
// so gardenlet retries from step 1.
func (a *actuator) fixStaleGRMConfig(ctx context.Context, log logr.Logger, namespace string) bool {
	// Only proceed if our webhook is fully ready (CA bundle set).
	if !a.isWebhookReady(ctx, log) {
		log.Info("GRM webhook not yet ready, skipping stale ConfigMap check (will retry on next reconcile)")
		return false
	}

	required := loadRequiredNamespaces()

	cmList := &corev1.ConfigMapList{}
	if err := a.client.List(ctx, cmList, &client.ListOptions{
		Namespace: namespace,
		LabelSelector: labels.SelectorFromSet(map[string]string{
			"resources.gardener.cloud/garbage-collectable-reference": "true",
		}),
	}); err != nil {
		log.Error(err, "Failed to list ConfigMaps for GRM stale check")
		return false
	}

	for i := range cmList.Items {
		cm := &cmList.Items[i]
		if !grmConfigMapRegex.MatchString(cm.Name) {
			continue
		}

		configYAML, ok := cm.Data["config.yaml"]
		if !ok || configYAML == "" {
			continue
		}

		missing := getMissingNamespaces(configYAML, required)

		if len(missing) > 0 {
			// Case 1: CM is missing our namespaces — delete CM + Deployment
			log.Info("GRM ConfigMap missing required namespaces — deleting CM + Deployment",
				"configmap", cm.Name, "namespace", namespace, "missing", missing)

			if err := a.client.Delete(ctx, cm); err != nil {
				log.Error(err, "Failed to delete stale GRM ConfigMap", "configmap", cm.Name)
				return false
			}
			log.Info("Stale GRM ConfigMap deleted", "configmap", cm.Name)

			a.deleteGRMDeployment(ctx, log, namespace)
			return true
		}

		// Case 2: CM is correct — check if GRM Deployment pre-dates the CM
		// (pods started with an older CM cached by kubelet)
		if a.hasStaleGRMDeployment(ctx, log, namespace, cm.CreationTimestamp.Time) {
			log.Info("GRM ConfigMap is correct but Deployment pre-dates it — deleting Deployment so pods restart with correct config",
				"configmap", cm.Name, "cmCreated", cm.CreationTimestamp.Time)
			a.deleteGRMDeployment(ctx, log, namespace)
			return true
		}
	}

	return false
}

// deleteGRMDeployment deletes the gardener-resource-manager Deployment.
func (a *actuator) deleteGRMDeployment(ctx context.Context, log logr.Logger, namespace string) {
	deploy := &appsv1.Deployment{}
	if err := a.client.Get(ctx, types.NamespacedName{
		Name:      "gardener-resource-manager",
		Namespace: namespace,
	}, deploy); err == nil {
		if err := a.client.Delete(ctx, deploy); err != nil {
			log.Error(err, "Failed to delete GRM Deployment")
		} else {
			log.Info("GRM Deployment deleted")
		}
	}
}

// hasStaleGRMDeployment checks if the GRM Deployment was created before the
// given time (typically the ConfigMap creation time). If the Deployment
// pre-dates the CM, its pods started with a cached old ConfigMap.
func (a *actuator) hasStaleGRMDeployment(ctx context.Context, log logr.Logger, namespace string, cmCreatedAt time.Time) bool {
	deploy := &appsv1.Deployment{}
	if err := a.client.Get(ctx, types.NamespacedName{
		Name:      "gardener-resource-manager",
		Namespace: namespace,
	}, deploy); err != nil {
		log.Error(err, "Failed to get GRM Deployment for staleness check")
		return false
	}

	if deploy.CreationTimestamp.Time.Before(cmCreatedAt) {
		log.Info("GRM Deployment pre-dates ConfigMap",
			"deployCreated", deploy.CreationTimestamp.Time, "cmCreated", cmCreatedAt)
		return true
	}

	return false
}

// loadRequiredNamespaces returns the list of namespaces that must be present
// in the GRM ConfigMap's targetClientConnection.namespaces field.
func loadRequiredNamespaces() []string {
	raw := os.Getenv("NAMESPACES")
	if raw == "" {
		return []string{"managed-resources"}
	}
	var ns []string
	for _, n := range strings.Split(raw, ",") {
		n = strings.TrimSpace(n)
		if n != "" {
			ns = append(ns, n)
		}
	}
	return ns
}

// triggerShootReconcile annotates the Shoot in the garden cluster with
// gardener.cloud/operation=reconcile to trigger an immediate gardenlet reconcile.
// This is best-effort — if the garden kubeconfig is not available (e.g.,
// injectGardenKubeconfig is not enabled), the extension falls back to waiting
// for gardenlet's regular resync cycle (up to 60 minutes).
func (a *actuator) triggerShootReconcile(ctx context.Context, log logr.Logger, meta *shootMetadata) {
	gardenClient, err := a.getGardenClient()
	if err != nil {
		log.Info("Garden kubeconfig not available, skipping shoot reconcile trigger — gardenlet will retry on its regular resync cycle", "error", err)
		return
	}

	shoot := &gardencorev1beta1.Shoot{}
	shootKey := types.NamespacedName{
		Name:      meta.Name,
		Namespace: meta.Namespace,
	}

	if err := gardenClient.Get(ctx, shootKey, shoot); err != nil {
		log.Error(err, "Failed to get Shoot from garden cluster", "shoot", shootKey)
		return
	}

	// Only annotate if no operation is already pending
	if v, ok := shoot.Annotations["gardener.cloud/operation"]; ok && v != "" {
		log.Info("Shoot already has a pending operation, skipping reconcile trigger", "operation", v)
		return
	}

	patch := client.MergeFrom(shoot.DeepCopy())
	if shoot.Annotations == nil {
		shoot.Annotations = make(map[string]string)
	}
	shoot.Annotations["gardener.cloud/operation"] = "reconcile"

	if err := gardenClient.Patch(ctx, shoot, patch); err != nil {
		log.Error(err, "Failed to trigger shoot reconcile via garden API", "shoot", shootKey)
		return
	}

	log.Info("Triggered shoot reconcile via garden API", "shoot", shootKey)
}

// getGardenClient creates a client for the garden (virtual) cluster using the
// kubeconfig injected by gardenlet when injectGardenKubeconfig is enabled.
// Returns an error if the GARDEN_KUBECONFIG env var is not set or the
// kubeconfig cannot be loaded.
func (a *actuator) getGardenClient() (client.Client, error) {
	kubeconfigPath := os.Getenv("GARDEN_KUBECONFIG")
	if kubeconfigPath == "" {
		return nil, fmt.Errorf("GARDEN_KUBECONFIG not set")
	}

	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("build garden REST config: %w", err)
	}

	gardenScheme := runtime.NewScheme()
	_ = gardencorev1beta1.AddToScheme(gardenScheme)

	c, err := client.New(restConfig, client.Options{Scheme: gardenScheme})
	if err != nil {
		return nil, fmt.Errorf("create garden client: %w", err)
	}

	return c, nil
}

// isWebhookReady checks if the GRM namespace provisioner webhook is fully
// configured by verifying the MutatingWebhookConfiguration exists and has
// a CA bundle set. Without this, the webhook server may not be ready to
// intercept ConfigMap CREATE requests from gardenlet.
func (a *actuator) isWebhookReady(ctx context.Context, log logr.Logger) bool {
	whc := &admissionregistrationv1.MutatingWebhookConfiguration{}
	if err := a.client.Get(ctx, types.NamespacedName{
		Name: "gardener-extension-extension-admission-shoot-addon",
	}, whc); err != nil {
		return false
	}

	// Check that at least one webhook has a CA bundle
	for _, wh := range whc.Webhooks {
		if len(wh.ClientConfig.CABundle) > 0 {
			return true
		}
	}

	return false
}

// getMissingNamespaces checks if a GRM config.yaml is missing any of the
// required namespaces in targetClientConnection.namespaces. Returns the list
// of missing namespaces, or nil if all are present.
func getMissingNamespaces(configYAML string, required []string) []string {
	if len(required) == 0 {
		return nil
	}

	var cfg map[string]interface{}
	if err := yaml.Unmarshal([]byte(configYAML), &cfg); err != nil {
		return nil
	}

	tcc, ok := cfg["targetClientConnection"]
	if !ok {
		return nil
	}

	tccMap, ok := tcc.(map[string]interface{})
	if !ok {
		return nil
	}

	// If no namespaces field, GRM watches everything — our namespaces are covered
	nsRaw, ok := tccMap["namespaces"]
	if !ok {
		return nil
	}

	// Build set of existing namespaces
	existing := make(map[string]bool)
	if nsList, ok := nsRaw.([]interface{}); ok {
		for _, v := range nsList {
			if s, ok := v.(string); ok {
				existing[s] = true
			}
		}
	}

	var missing []string
	for _, ns := range required {
		if !existing[ns] {
			missing = append(missing, ns)
		}
	}
	return missing
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// extractShootMetadata pulls relevant shoot info from the Cluster resource.
func (a *actuator) extractShootMetadata(cluster *extensionscontroller.Cluster, namespace string) (*shootMetadata, error) {
	shoot := cluster.Shoot
	if shoot == nil {
		return nil, fmt.Errorf("cluster has no shoot")
	}

	project := strings.TrimPrefix(shoot.Namespace, "garden-")
	if shoot.Namespace == "garden" {
		project = "garden"
	}

	region := shoot.Spec.Region
	partition := "aws"
	if strings.Contains(region, "gov") {
		partition = "aws-us-gov"
	}

	// Extract VPC ID and worker CIDRs from infrastructure config
	var vpcID string
	var workerCIDRs []string
	if shoot.Spec.Provider.InfrastructureConfig != nil {
		raw := shoot.Spec.Provider.InfrastructureConfig.Raw
		vpcID, workerCIDRs = parseInfraConfig(raw)
	}

	// Extract node security group from Infrastructure status
	nodeSG := a.getNodeSecurityGroupFromInfraStatus(cluster, namespace)

	seedName := ""
	if shoot.Spec.SeedName != nil {
		seedName = *shoot.Spec.SeedName
	}

	return &shootMetadata{
		Name:              shoot.Name,
		Namespace:         shoot.Namespace,
		Project:           project,
		ControlNamespace:  namespace,
		Region:            region,
		ProviderType:      shoot.Spec.Provider.Type,
		VpcID:             vpcID,
		WorkerCIDRs:       workerCIDRs,
		NodeRoleName:      fmt.Sprintf("%s-nodes", namespace),
		NodeSecurityGroup: nodeSG,
		Partition:         partition,
		SeedName:          seedName,
	}, nil
}

// getNodeSecurityGroupFromInfraStatus reads the node SG from the Infrastructure
// status providerStatus. The Infrastructure controller stores this after creating
// the shoot's AWS infrastructure.
//
// Path: infrastructure.status.providerStatus.vpc.securityGroups[purpose=nodes].id
func (a *actuator) getNodeSecurityGroupFromInfraStatus(cluster *extensionscontroller.Cluster, namespace string) string {
	// The Cluster resource embeds the shoot but not the Infrastructure status.
	// We need to read the Infrastructure CR directly from the seed.
	infra := &extensionsv1alpha1.Infrastructure{}
	shootName := ""
	if cluster.Shoot != nil {
		shootName = cluster.Shoot.Name
	}
	if shootName == "" {
		return ""
	}

	if err := a.client.Get(context.Background(), types.NamespacedName{
		Name:      shootName,
		Namespace: namespace,
	}, infra); err != nil {
		return ""
	}

	if infra.Status.ProviderStatus == nil || infra.Status.ProviderStatus.Raw == nil {
		return ""
	}

	// Parse the providerStatus to extract the node SG
	var status struct {
		VPC struct {
			SecurityGroups []struct {
				ID      string `json:"id"`
				Purpose string `json:"purpose"`
			} `json:"securityGroups"`
		} `json:"vpc"`
	}

	if err := json.Unmarshal(infra.Status.ProviderStatus.Raw, &status); err != nil {
		return ""
	}

	for _, sg := range status.VPC.SecurityGroups {
		if sg.Purpose == "nodes" {
			return sg.ID
		}
	}

	return ""
}

// parseInfraConfig extracts VPC ID and worker CIDRs from raw infrastructure config JSON.
func parseInfraConfig(raw []byte) (string, []string) {
	var infraConfig struct {
		Networks struct {
			VPC struct {
				ID string `json:"id"`
			} `json:"vpc"`
			Zones []struct {
				Workers string `json:"workers"`
			} `json:"zones"`
		} `json:"networks"`
	}

	if err := json.Unmarshal(raw, &infraConfig); err != nil {
		return "", nil
	}

	var cidrs []string
	for _, z := range infraConfig.Networks.Zones {
		if z.Workers != "" {
			cidrs = append(cidrs, z.Workers)
		}
	}

	return infraConfig.Networks.VPC.ID, cidrs
}

// getCloudProviderCredentials reads the cloudprovider secret from the shoot's
// control plane namespace.
func (a *actuator) getCloudProviderCredentials(ctx context.Context, namespace string) (*awsutil.Credentials, error) {
	secret := &corev1.Secret{}
	if err := a.client.Get(ctx, types.NamespacedName{
		Name:      "cloudprovider",
		Namespace: namespace,
	}, secret); err != nil {
		return nil, fmt.Errorf("failed to get cloudprovider secret in %s: %w", namespace, err)
	}

	accessKeyID := string(secret.Data["accessKeyID"])
	secretAccessKey := string(secret.Data["secretAccessKey"])

	if accessKeyID == "" {
		return nil, fmt.Errorf("cloudprovider secret in %s is missing required field accessKeyID", namespace)
	}
	if secretAccessKey == "" {
		return nil, fmt.Errorf("cloudprovider secret in %s is missing required field secretAccessKey", namespace)
	}

	return &awsutil.Credentials{
		AccessKeyID:    accessKeyID,
		SecretAccessKey: secretAccessKey,
		RoleARN:        string(secret.Data["roleARN"]),
		Token:          string(secret.Data["token"]),
	}, nil
}

// deleteManagedResource deletes a ManagedResource and waits for it to be gone.
func (a *actuator) deleteManagedResource(ctx context.Context, namespace, name string) error {
	if err := managedresources.DeleteForShoot(ctx, a.client, namespace, name); err != nil {
		return err
	}
	return managedresources.WaitUntilDeleted(ctx, a.client, namespace, name)
}

// --------------------------------------------------------------------------
// Registry secrets
// --------------------------------------------------------------------------

// renderRegistrySecrets reads each registry secret from the seed and produces
// shoot-side kubernetes.io/dockerconfigjson Secret manifests.
func (a *actuator) renderRegistrySecrets(ctx context.Context, manifest *addonpkg.AddonManifest, namespace string) (map[string][]byte, error) {
	result := map[string][]byte{}

	for _, rs := range manifest.RegistrySecrets {
		// Read seed secret
		seedSecret := &corev1.Secret{}
		seedNS := rs.SeedSecretRef.Namespace
		if seedNS == "" {
			seedNS = namespace // extension's namespace
		}
		if err := a.client.Get(ctx, types.NamespacedName{
			Name:      rs.SeedSecretRef.Name,
			Namespace: seedNS,
		}, seedSecret); err != nil {
			return nil, fmt.Errorf("read seed registry secret %s/%s: %w", seedNS, rs.SeedSecretRef.Name, err)
		}

		// Build shoot secret manifest
		targetNS := manifest.DefaultNamespace
		shootSecret := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: kubernetes.io/dockerconfigjson
data:
  .dockerconfigjson: %s
`, rs.Name, targetNS, base64.StdEncoding.EncodeToString(seedSecret.Data[".dockerconfigjson"]))

		result[fmt.Sprintf("registry-secret-%s.yaml", rs.Name)] = []byte(shootSecret)
	}

	return result, nil
}

// --------------------------------------------------------------------------
// Embedded chart rendering
// --------------------------------------------------------------------------

// renderAddonChart renders an addon's Helm chart from the embedded FS.
//
// Values layering (later wins):
//  1. addons/<valuesPath>/values.yaml           -- base values
//  2. addons/<valuesPath>/values.<provider>.yaml -- provider-specific
//  3. addon.ShootValues from manifest.yaml       -- per-addon shoot values
//  4. Image overrides from env vars              -- build-time image pins
//
// Template variables in ShootValues are expanded:
//
//	{{ .Region }}   -> meta.Region
//	{{ .SeedName }} -> meta.SeedName
func (a *actuator) renderAddonChart(addon *addonpkg.Addon, meta *shootMetadata, manifest *addonpkg.AddonManifest) (map[string][]byte, error) {
	// Layer 1: base values.yaml from the addon's values path
	merged := map[string]interface{}{}
	if addon.ValuesPath != "" {
		baseVals, err := readEmbeddedValues(embedded.Addons, "addons/"+addon.ValuesPath+"/values.yaml")
		if err != nil {
			// values.yaml is optional -- some addons may only have provider-specific
			if !isNotExist(err) {
				return nil, fmt.Errorf("read base values: %w", err)
			}
		} else {
			merged = baseVals
		}

		// Layer 2: provider-specific values
		providerFile := fmt.Sprintf("addons/%s/values.%s.yaml", addon.ValuesPath, meta.ProviderType)
		providerVals, err := readEmbeddedValues(embedded.Addons, providerFile)
		if err != nil {
			// Provider file is optional
			if !isNotExist(err) {
				return nil, fmt.Errorf("read provider values: %w", err)
			}
		} else {
			merged = mergeMaps(merged, providerVals)
		}
	}

	// Layer 3: shoot values from the manifest (with template expansion)
	if addon.ShootValues != nil {
		expanded := expandShootValues(addon.ShootValues, meta)
		merged = mergeMaps(merged, expanded)
	}

	// Layer 4: imagePullSecrets from addon manifest
	if len(addon.ImagePullSecrets) > 0 {
		pullSecrets := make([]map[string]interface{}, len(addon.ImagePullSecrets))
		for i, name := range addon.ImagePullSecrets {
			pullSecrets[i] = map[string]interface{}{"name": name}
		}
		merged["imagePullSecrets"] = pullSecrets
	}

	// Layer 5: image overrides from environment variables
	if addon.Image != nil {
		applyImageOverride(merged, addon)
	}

	// Create a chart renderer using the REST config from the manager
	renderer, err := chartrenderer.NewForConfig(a.restConfig)
	if err != nil {
		return nil, fmt.Errorf("create chart renderer: %w", err)
	}

	// Determine chart path and target namespace
	chartPath := "addons/" + addon.Chart.Path
	ns := addon.GetNamespace(manifest.DefaultNamespace)
	releaseName := addon.GetManagedResourceName()

	// Render the embedded chart
	rendered, err := renderer.RenderEmbeddedFS(embedded.Addons, chartPath, releaseName, ns, merged)
	if err != nil {
		return nil, fmt.Errorf("render embedded chart %s: %w", chartPath, err)
	}

	return rendered.AsSecretData(), nil
}

// readEmbeddedValues reads a YAML values file from the embedded FS and returns
// it as a map.
func readEmbeddedValues(efs fs.ReadFileFS, path string) (map[string]interface{}, error) {
	data, err := efs.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var vals map[string]interface{}
	if err := yaml.Unmarshal(data, &vals); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if vals == nil {
		vals = map[string]interface{}{}
	}
	return vals, nil
}

// isNotExist checks if an error indicates a file was not found.
func isNotExist(err error) bool {
	return err != nil && strings.Contains(err.Error(), "file does not exist")
}

// expandShootValues processes a ShootValues map, replacing template variables
// with shoot-specific values.
//
// Supported variables:
//
//	{{ .Region }}   -> shootMetadata.Region
//	{{ .SeedName }} -> shootMetadata.SeedName
func expandShootValues(vals map[string]interface{}, meta *shootMetadata) map[string]interface{} {
	result := make(map[string]interface{}, len(vals))
	for k, v := range vals {
		result[k] = expandValue(v, meta)
	}
	return result
}

// expandValue recursively expands template variables in a single value.
func expandValue(v interface{}, meta *shootMetadata) interface{} {
	switch val := v.(type) {
	case string:
		s := val
		s = strings.ReplaceAll(s, "{{ .Region }}", meta.Region)
		s = strings.ReplaceAll(s, "{{ .SeedName }}", meta.SeedName)
		s = strings.ReplaceAll(s, "{{ .ShootName }}", meta.Name)
		s = strings.ReplaceAll(s, "{{ .ShootNamespace }}", meta.Namespace)
		s = strings.ReplaceAll(s, "{{ .Project }}", meta.Project)
		s = strings.ReplaceAll(s, "{{ .ControlNamespace }}", meta.ControlNamespace)
		return s
	case map[string]interface{}:
		result := make(map[string]interface{}, len(val))
		for mk, mv := range val {
			result[mk] = expandValue(mv, meta)
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(val))
		for i, item := range val {
			result[i] = expandValue(item, meta)
		}
		return result
	default:
		return v
	}
}

// applyImageOverride reads ADDON_<UPPER_NAME>_IMAGE_REPOSITORY and
// ADDON_<UPPER_NAME>_IMAGE_TAG environment variables and, if set, overrides the
// image values at the key path specified by addon.Image.ValuesKey.
//
// For example, if addon.Name is "fluent-bit" and addon.Image.ValuesKey is "image",
// this looks for ADDON_FLUENT_BIT_IMAGE_REPOSITORY and ADDON_FLUENT_BIT_IMAGE_TAG.
func applyImageOverride(merged map[string]interface{}, addon *addonpkg.Addon) {
	if addon.Image == nil || addon.Image.ValuesKey == "" {
		return
	}

	// Build env var prefix: "fluent-bit" -> "FLUENT_BIT"
	envName := strings.ToUpper(strings.ReplaceAll(addon.Name, "-", "_"))
	repoEnv := os.Getenv(fmt.Sprintf("ADDON_%s_IMAGE_REPOSITORY", envName))
	tagEnv := os.Getenv(fmt.Sprintf("ADDON_%s_IMAGE_TAG", envName))

	// Use defaults from the manifest if env vars are not set
	repo := addon.Image.DefaultRepository
	tag := addon.Image.DefaultTag
	if repoEnv != "" {
		repo = repoEnv
	}
	if tagEnv != "" {
		tag = tagEnv
	}

	// Only apply if we have at least a repository
	if repo == "" {
		return
	}

	imageMap := map[string]interface{}{
		"repository": repo,
	}
	if tag != "" {
		imageMap["tag"] = tag
	}

	// Set the image at the specified values key.
	// Supports nested keys separated by "." (e.g., "spec.image").
	setNestedValue(merged, addon.Image.ValuesKey, imageMap)
}

// setNestedValue sets a value in a nested map using a dot-separated key path.
// Creates intermediate maps as needed.
func setNestedValue(m map[string]interface{}, keyPath string, value interface{}) {
	parts := strings.Split(keyPath, ".")
	current := m
	for i, part := range parts {
		if i == len(parts)-1 {
			current[part] = value
			return
		}
		next, ok := current[part]
		if !ok {
			next = map[string]interface{}{}
			current[part] = next
		}
		nextMap, ok := next.(map[string]interface{})
		if !ok {
			nextMap = map[string]interface{}{}
			current[part] = nextMap
		}
		current = nextMap
	}
}

// mergeMaps deep-merges src into dst (src wins on conflicts).
func mergeMaps(dst, src map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	for k, v := range dst {
		result[k] = v
	}
	for k, v := range src {
		if dstVal, ok := result[k]; ok {
			if dstMap, dstOk := dstVal.(map[string]interface{}); dstOk {
				if srcMap, srcOk := v.(map[string]interface{}); srcOk {
					result[k] = mergeMaps(dstMap, srcMap)
					continue
				}
			}
		}
		result[k] = v
	}
	return result
}
