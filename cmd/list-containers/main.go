// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and contributors
// SPDX-License-Identifier: Apache-2.0

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
		Short: "List running container images in a Kubernetes cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			if showVersion {
				fmt.Println(version)
				return nil
			}
			return run(kubeconfig, showSHA256, verbose, onlyDigest)
		},
	}

	cmd.Flags().BoolVar(&showSHA256, "sha256", false, "Prefer SHA256 digest over tag")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "Show namespace/pod with image")
	cmd.Flags().BoolVar(&onlyDigest, "only-digest", false, "Show only images with sha256 digests")
	cmd.Flags().BoolVar(&showVersion, "version", false, "Show version and exit")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file")

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

	seen := make(map[string]bool)
	for _, pod := range pods.Items {
		for _, cs := range pod.Status.ContainerStatuses {
			display := cs.Image
			if showSHA256 && strings.Contains(cs.ImageID, "@sha256:") {
				display = strings.SplitN(cs.ImageID, "://", 2)[len(strings.SplitN(cs.ImageID, "://", 2))-1]
			}

			if onlyDigest && !strings.Contains(display, "@sha256:") {
				continue
			}

			if verbose {
				display = fmt.Sprintf("%s/%s: %s", pod.Namespace, pod.Name, display)
			}

			if !seen[display] {
				seen[display] = true
				fmt.Println(display)
			}
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
