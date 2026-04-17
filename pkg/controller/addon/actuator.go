package addon

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"bytes"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"github.com/go-logr/logr"
	"gopkg.in/yaml.v3"
	helmchart "helm.sh/helm/v3/pkg/chart"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	yamlutil "sigs.k8s.io/yaml"
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
	resourcesv1alpha1 "github.com/gardener/gardener/pkg/apis/resources/v1alpha1"
	gardenerchartrenderer "github.com/gardener/gardener/pkg/chartrenderer"
	"github.com/gardener/gardener/pkg/utils/managedresources"
	"k8s.io/client-go/discovery"

	hookaware "github.com/amendezsap/gardener-extension-shoot-addon-service/pkg/chartrenderer"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	addonpkg "github.com/amendezsap/gardener-extension-shoot-addon-service/pkg/addon"
	"github.com/amendezsap/gardener-extension-shoot-addon-service/pkg/addon/oci"
	"github.com/amendezsap/gardener-extension-shoot-addon-service/pkg/apis/config"
	awsutil "github.com/amendezsap/gardener-extension-shoot-addon-service/pkg/aws"
	gcputil "github.com/amendezsap/gardener-extension-shoot-addon-service/pkg/gcp"
	"github.com/amendezsap/gardener-extension-shoot-addon-service/charts/embedded"
)

// ConfigMapName is the name of the ConfigMap that holds runtime addon configuration.
const ConfigMapName = "shoot-addon-service-config"

// ManagedResource names for shared infrastructure (namespace, registry secrets).
// These use the Gardener convention prefix. Addon MRs use bare names.
const (
	mrNamespace       = addonpkg.ManagedResourcePrefix + "namespace"
	mrNamespaceSeed   = addonpkg.ManagedResourcePrefix + "namespace-seed"
	mrRegistrySecrets = addonpkg.ManagedResourcePrefix + "registry-secrets"
)

// Legacy MR names from previous versions that need cleanup.
// Namespace MR uses keepObjects=true (namespace must survive).
// Addon MRs use keepObjects=true for the convention-prefixed names only
// (resources are now owned by the bare-name MR).
var oldNamespaceMRNames = []string{
	"addon-namespace",
}

var oldSeedNamespaceMRNames = []string{
	"seed-addon-namespace",
}

var oldRegistryMRNames = []string{
	"addon-registry-secrets",
}

// oldShootMRNames returns legacy shoot-class MR names for an addon.
var oldShootMRNames = func(addonName string) []string {
	return []string{
		"addon-" + addonName,                                             // v0.1.x
		"managed-resources-" + addonName,                                 // v0.1.x alternate
		addonpkg.ManagedResourcePrefix + addonName,                       // v0.4.0-v0.4.3
	}
}

// oldSeedMRNames returns legacy seed-class MR names for an addon.
var oldSeedMRNames = func(addonName string) []string {
	return []string{
		"seed-addon-" + addonName,                                        // v0.2.x
		addonpkg.ManagedResourcePrefix + addonName + "-seed",             // v0.4.0-v0.4.3
		"seed-extension-shoot-addon-service-" + addonName,                // incorrect double prefix
	}
}

// deleteHookData holds pre/post-delete hook manifests for an addon.
type deleteHookData struct {
	PreDelete  [][]byte
	PostDelete [][]byte
}

// actuator implements the extension.Actuator interface.
type actuator struct {
	client             client.Client
	restConfig         *rest.Config
	chartPuller        *oci.ChartPuller
	seedAddonsHash             string
	managedSeedChecked         bool
	managedSeedResult          bool
	seedProviderType           string
	seedProviderChecked        bool
	managedKubernetesProvider  string
	managedKubernetesChecked   bool
	// deleteHooks stores pre/post-delete hook manifests keyed by addon name.
	// Populated during Reconcile, consumed during Delete.
	deleteHooks                map[string]*deleteHookData
	mu                         sync.Mutex
}

// NewActuator creates a new actuator.
func NewActuator(mgr manager.Manager) *actuator {
	cacheDir := os.Getenv("OCI_CHART_CACHE_DIR")
	if cacheDir == "" {
		cacheDir = "/tmp/addon-charts"
	}

	log := mgr.GetLogger().WithName("addon-actuator")
	puller, err := oci.NewChartPuller(cacheDir, log)
	if err != nil {
		log.Error(err, "Failed to create OCI chart puller — OCI charts will not be available")
	}

	return &actuator{
		client:      mgr.GetClient(),
		restConfig:  mgr.GetConfig(),
		chartPuller: puller,
	}
}

// shootMetadata holds extracted shoot info needed for reconciliation.
type shootMetadata struct {
	Name                      string
	Namespace                 string
	Project                   string
	ControlNamespace          string
	Region                    string
	ProviderType              string
	VpcID                     string
	WorkerCIDRs               []string
	NodeRoleName              string
	NodeSecurityGroup         string // from Infrastructure status (AWS)
	Partition                 string
	SeedName                  string
	GCPNodeServiceAccount     string // from Infrastructure status (GCP)
	ClusterRole               string // "runtime", "managed-seed", or "shoot"
	ManagedKubernetesProvider string // "GKE", "EKS", "AKS", or "" for self-managed
}

// Reconcile creates/updates IAM policies, VPC endpoints, and deploys addon
// charts as ManagedResources.
func (a *actuator) Reconcile(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	cluster, err := extensionscontroller.GetCluster(ctx, a.client, ex.Namespace)
	if err != nil {
		return fmt.Errorf("failed to get cluster: %w", err)
	}

	meta, err := a.extractShootMetadata(ctx, log, cluster, ex.Namespace)
	if err != nil {
		return fmt.Errorf("failed to extract shoot metadata: %w", err)
	}

	// Load addon config from ConfigMap (runtime) or embedded FS (fallback)
	manifest, configMapValues, err := a.loadAddonConfig(ctx, log, ex.Namespace)
	if err != nil {
		return fmt.Errorf("failed to load addon config: %w", err)
	}
	if manifest == nil {
		log.Info("No addon configuration found — nothing to deploy")
		return nil
	}

	// Deploy seed-targeted addons when manifest changes.
	a.reconcileSeedAddons(ctx, log, ex.Namespace, meta.ProviderType, manifest, configMapValues)

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

	// GCP-specific features (IAM role bindings) only apply to shoots using provider-gcp.
	var gcpClient *gcputil.Client
	if meta.ProviderType == "gcp" {
		gcpCreds, err := a.getGCPCloudProviderCredentials(ctx, ex.Namespace)
		if err != nil {
			return fmt.Errorf("failed to get GCP cloud provider credentials: %w", err)
		}
		gcpClient, err = gcputil.NewClient(gcpCreds)
		if err != nil {
			return fmt.Errorf("failed to create GCP client: %w", err)
		}
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
	if err := managedresources.CreateForShoot(ctx, a.client, ex.Namespace, mrNamespace, "shoot-addon-service", false, nsData); err != nil {
		return fmt.Errorf("deploy target namespace: %w", err)
	}

	// Deploy registry secrets (shared across all addons)
	if len(manifest.RegistrySecrets) > 0 {
		secretData, err := a.renderRegistrySecrets(ctx, manifest, ex.Namespace)
		if err != nil {
			return fmt.Errorf("render registry secrets: %w", err)
		}
		if err := managedresources.CreateForShoot(ctx, a.client, ex.Namespace, mrRegistrySecrets, "shoot-addon-service", false, secretData); err != nil {
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

	// Global GCP: bind IAM roles to the shoot's node service account
	var currentGCPRoles []string
	if meta.ProviderType == "gcp" && manifest.GlobalGCP != nil && len(manifest.GlobalGCP.IAMRoles) > 0 {
		if meta.GCPNodeServiceAccount == "" {
			log.Info("No GCP node service account found in Infrastructure status, skipping IAM role bindings")
		} else {
			member := fmt.Sprintf("serviceAccount:%s", meta.GCPNodeServiceAccount)
			log.Info("Ensuring global GCP IAM role bindings", "serviceAccount", meta.GCPNodeServiceAccount)
			for _, role := range manifest.GlobalGCP.IAMRoles {
				if err := gcpClient.AddIAMPolicyBinding(ctx, member, role); err != nil {
					return fmt.Errorf("failed to add GCP IAM binding %s for %s: %w", role, member, err)
				}
				log.Info("GCP IAM role bound", "role", role, "member", member)
				currentGCPRoles = append(currentGCPRoles, role)
			}

			// Remove roles that were previously bound but removed from the manifest
			if prevStatus != nil && len(prevStatus.GlobalGCPIAMRoles) > 0 {
				currentSet := make(map[string]bool, len(currentGCPRoles))
				for _, r := range currentGCPRoles {
					currentSet[r] = true
				}
				for _, prevRole := range prevStatus.GlobalGCPIAMRoles {
					if !currentSet[prevRole] {
						log.Info("Removing stale GCP IAM role binding", "role", prevRole, "member", member)
						if err := gcpClient.RemoveIAMPolicyBinding(ctx, member, prevRole); err != nil {
							log.Error(err, "Failed to remove stale GCP IAM role binding", "role", prevRole)
							// Non-fatal — role may have been manually removed already
						}
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

		// Look up per-shoot override for this addon
		var addonOverride *config.AddonOverride
		if cfg.Addons != nil {
			if override, ok := cfg.Addons[addon.Name]; ok {
				addonOverride = &override
			}
		}

		secretData, err := a.renderAddonChart(addon, meta, manifest, configMapValues, addonOverride)
		if err != nil {
			return fmt.Errorf("failed to render chart for addon %s: %w", addon.Name, err)
		}

		log.Info("Deploying addon ManagedResource", "addon", addon.Name, "managedResource", mrName, "targetNamespace", ns)
		if err := managedresources.CreateForShoot(ctx, a.client, ex.Namespace, mrName, "shoot-addon-service", false, secretData); err != nil {
			return fmt.Errorf("failed to deploy ManagedResource for addon %s: %w", addon.Name, err)
		}
	}

	// Clean up legacy ManagedResource names from previous versions.
	// All MRs use keepObjects=true — resources are preserved for the new MR to
	// adopt. This avoids a race condition where the GRM deletes resources from
	// the old MR while the new MR is trying to manage them.
	//
	// The Helm release name is stable (addon.Name), so DaemonSet label selectors
	// are always consistent between old and new MRs. keepObjects=true is safe
	// for all resource types.
	for i := range manifest.Addons {
		addon := &manifest.Addons[i]
		currentName := addon.GetManagedResourceName()
		for _, oldName := range oldShootMRNames(addon.Name) {
			if oldName != currentName {
				a.cleanupRenamedManagedResource(ctx, log, ex.Namespace, oldName, currentName)
			}
		}
	}
	for _, oldName := range oldNamespaceMRNames {
		a.cleanupRenamedManagedResource(ctx, log, ex.Namespace, oldName, mrNamespace)
	}
	for _, oldName := range oldRegistryMRNames {
		a.cleanupRenamedManagedResource(ctx, log, ex.Namespace, oldName, mrRegistrySecrets)
	}

	// Track global IAM policies in status for stale policy detection
	newStatus.GlobalIAMPolicies = currentGlobalPolicies
	newStatus.GlobalGCPIAMRoles = currentGCPRoles
	newStatus.GCPNodeServiceAccount = meta.GCPNodeServiceAccount

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

	meta, err := a.extractShootMetadata(ctx, log, cluster, ex.Namespace)
	if err != nil {
		return fmt.Errorf("failed to extract shoot metadata: %w", err)
	}

	manifest, _, err := a.loadAddonConfig(ctx, log, ex.Namespace)
	if err != nil {
		return fmt.Errorf("failed to load addon config: %w", err)
	}
	if manifest == nil {
		log.Info("No addon configuration found — nothing to clean up")
		return nil
	}

	prevStatus, err := config.GetPreviousStatus(ex)
	if err != nil {
		log.Error(err, "Failed to parse previous status, treating as empty")
		prevStatus = &config.ProviderStatus{}
	}

	log = log.WithValues("shoot", meta.Name, "namespace", meta.Namespace)

	// Collect errors so all cleanup steps run even if some fail
	var errs []error

	// 0. Execute pre-delete hooks for addons that have them.
	// If deleteFailurePolicy is "Abort" and a hook fails, addon removal is blocked.
	targetNS := manifest.DefaultNamespace
	for i := range manifest.Addons {
		addon := &manifest.Addons[i]
		if addon.Hooks != nil && addon.Hooks.Include {
			if err := a.executeDeleteHooks(ctx, log, targetNS, addon.Name, "pre-delete"); err != nil {
				if addon.Hooks.ShouldAbortOnDeleteFailure() {
					return fmt.Errorf("pre-delete hook failed for addon %s (policy: Abort): %w", addon.Name, err)
				}
				log.Info("Pre-delete hook failed, continuing with deletion (policy: Continue)", "addon", addon.Name, "error", err)
			}
		}
	}

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

	// 1a. Execute post-delete hooks after MR deletion
	for i := range manifest.Addons {
		addon := &manifest.Addons[i]
		if addon.Hooks != nil && addon.Hooks.Include {
			if err := a.executeDeleteHooks(ctx, log, targetNS, addon.Name, "post-delete"); err != nil {
				if addon.Hooks.ShouldAbortOnDeleteFailure() {
					errs = append(errs, fmt.Errorf("post-delete hook failed for addon %s: %w", addon.Name, err))
				} else {
					log.Info("Post-delete hook failed, continuing (policy: Continue)", "addon", addon.Name, "error", err)
				}
			}
		}
	}

	// 1b. Delete namespace ManagedResource (current + legacy names)
	log.Info("Deleting namespace ManagedResource")
	if err := a.deleteManagedResource(ctx, ex.Namespace, mrNamespace); err != nil {
		log.Error(err, "Failed to delete namespace ManagedResource")
		errs = append(errs, err)
	}
	for _, oldName := range oldNamespaceMRNames {
		_ = a.deleteManagedResource(ctx, ex.Namespace, oldName) // best-effort cleanup
	}

	// 1c. Delete registry secrets ManagedResource
	if len(manifest.RegistrySecrets) > 0 {
		log.Info("Deleting registry secrets ManagedResource")
		if err := a.deleteManagedResource(ctx, ex.Namespace, mrRegistrySecrets); err != nil {
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

	// 3. Clean up GCP resources if this is a GCP shoot
	if meta.ProviderType == "gcp" {
		gcpCreds, err := a.getGCPCloudProviderCredentials(ctx, ex.Namespace)
		if err != nil {
			log.Error(err, "Failed to get GCP cloud provider credentials, skipping GCP cleanup")
		} else {
			gcpClient, err := gcputil.NewClient(gcpCreds)
			if err != nil {
				log.Error(err, "Failed to create GCP client")
			} else {
				// Determine the node service account from previous status or current metadata
				nodeServiceAccount := meta.GCPNodeServiceAccount
				if nodeServiceAccount == "" && prevStatus != nil {
					nodeServiceAccount = prevStatus.GCPNodeServiceAccount
				}

				if nodeServiceAccount != "" {
					member := fmt.Sprintf("serviceAccount:%s", nodeServiceAccount)

					// Unbind roles from manifest
					if manifest.GlobalGCP != nil && len(manifest.GlobalGCP.IAMRoles) > 0 {
						log.Info("Removing GCP IAM role bindings", "serviceAccount", nodeServiceAccount)
						for _, role := range manifest.GlobalGCP.IAMRoles {
							if err := gcpClient.RemoveIAMPolicyBinding(ctx, member, role); err != nil {
								log.Error(err, "Failed to remove GCP IAM role binding", "role", role)
								errs = append(errs, err)
							}
						}
					}

					// Also unbind any roles tracked in previous status that may have
					// been removed from the manifest since the last reconcile
					if prevStatus != nil && len(prevStatus.GlobalGCPIAMRoles) > 0 {
						for _, role := range prevStatus.GlobalGCPIAMRoles {
							if err := gcpClient.RemoveIAMPolicyBinding(ctx, member, role); err != nil {
								log.Error(err, "Failed to remove previously tracked GCP IAM role binding", "role", role)
								// Non-fatal
							}
						}
					}
				} else {
					log.Info("No GCP node service account available, skipping IAM cleanup")
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
// Runtime addon config loading
// --------------------------------------------------------------------------

// loadAddonConfig reads addon configuration from the ConfigMap first, falling
// back to embedded addons if the ConfigMap doesn't exist. Returns the manifest,
// a map of values files from the ConfigMap (nil if using embedded), and an error.
func (a *actuator) loadAddonConfig(ctx context.Context, log logr.Logger, namespace string) (*addonpkg.AddonManifest, map[string]string, error) {
	// The ConfigMap lives in the extension's own namespace (where the controller
	// pod runs), not in the shoot's control plane namespace.
	extensionNS := getExtensionNamespace()
	if extensionNS == "" {
		extensionNS = namespace
	}

	cm := &corev1.ConfigMap{}
	err := a.client.Get(ctx, types.NamespacedName{
		Name:      ConfigMapName,
		Namespace: extensionNS,
	}, cm)

	if err == nil {
		// ConfigMap exists — parse manifest from it
		log.Info("Using addon configuration from ConfigMap")
		manifest, err := addonpkg.ReadManifestFromData(cm.Data["manifest.yaml"])
		if err != nil {
			return nil, nil, fmt.Errorf("parse ConfigMap manifest: %w", err)
		}
		return manifest, cm.Data, nil
	}

	if !apierrors.IsNotFound(err) {
		return nil, nil, fmt.Errorf("get addon config ConfigMap: %w", err)
	}

	// ConfigMap not found — try embedded fallback
	manifest, err := addonpkg.ReadManifest(embedded.Addons)
	if err != nil {
		log.Info("No addon ConfigMap found and no embedded addons — nothing to deploy")
		return nil, nil, nil
	}

	log.Info("Using embedded addon configuration (no ConfigMap found)")
	return manifest, nil, nil
}

// parseYAMLValues parses a YAML string into a map for chart values merging.
func parseYAMLValues(raw string) (map[string]interface{}, error) {
	var vals map[string]interface{}
	if err := yaml.Unmarshal([]byte(raw), &vals); err != nil {
		return nil, err
	}
	if vals == nil {
		vals = map[string]interface{}{}
	}
	return vals, nil
}

// isManagedSeed checks if this seed is also a shoot managed by a parent seed.
// On managed seeds, the parent extension deploys addons via shoot-class MRs,
// so the managed seed's own extension should skip seed addon deployment.
//
// Detection: a managed seed has its gardenlet deployed by a parent. The parent
// creates a control plane namespace like shoot--garden--<seedName> on the parent
// cluster. But we can't check the parent cluster from here.
//
// Simpler: check if this seed's name appears as a shoot in the garden API.
// If GARDEN_KUBECONFIG is available, query for a Shoot with the seed's name.
// If found, this seed is managed.
// isManagedSeed checks if this seed is also a shoot managed by a parent seed.
// Cached after first check — seed status doesn't change during controller lifetime.
func (a *actuator) isManagedSeed(ctx context.Context, log logr.Logger, seedName string) bool {
	if seedName == "" {
		return false
	}

	a.mu.Lock()
	if a.managedSeedChecked {
		result := a.managedSeedResult
		a.mu.Unlock()
		return result
	}
	a.mu.Unlock()

	result := a.checkManagedSeed(ctx, log, seedName)

	a.mu.Lock()
	a.managedSeedChecked = true
	a.managedSeedResult = result
	a.mu.Unlock()

	return result
}

func (a *actuator) checkManagedSeed(ctx context.Context, log logr.Logger, seedName string) bool {
	gardenClient, err := a.getGardenClient()
	if err != nil {
		return false
	}

	shootList := &gardencorev1beta1.ShootList{}
	if err := gardenClient.List(ctx, shootList); err != nil {
		log.Error(err, "Failed to list shoots for managed seed detection")
		return false
	}

	for _, s := range shootList.Items {
		if s.Name == seedName {
			log.Info("Seed is a managed seed (shoot exists with same name)",
				"seedName", seedName, "shootNamespace", s.Namespace)
			return true
		}
	}

	return false
}

// getSeedProviderType returns the provider type of the seed/runtime cluster by
// querying the Seed object from the garden API. Cached after first check.
// Falls back to the shoot's provider type if the garden API is unavailable.
func (a *actuator) getSeedProviderType(ctx context.Context, log logr.Logger, seedName string) string {
	if seedName == "" {
		return ""
	}

	a.mu.Lock()
	if a.seedProviderChecked {
		result := a.seedProviderType
		a.mu.Unlock()
		return result
	}
	a.mu.Unlock()

	result := a.lookupSeedProviderType(ctx, log, seedName)

	a.mu.Lock()
	a.seedProviderChecked = true
	a.seedProviderType = result
	a.mu.Unlock()

	return result
}

func (a *actuator) lookupSeedProviderType(ctx context.Context, log logr.Logger, seedName string) string {
	gardenClient, err := a.getGardenClient()
	if err != nil {
		log.Info("Garden client unavailable, cannot determine seed provider type", "error", err)
		return ""
	}

	seed := &gardencorev1beta1.Seed{}
	if err := gardenClient.Get(ctx, types.NamespacedName{Name: seedName}, seed); err != nil {
		log.Error(err, "Failed to get Seed object for provider type detection", "seedName", seedName)
		return ""
	}

	providerType := seed.Spec.Provider.Type
	log.Info("Detected seed provider type", "seedName", seedName, "providerType", providerType)
	return providerType
}

// shootIsManagedSeed checks if a shoot is also a managed seed (a Seed object
// exists with the same name in the garden cluster). Returns false if the
// garden API is unavailable.
func (a *actuator) shootIsManagedSeed(ctx context.Context, log logr.Logger, shootName string) bool {
	gardenClient, err := a.getGardenClient()
	if err != nil {
		return false
	}

	seed := &gardencorev1beta1.Seed{}
	if err := gardenClient.Get(ctx, types.NamespacedName{Name: shootName}, seed); err != nil {
		// Not found means this shoot is not a managed seed, which is the common case.
		return false
	}
	return true
}

// getManagedKubernetesProvider returns the cloud-provider-managed Kubernetes
// distribution running on the runtime cluster (e.g., "GKE", "EKS", "AKS").
// Returns "" if the runtime is self-managed Kubernetes (e.g., kubeadm, kops,
// or a Gardener-provisioned shoot acting as a seed).
//
// Resolution order:
//  1. MANAGED_KUBERNETES_PROVIDER env var (operator override, always wins)
//  2. Auto-detection via node labels (uses uncached API call to avoid
//     poisoning the controller-runtime cache if RBAC denies node access)
//
// Cached after first lookup.
func (a *actuator) getManagedKubernetesProvider(ctx context.Context, log logr.Logger) string {
	a.mu.Lock()
	if a.managedKubernetesChecked {
		result := a.managedKubernetesProvider
		a.mu.Unlock()
		return result
	}
	a.mu.Unlock()

	// Check for operator override first
	result := os.Getenv("MANAGED_KUBERNETES_PROVIDER")
	if result != "" {
		log.Info("Using managed Kubernetes provider from env var", "provider", result)
	} else {
		result = a.detectManagedKubernetesProvider(ctx, log)
	}

	a.mu.Lock()
	a.managedKubernetesChecked = true
	a.managedKubernetesProvider = result
	a.mu.Unlock()

	return result
}

func (a *actuator) detectManagedKubernetesProvider(ctx context.Context, log logr.Logger) string {
	// Use a direct (uncached) API client to list nodes. The controller-runtime
	// cached client (a.client) sets up a watch reflector for every resource type
	// it touches. If the extension's ServiceAccount lacks nodes RBAC, the
	// reflector enters an infinite retry loop that floods logs and wastes
	// resources. A direct client avoids this — if the call fails, nothing is
	// left behind.
	directClient, err := client.New(a.restConfig, client.Options{})
	if err != nil {
		log.Info("Failed to create direct client for node detection", "error", err)
		return ""
	}

	nodeList := &corev1.NodeList{}
	if err := directClient.List(ctx, nodeList, &client.ListOptions{Limit: 1}); err != nil {
		log.Info("Cannot detect managed Kubernetes provider (nodes not accessible, set MANAGED_KUBERNETES_PROVIDER env var to override)", "error", err)
		return ""
	}
	if len(nodeList.Items) == 0 {
		return ""
	}

	// Detection is keyed on node labels set by each managed Kubernetes service.
	// Add cases here when new services need to be supported — see docs/usage.md
	// for the list of currently detected providers.
	labels := nodeList.Items[0].Labels
	switch {
	case hasLabelPrefix(labels, "cloud.google.com/gke-"):
		return "GKE"
	case hasLabelPrefix(labels, "eks.amazonaws.com/"):
		return "EKS"
	case hasLabelPrefix(labels, "kubernetes.azure.com/"):
		return "AKS"
	case hasLabel(labels, "node.openshift.io/os_id"):
		return "OpenShift"
	}
	return ""
}

// hasLabel returns true if the labels map has the given key.
func hasLabel(labels map[string]string, key string) bool {
	_, ok := labels[key]
	return ok
}

// hasLabelPrefix returns true if any label key starts with the given prefix.
func hasLabelPrefix(labels map[string]string, prefix string) bool {
	for k := range labels {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// getExtensionNamespace returns the namespace where the extension controller pod
// runs. This is where the addon ConfigMap is deployed by the Helm chart.
func getExtensionNamespace() string {
	// Try reading from the pod's service account namespace file
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err == nil && len(data) > 0 {
		return string(data)
	}
	return ""
}

// --------------------------------------------------------------------------
// Seed/runtime addon deployment
// --------------------------------------------------------------------------

// reconcileSeedAddons deploys addons with target "seed" or "global" to the
// seed/runtime cluster as seed-class ManagedResources. Uses hash-based change
// detection to redeploy when the manifest changes (e.g., ConfigMap update).
func (a *actuator) reconcileSeedAddons(ctx context.Context, log logr.Logger, namespace string, providerType string, manifest *addonpkg.AddonManifest, configMapValues map[string]string) {
	// On managed seeds, the parent seed's extension already deploys addons via
	// shoot-class ManagedResources into the shoot cluster (which IS the managed
	// seed). Skip seed addon deployment to avoid duplicates.
	//
	// Detection: check if this seed is also a shoot by looking for a shoot-class
	// addon ManagedResource targeting this seed. If an addon MR already exists
	// from the parent extension, skip seed addons.
	seedName := os.Getenv("SEED_NAME")
	if a.isManagedSeed(ctx, log, seedName) {
		log.Info("Skipping seed addon deployment — managed seed, parent extension deploys via shoot MR",
			"seedName", seedName)
		// Clean up any stale seed MRs (current + legacy names).
		for _, name := range append(oldSeedNamespaceMRNames, mrNamespaceSeed) {
			if err := managedresources.DeleteForSeed(ctx, a.client, namespace, name); err == nil {
				log.Info("Cleaned up stale seed ManagedResource", "managedResource", name)
			}
		}
		for i := range manifest.Addons {
			addon := &manifest.Addons[i]
			if addon.DeploysToSeed() {
				mrName := addon.GetSeedManagedResourceName()
				if err := managedresources.DeleteForSeed(ctx, a.client, namespace, mrName); err == nil {
					log.Info("Cleaned up stale seed ManagedResource", "managedResource", mrName)
				}
				for _, oldName := range oldSeedMRNames(addon.Name) {
					if err := managedresources.DeleteForSeed(ctx, a.client, namespace, oldName); err == nil {
						log.Info("Cleaned up stale seed ManagedResource", "managedResource", oldName)
					}
				}
			}
		}
		return
	}

	region := os.Getenv("REGION")
	if region == "" {
		region = "us-east-1"
	}

	// Use the seed/runtime's own provider type for seed addon rendering,
	// not the shoot's provider type. The seed DaemonSet runs on the runtime
	// nodes, so provider-specific values (e.g., CloudWatch vs Stackdriver)
	// must match the runtime's cloud provider.
	seedProvider := a.getSeedProviderType(ctx, log, seedName)
	if seedProvider == "" {
		seedProvider = providerType // fallback to shoot provider if garden API unavailable
	}

	// Detect if the runtime is a cloud-provider-managed Kubernetes service
	// (GKE, EKS, AKS) for use in addon templates that need to differentiate
	// cluster types. Empty if self-managed Kubernetes.
	managedK8s := a.getManagedKubernetesProvider(ctx, log)

	// Seed addons only run on raw runtime clusters — managed seeds are skipped
	// earlier in this function via isManagedSeed. So ClusterRole is always
	// "runtime" at this point.
	meta := &shootMetadata{
		Name:                      seedName,
		SeedName:                  seedName,
		Project:                   seedName,
		Region:                    region,
		ProviderType:              seedProvider,
		ClusterRole:               "runtime",
		ManagedKubernetesProvider: managedK8s,
	}

	// Render all seed addon charts first, then hash the rendered output.
	// This ensures any change — manifest, config values, template variables,
	// or code — triggers re-deployment.
	type renderedAddon struct {
		addon      *addonpkg.Addon
		mrName     string
		secretData map[string][]byte
	}
	var rendered []renderedAddon
	h := sha256.New()

	for i := range manifest.Addons {
		addon := &manifest.Addons[i]
		if !addon.DeploysToSeed() || !addon.Enabled {
			continue
		}

		secretData, err := a.renderAddonChart(addon, meta, manifest, configMapValues, nil)
		if err != nil {
			log.Error(err, "Failed to render seed addon chart", "addon", addon.Name)
			continue
		}

		mrName := addon.GetSeedManagedResourceName()
		rendered = append(rendered, renderedAddon{addon, mrName, secretData})

		// Hash the rendered output for change detection
		for k, v := range secretData {
			h.Write([]byte(k))
			h.Write(v)
		}
	}

	outputHash := fmt.Sprintf("%x", h.Sum(nil))[:16]

	a.mu.Lock()
	if a.seedAddonsHash == outputHash {
		a.mu.Unlock()
		return // No change since last reconciliation
	}
	a.seedAddonsHash = outputHash
	a.mu.Unlock()

	log.Info("Reconciling seed-targeted addons", "outputHash", outputHash)

	// Deploy target namespace with gardener.cloud/role: extension label so the
	// seed GRM's restricted informer cache includes it. Without this label, the
	// GRM can't manage resources in the namespace on newer Gardener versions.
	targetNS := manifest.DefaultNamespace
	nsData := map[string][]byte{
		"namespace.yaml": []byte(fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
  labels:
    pod-security.kubernetes.io/enforce: privileged
    gardener.cloud/role: extension
`, targetNS)),
	}
	if err := managedresources.CreateForSeed(ctx, a.client, namespace, mrNamespaceSeed, false, nsData); err != nil {
		log.Error(err, "Failed to deploy seed addon namespace")
		return
	}

	for _, r := range rendered {
		log.Info("Deploying seed addon ManagedResource", "addon", r.addon.Name, "managedResource", r.mrName)
		if err := managedresources.CreateForSeed(ctx, a.client, namespace, r.mrName, false, r.secretData); err != nil {
			log.Error(err, "Failed to deploy seed addon ManagedResource", "addon", r.addon.Name)
		}
	}

	log.Info("Seed addon reconciliation complete")
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
func (a *actuator) extractShootMetadata(ctx context.Context, log logr.Logger, cluster *extensionscontroller.Cluster, namespace string) (*shootMetadata, error) {
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

	// Extract node security group from Infrastructure status (AWS)
	nodeSG := a.getNodeSecurityGroupFromInfraStatus(cluster, namespace)

	// Extract GCP node service account from Infrastructure status (GCP)
	gcpNodeSA := a.getGCPNodeServiceAccountFromInfraStatus(cluster, namespace)

	seedName := ""
	if shoot.Spec.SeedName != nil {
		seedName = *shoot.Spec.SeedName
	}

	// Detect if this shoot is also a managed seed (a Seed object exists with
	// the same name). Used by addon templates to differentiate cluster role.
	clusterRole := "shoot"
	if a.shootIsManagedSeed(ctx, log, shoot.Name) {
		clusterRole = "managed-seed"
	}

	return &shootMetadata{
		Name:                      shoot.Name,
		Namespace:                 shoot.Namespace,
		Project:                   project,
		ControlNamespace:          namespace,
		Region:                    region,
		ProviderType:              shoot.Spec.Provider.Type,
		VpcID:                     vpcID,
		WorkerCIDRs:               workerCIDRs,
		NodeRoleName:              fmt.Sprintf("%s-nodes", namespace),
		NodeSecurityGroup:         nodeSG,
		Partition:                 partition,
		SeedName:                  seedName,
		GCPNodeServiceAccount:     gcpNodeSA,
		ClusterRole:               clusterRole,
		ManagedKubernetesProvider: "", // shoots are vanilla Kubernetes
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
// control plane namespace. Supports two credential formats:
//   - Static credentials: accessKeyID + secretAccessKey fields
//   - Workload Identity: roleARN + token fields (STS AssumeRoleWithWebIdentity)
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
	roleARN := string(secret.Data["roleARN"])
	token := string(secret.Data["token"])

	// Static credentials (standard shoots)
	if accessKeyID != "" && secretAccessKey != "" {
		return &awsutil.Credentials{
			AccessKeyID:    accessKeyID,
			SecretAccessKey: secretAccessKey,
			RoleARN:        roleARN,
			Token:          token,
		}, nil
	}

	// Workload Identity credentials (managed seeds, workload identity shoots)
	if roleARN != "" && token != "" {
		return &awsutil.Credentials{
			RoleARN:          roleARN,
			WebIdentityToken: token,
		}, nil
	}

	return nil, fmt.Errorf("cloudprovider secret in %s has no valid credentials: need accessKeyID+secretAccessKey or roleARN+token", namespace)
}

// getGCPCloudProviderCredentials reads the cloudprovider secret from the shoot's
// control plane namespace. Supports two credential formats:
//   - Static: serviceaccount.json field (standard shoots)
//   - Workload Identity Federation: credentialsConfig + token + projectID (managed seeds)
func (a *actuator) getGCPCloudProviderCredentials(ctx context.Context, namespace string) (*gcputil.Credentials, error) {
	secret := &corev1.Secret{}
	if err := a.client.Get(ctx, types.NamespacedName{
		Name:      "cloudprovider",
		Namespace: namespace,
	}, secret); err != nil {
		return nil, fmt.Errorf("failed to get cloudprovider secret in %s: %w", namespace, err)
	}

	// Static credentials (standard shoots)
	serviceAccountJSON := secret.Data["serviceaccount.json"]
	if len(serviceAccountJSON) > 0 {
		return &gcputil.Credentials{
			ServiceAccountJSON: serviceAccountJSON,
		}, nil
	}

	// Workload Identity Federation (managed seeds)
	credentialsConfig := secret.Data["credentialsConfig"]
	token := string(secret.Data["token"])
	projectID := string(secret.Data["projectID"])
	if len(credentialsConfig) > 0 && token != "" {
		return &gcputil.Credentials{
			CredentialsConfig: credentialsConfig,
			Token:             token,
			ProjectID:         projectID,
		}, nil
	}

	return nil, fmt.Errorf("cloudprovider secret in %s has no valid GCP credentials: need serviceaccount.json or credentialsConfig+token", namespace)
}

// getGCPNodeServiceAccountFromInfraStatus reads the node service account email
// from the Infrastructure status providerStatus. The GCP Infrastructure controller
// stores this after creating the shoot's GCP infrastructure.
//
// Path: infrastructure.status.providerStatus.serviceAccountEmail
func (a *actuator) getGCPNodeServiceAccountFromInfraStatus(cluster *extensionscontroller.Cluster, namespace string) string {
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

	var status struct {
		ServiceAccountEmail string `json:"serviceAccountEmail"`
	}

	if err := json.Unmarshal(infra.Status.ProviderStatus.Raw, &status); err != nil {
		return ""
	}

	return status.ServiceAccountEmail
}

// deleteManagedResource deletes a ManagedResource and waits for it to be gone.
func (a *actuator) deleteManagedResource(ctx context.Context, namespace, name string) error {
	if err := managedresources.DeleteForShoot(ctx, a.client, namespace, name); err != nil {
		return err
	}
	return managedresources.WaitUntilDeleted(ctx, a.client, namespace, name)
}

// cleanupRenamedManagedResource deletes a legacy ManagedResource that has been
// replaced by a new MR with a different name but the same underlying resources.
//
// Flow:
//  1. Snapshot the old MR's resource inventory from status.resources
//  2. Set keepObjects=true and delete the old MR (resources stay)
//  3. Read the new MR's resource inventory
//  4. Delete orphaned resources (in old but not in new)
//
// Uses a direct (uncached) API client to avoid registering ManagedResource
// with the controller-runtime cache, which would trigger background watch
// reflectors and potentially cause RBAC-related cache poisoning.
func (a *actuator) cleanupRenamedManagedResource(ctx context.Context, log logr.Logger, namespace, oldName, newName string) {
	directClient, err := client.New(a.restConfig, client.Options{})
	if err != nil {
		log.Info("Failed to create direct client for MR cleanup", "error", err)
		return
	}

	// 1. Read old MR and snapshot its resource inventory
	oldMR := &resourcesv1alpha1.ManagedResource{}
	key := types.NamespacedName{Name: oldName, Namespace: namespace}
	if err := directClient.Get(ctx, key, oldMR); err != nil {
		return // not found — already cleaned up
	}
	oldResources := oldMR.Status.Resources

	// 2. Set keepObjects=true and delete the old MR
	keepObjects := true
	oldMR.Spec.KeepObjects = &keepObjects
	if err := directClient.Update(ctx, oldMR); err != nil {
		log.Info("Failed to set keepObjects on old ManagedResource", "old", oldName, "error", err)
		return
	}

	if err := directClient.Delete(ctx, oldMR); err != nil {
		log.Info("Failed to delete old ManagedResource", "old", oldName, "error", err)
		return
	}

	log.Info("Cleaned up renamed ManagedResource (keepObjects=true)", "old", oldName, "new", newName)

	// 3. Read new MR's resource inventory for orphan detection
	if len(oldResources) == 0 {
		return // nothing to compare
	}

	newMR := &resourcesv1alpha1.ManagedResource{}
	newKey := types.NamespacedName{Name: newName, Namespace: namespace}
	if err := directClient.Get(ctx, newKey, newMR); err != nil {
		log.Info("Cannot read new MR for orphan detection, skipping", "new", newName, "error", err)
		return
	}

	// 4. Build set of new MR's resources for fast lookup
	newSet := make(map[string]bool, len(newMR.Status.Resources))
	for _, ref := range newMR.Status.Resources {
		newSet[resourceKey(ref)] = true
	}

	// 5. Delete orphans: resources in old MR but not in new MR
	for _, ref := range oldResources {
		if newSet[resourceKey(ref)] {
			continue // owned by new MR, skip
		}
		if err := deleteOrphanedResource(ctx, directClient, ref); err != nil {
			log.Info("Failed to delete orphaned resource", "resource", resourceKey(ref), "error", err)
		} else {
			log.Info("Deleted orphaned resource from old ManagedResource", "resource", resourceKey(ref), "old", oldName)
		}
	}
}

// resourceKey returns a unique string key for a ManagedResource ObjectReference.
func resourceKey(ref resourcesv1alpha1.ObjectReference) string {
	return fmt.Sprintf("%s/%s/%s/%s", ref.GroupVersionKind().Group, ref.GroupVersionKind().Kind, ref.Namespace, ref.Name)
}

// deleteOrphanedResource deletes a single resource identified by an ObjectReference.
func deleteOrphanedResource(ctx context.Context, c client.Client, ref resourcesv1alpha1.ObjectReference) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(ref.GroupVersionKind())
	obj.SetName(ref.Name)
	obj.SetNamespace(ref.Namespace)
	return client.IgnoreNotFound(c.Delete(ctx, obj))
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
func (a *actuator) renderAddonChart(addon *addonpkg.Addon, meta *shootMetadata, manifest *addonpkg.AddonManifest, configMapValues map[string]string, perShootOverride *config.AddonOverride) (map[string][]byte, error) {
	merged := map[string]interface{}{}

	if configMapValues != nil {
		// ConfigMap mode: values come from ConfigMap keys
		baseKey := fmt.Sprintf("values.%s.yaml", addon.Name)
		if raw, ok := configMapValues[baseKey]; ok {
			baseVals, err := parseYAMLValues(raw)
			if err != nil {
				return nil, fmt.Errorf("parse ConfigMap values %s: %w", baseKey, err)
			}
			merged = baseVals
		}

		// Provider-specific values from ConfigMap
		if meta.ProviderType != "" {
			providerKey := fmt.Sprintf("values.%s.%s.yaml", addon.Name, meta.ProviderType)
			if raw, ok := configMapValues[providerKey]; ok {
				providerVals, err := parseYAMLValues(raw)
				if err != nil {
					return nil, fmt.Errorf("parse ConfigMap values %s: %w", providerKey, err)
				}
				merged = mergeMaps(merged, providerVals)
			}
		}
	} else if addon.ValuesPath != "" {
		// Embedded mode: values from embedded FS (existing logic)
		baseVals, err := readEmbeddedValues(embedded.Addons, "addons/"+addon.ValuesPath+"/values.yaml")
		if err != nil {
			if !isNotExist(err) {
				return nil, fmt.Errorf("read base values: %w", err)
			}
		} else {
			merged = baseVals
		}

		if meta.ProviderType != "" {
			providerFile := fmt.Sprintf("addons/%s/values.%s.yaml", addon.ValuesPath, meta.ProviderType)
			providerVals, err := readEmbeddedValues(embedded.Addons, providerFile)
			if err != nil {
				if !isNotExist(err) {
					return nil, fmt.Errorf("read provider values: %w", err)
				}
			} else {
				merged = mergeMaps(merged, providerVals)
			}
		}
	}

	// Shoot values from the manifest (with template expansion)
	if addon.ShootValues != nil {
		expanded := expandShootValues(addon.ShootValues, meta)
		merged = mergeMaps(merged, expanded)
	}

	// imagePullSecrets from addon manifest
	if len(addon.ImagePullSecrets) > 0 {
		pullSecrets := make([]map[string]interface{}, len(addon.ImagePullSecrets))
		for i, name := range addon.ImagePullSecrets {
			pullSecrets[i] = map[string]interface{}{"name": name}
		}
		merged["imagePullSecrets"] = pullSecrets
	}

	// Image overrides from environment variables
	if addon.Image != nil {
		applyImageOverride(merged, addon)
	}

	// Per-shoot values override (highest priority — for debugging/testing)
	if perShootOverride != nil && perShootOverride.ValuesOverride != "" {
		overrideVals, err := parseYAMLValues(perShootOverride.ValuesOverride)
		if err != nil {
			return nil, fmt.Errorf("parse per-shoot values override for %s: %w", addon.Name, err)
		}
		if perShootOverride.IsOverrideMode() {
			// Full replace — discard all previous values
			merged = overrideVals
		} else {
			// Merge (default) — additive, only specified keys change
			merged = mergeMaps(merged, overrideVals)
		}
	}

	ns := addon.GetNamespace(manifest.DefaultNamespace)
	// Use addon name as the Helm release name, NOT the MR name. The release
	// name sets app.kubernetes.io/instance labels, which are immutable on
	// DaemonSets. Changing the release name would break existing deployments.
	releaseName := addon.Name

	// Hook-aware rendering path: when addon has hooks.include: true, use
	// the hook-aware renderer which includes hook-annotated templates and
	// captures delete hooks separately.
	if addon.Hooks != nil && addon.Hooks.Include {
		return a.renderAddonChartWithHooks(addon, releaseName, ns, merged)
	}

	// Standard rendering path: Gardener chartrenderer (hooks silently dropped)
	renderer, err := gardenerchartrenderer.NewForConfig(a.restConfig)
	if err != nil {
		return nil, fmt.Errorf("create chart renderer: %w", err)
	}

	var rendered *gardenerchartrenderer.RenderedChart

	if addon.Chart.OCI != "" {
		archive, err := a.pullOCIChart(addon)
		if err != nil {
			return nil, fmt.Errorf("pull OCI chart %s: %w", addon.Chart.OCI, err)
		}
		rendered, err = renderer.RenderArchive(archive, releaseName, ns, merged)
		if err != nil {
			return nil, fmt.Errorf("render OCI chart %s: %w", addon.Chart.OCI, err)
		}
	} else if addon.Chart.Path != "" {
		chartPath := "addons/" + addon.Chart.Path
		rendered, err = renderer.RenderEmbeddedFS(embedded.Addons, chartPath, releaseName, ns, merged)
		if err != nil {
			return nil, fmt.Errorf("render embedded chart %s: %w", chartPath, err)
		}
	} else {
		return nil, fmt.Errorf("addon %s: no chart source (oci or path) specified", addon.Name)
	}

	return rendered.AsSecretData(), nil
}

// renderAddonChartWithHooks uses the hook-aware renderer to include Helm
// hook-annotated templates. Delete hooks are stored in a separate secret
// for execution during addon removal.
func (a *actuator) renderAddonChartWithHooks(addon *addonpkg.Addon, releaseName, namespace string, values map[string]interface{}) (map[string][]byte, error) {
	disc, err := discovery.NewDiscoveryClientForConfig(a.restConfig)
	if err != nil {
		return nil, fmt.Errorf("create discovery client: %w", err)
	}
	sv, err := disc.ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("get server version: %w", err)
	}

	hookRenderer := hookaware.NewHookAwareRenderer(sv)

	hookCfg := &hookaware.HookConfig{
		Include:          true,
		StripAnnotations: addon.Hooks.ShouldStripAnnotations(),
		DeleteTimeout:    addon.Hooks.GetDeleteTimeout(),
		ExcludeTypes:     addon.Hooks.GetExcludeTypes(),
	}

	var result *hookaware.RenderResult

	if addon.Chart.OCI != "" {
		archive, err := a.pullOCIChart(addon)
		if err != nil {
			return nil, fmt.Errorf("pull OCI chart %s: %w", addon.Chart.OCI, err)
		}
		result, err = hookRenderer.RenderArchive(archive, releaseName, namespace, values, hookCfg)
		if err != nil {
			return nil, fmt.Errorf("render OCI chart with hooks %s: %w", addon.Chart.OCI, err)
		}
	} else if addon.Chart.Path != "" {
		chartPath := "addons/" + addon.Chart.Path
		chart, err := loadEmbeddedChart(embedded.Addons, chartPath)
		if err != nil {
			return nil, fmt.Errorf("load embedded chart %s: %w", chartPath, err)
		}
		result, err = hookRenderer.RenderChart(chart, releaseName, namespace, values, hookCfg)
		if err != nil {
			return nil, fmt.Errorf("render embedded chart with hooks %s: %w", chartPath, err)
		}
	} else {
		return nil, fmt.Errorf("addon %s: no chart source (oci or path) specified", addon.Name)
	}

	// Store delete hooks for later execution during addon removal.
	// These are saved as a separate secret, not as part of the MR.
	if len(result.PreDeleteHooks) > 0 || len(result.PostDeleteHooks) > 0 {
		a.storeDeleteHooks(addon.Name, result.PreDeleteHooks, result.PostDeleteHooks)
	}

	return result.MRData, nil
}

// storeDeleteHooks saves pre/post-delete hook manifests for later execution.
func (a *actuator) storeDeleteHooks(addonName string, preDelete, postDelete [][]byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.deleteHooks == nil {
		a.deleteHooks = make(map[string]*deleteHookData)
	}
	a.deleteHooks[addonName] = &deleteHookData{
		PreDelete:  preDelete,
		PostDelete: postDelete,
	}
}

// executeDeleteHooks runs pre-delete or post-delete hook manifests directly
// on the cluster (not via MR). Uses an uncached client. Waits for Jobs to
// complete up to the configured timeout. Returns an error if any hook
// resource fails to apply or a Job fails/times out.
func (a *actuator) executeDeleteHooks(ctx context.Context, log logr.Logger, namespace, addonName string, hookType string) error {
	a.mu.Lock()
	hooks, ok := a.deleteHooks[addonName]
	a.mu.Unlock()
	if !ok {
		return nil
	}

	var manifests [][]byte
	switch hookType {
	case "pre-delete":
		manifests = hooks.PreDelete
	case "post-delete":
		manifests = hooks.PostDelete
	}

	if len(manifests) == 0 {
		return nil
	}

	directClient, err := client.New(a.restConfig, client.Options{})
	if err != nil {
		return fmt.Errorf("create direct client for delete hooks: %w", err)
	}

	log.Info("Executing delete hooks", "addon", addonName, "hookType", hookType, "count", len(manifests))

	for _, manifest := range manifests {
		// Apply the hook resource directly
		objs, err := parseManifest(manifest)
		if err != nil {
			log.Info("Failed to parse delete hook manifest", "addon", addonName, "error", err)
			continue
		}
		for _, obj := range objs {
			obj.SetNamespace(namespace)
			if err := directClient.Create(ctx, obj); err != nil {
				if apierrors.IsAlreadyExists(err) {
					// Update if already exists
					if err := directClient.Update(ctx, obj); err != nil {
						log.Info("Failed to update delete hook resource", "addon", addonName, "kind", obj.GetKind(), "name", obj.GetName(), "error", err)
					}
				} else {
					log.Info("Failed to create delete hook resource", "addon", addonName, "kind", obj.GetKind(), "name", obj.GetName(), "error", err)
				}
			} else {
				log.Info("Applied delete hook resource", "addon", addonName, "kind", obj.GetKind(), "name", obj.GetName())
			}
		}
	}

	// Wait for any Jobs to complete
	return a.waitForHookJobs(ctx, log, directClient, namespace, addonName, hookType)
}

// waitForHookJobs waits for delete hook Jobs to complete with a timeout.
// Returns an error if any Job fails or times out.
func (a *actuator) waitForHookJobs(ctx context.Context, log logr.Logger, c client.Client, namespace, addonName, hookType string) error {
	timeout := 300 // default

	jobList := &batchv1.JobList{}
	if err := c.List(ctx, jobList, &client.ListOptions{Namespace: namespace}); err != nil {
		return fmt.Errorf("list Jobs for hook completion: %w", err)
	}

	var hookErr error
	for _, job := range jobList.Items {
		if job.CreationTimestamp.Time.Before(time.Now().Add(-5 * time.Minute)) {
			continue // skip old jobs
		}

		log.Info("Waiting for delete hook Job to complete", "addon", addonName, "job", job.Name, "timeout", timeout)

		deadline := time.Now().Add(time.Duration(timeout) * time.Second)
		for time.Now().Before(deadline) {
			if err := c.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: namespace}, &job); err != nil {
				hookErr = fmt.Errorf("get Job %s: %w", job.Name, err)
				break
			}
			if job.Status.Succeeded > 0 {
				log.Info("Delete hook Job completed", "addon", addonName, "job", job.Name)
				break
			}
			if job.Status.Failed > 0 {
				hookErr = fmt.Errorf("delete hook Job %s failed", job.Name)
				log.Info("Delete hook Job failed", "addon", addonName, "job", job.Name)
				break
			}
			time.Sleep(5 * time.Second)
		}

		if time.Now().After(deadline) {
			hookErr = fmt.Errorf("delete hook Job %s timed out after %ds", job.Name, timeout)
			log.Info("Delete hook Job timed out", "addon", addonName, "job", job.Name, "timeout", timeout)
		}
	}

	return hookErr
}

// parseManifest parses a YAML manifest into unstructured objects.
func parseManifest(data []byte) ([]*unstructured.Unstructured, error) {
	var result []*unstructured.Unstructured
	docs := strings.Split(string(data), "\n---\n")
	for _, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" || doc == "---" {
			continue
		}
		obj := &unstructured.Unstructured{}
		if err := yamlutil.Unmarshal([]byte(doc), &obj.Object); err != nil {
			return nil, fmt.Errorf("unmarshal YAML: %w", err)
		}
		if obj.GetKind() != "" {
			result = append(result, obj)
		}
	}
	return result, nil
}

// loadEmbeddedChart loads a chart from the embedded filesystem.
func loadEmbeddedChart(efs fs.ReadFileFS, chartPath string) (*helmchart.Chart, error) {
	var files []*helmloader.BufferedFile
	err := fs.WalkDir(efs, chartPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := efs.ReadFile(p)
		if err != nil {
			return err
		}
		relPath := strings.TrimPrefix(p, chartPath+"/")
		files = append(files, &helmloader.BufferedFile{Name: relPath, Data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return helmloader.LoadFiles(files)
}

// pullOCIChart pulls a chart from OCI with fallback to cache.
func (a *actuator) pullOCIChart(addon *addonpkg.Addon) ([]byte, error) {
	if a.chartPuller == nil {
		return nil, fmt.Errorf("OCI puller not initialized — cannot pull chart %s", addon.Chart.OCI)
	}

	result, err := a.chartPuller.Pull(context.Background(), addon.Chart.OCI, addon.Chart.Version)
	if err != nil {
		return nil, err
	}
	return result.Archive, nil
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

// maxTemplateOutputBytes is the maximum rendered template size (1MB).
// Prevents memory exhaustion from malicious templates like {{ repeat 999999 "x" }}.
const maxTemplateOutputBytes = 1 << 20

// templateTimeout is the maximum time allowed for template execution.
const templateTimeout = 5 * time.Second

// safeFuncMap returns Sprig's text function map with dangerous functions removed.
// Removed functions that could leak secrets or perform unnecessary crypto operations.
func safeFuncMap() template.FuncMap {
	funcMap := sprig.TxtFuncMap()
	// Remove functions that could leak environment variables or secrets
	delete(funcMap, "env")
	delete(funcMap, "expandenv")
	// Remove crypto functions (unnecessary for values rendering)
	delete(funcMap, "genPrivateKey")
	delete(funcMap, "genCA")
	delete(funcMap, "genSelfSignedCert")
	delete(funcMap, "genSignedCert")
	delete(funcMap, "derivePassword")
	delete(funcMap, "buildCustomCert")
	delete(funcMap, "encryptAES")
	delete(funcMap, "decryptAES")
	return funcMap
}

// templateData is the struct exposed to Go templates. Field names match the
// template variables documented in docs/usage.md.
type templateData struct {
	Region                    string
	SeedName                  string
	ShootName                 string
	ShootNamespace            string
	Project                   string
	ControlNamespace          string
	ProviderType              string
	ClusterRole               string
	ManagedKubernetesProvider string
}

func newTemplateData(meta *shootMetadata) *templateData {
	return &templateData{
		Region:                    meta.Region,
		SeedName:                  meta.SeedName,
		ShootName:                 meta.Name,
		ShootNamespace:            meta.Namespace,
		Project:                   meta.Project,
		ControlNamespace:          meta.ControlNamespace,
		ProviderType:              meta.ProviderType,
		ClusterRole:               meta.ClusterRole,
		ManagedKubernetesProvider: meta.ManagedKubernetesProvider,
	}
}

// expandShootValues processes a ShootValues map, expanding Go template expressions
// with shoot-specific values. Supports full Go template syntax including
// conditionals, Sprig string functions, and pipelines.
//
// Example template expressions:
//
//	{{ .Region }}                                          simple variable
//	{{- if eq .ClusterRole "runtime" }}GKE{{- else }}K8s{{- end }}  conditional
//	{{ .ClusterRole | replace "managed-" "" }}             Sprig function
//
// If a string does not contain template syntax ({{ }}), it is returned unchanged
// (fast path, no template parsing overhead).
func expandShootValues(vals map[string]interface{}, meta *shootMetadata) map[string]interface{} {
	data := newTemplateData(meta)
	result := make(map[string]interface{}, len(vals))
	for k, v := range vals {
		result[k] = expandValue(v, data)
	}
	return result
}

// expandValue recursively expands Go template expressions in a single value.
// Returns the original value unchanged if template parsing or execution fails
// (passthrough on error — never breaks the reconcile).
func expandValue(v interface{}, data *templateData) interface{} {
	switch val := v.(type) {
	case string:
		// Fast path: skip template parsing if no template syntax present
		if !strings.Contains(val, "{{") {
			return val
		}
		return executeTemplate(val, data)
	case map[string]interface{}:
		result := make(map[string]interface{}, len(val))
		for mk, mv := range val {
			result[mk] = expandValue(mv, data)
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(val))
		for i, item := range val {
			result[i] = expandValue(item, data)
		}
		return result
	default:
		return v
	}
}

// executeTemplate parses and executes a Go template string with Sprig functions.
// Returns the original string if parsing or execution fails (passthrough on error).
// Enforces a timeout and output size limit as safety measures.
func executeTemplate(s string, data *templateData) string {
	tmpl, err := template.New("").Funcs(safeFuncMap()).Parse(s)
	if err != nil {
		return s // passthrough: not a valid template, treat as literal
	}

	// Execute with timeout to prevent infinite loops
	ctx, cancel := context.WithTimeout(context.Background(), templateTimeout)
	defer cancel()

	var buf bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- tmpl.Execute(&buf, data)
	}()

	select {
	case err := <-done:
		if err != nil {
			return s // passthrough: execution failed
		}
	case <-ctx.Done():
		return s // passthrough: timeout
	}

	// Enforce output size limit
	if buf.Len() > maxTemplateOutputBytes {
		return s // passthrough: output too large
	}

	return buf.String()
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
