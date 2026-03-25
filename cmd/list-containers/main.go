package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var version = "0.1.0"

func main() {
	var showSHA256, verbose, onlyDigest, showVersion bool
	var kubeconfig string

	cmd := &cobra.Command{
		Use:   "list-containers",
		Short: "List running containers in a Kubernetes cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			if showVersion {
				fmt.Println(version)
				return nil
			}
			return run(kubeconfig, showSHA256, verbose, onlyDigest)
		},
	}

	cmd.Flags().BoolVar(&showSHA256, "sha256", false, "Display container images as SHA256 values")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "Show namespace/pod_name with image path")
	cmd.Flags().BoolVar(&onlyDigest, "only-digest", false, "Show only images with sha256 digests")
	cmd.Flags().BoolVar(&showVersion, "version", false, "Show version and exit")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file (optional)")

	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(kubeconfig string, showSHA256, verbose, onlyDigest bool) error {
	cfg, err := buildConfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("build config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}

	pods, err := clientset.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}

	for _, pod := range pods.Items {
		if pod.Status.ContainerStatuses == nil {
			continue
		}
		for _, cs := range pod.Status.ContainerStatuses {
			var imageDisplay string

			if showSHA256 {
				// Use image_id if it contains @sha256: (matches Python behavior)
				if cs.ImageID != "" && strings.Contains(cs.ImageID, "@sha256:") {
					imageDisplay = cs.ImageID
				} else {
					imageDisplay = cs.Image // fallback
				}
			} else {
				imageDisplay = cs.Image
			}

			var line string
			if verbose {
				line = fmt.Sprintf("%s/%s: %s", pod.Namespace, pod.Name, imageDisplay)
			} else {
				line = imageDisplay
			}

			if onlyDigest && !strings.Contains(line, "@sha256:") {
				continue
			}

			fmt.Println(line)
		}
	}

	return nil
}

func buildConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}
