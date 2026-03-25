package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/amendezsap/gardener-extension-shoot-addon-service/pkg/addon"
)

const addonsDir = "charts/embedded/addons"

func main() {
	root := &cobra.Command{
		Use:   "addon-prepare",
		Short: "Build-time tool for managing addon charts",
		Long: `addon-prepare pulls Helm charts from various sources into the addons
directory for embedding into the extension binary via go:embed.

This tool is NOT shipped in the runtime container image.`,
	}

	root.AddCommand(prepareCmd())
	root.AddCommand(pullChartCmd())
	root.AddCommand(validateCmd())
	root.AddCommand(schemaCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// prepare — read manifest, pull all charts
// ---------------------------------------------------------------------------

func prepareCmd() *cobra.Command {
	var manifestPath string
	var verify bool
	var force bool

	cmd := &cobra.Command{
		Use:   "prepare",
		Short: "Pull all charts declared in the addon manifest",
		Long: `Reads manifest.yaml from the addons directory, pulls each chart
from its declared source (OCI, Helm repo, Git, URL, tarball, or path),
and places them in the addons directory for go:embed.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPrepare(manifestPath, verify, force)
		},
	}

	cmd.Flags().StringVar(&manifestPath, "manifest", filepath.Join(addonsDir, "manifest.yaml"), "Path to addon manifest")
	cmd.Flags().BoolVar(&verify, "verify", false, "Verify charts exist without pulling (for CI)")
	cmd.Flags().BoolVar(&force, "force", false, "Re-pull charts even if they already exist locally")

	return cmd
}

func runPrepare(manifestPath string, verify bool, force bool) error {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}

	var manifest addon.AddonManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}

	if manifest.DefaultNamespace == "" {
		return fmt.Errorf("manifest: defaultNamespace is required")
	}

	fmt.Printf("Addon manifest: %d addon(s), defaultNamespace=%s\n", len(manifest.Addons), manifest.DefaultNamespace)

	for _, a := range manifest.Addons {
		if a.Name == "" {
			return fmt.Errorf("addon: name is required")
		}

		chartDir := filepath.Join(addonsDir, a.Name, "chart")

		if verify {
			// Just check the chart exists
			if _, err := os.Stat(filepath.Join(chartDir, "Chart.yaml")); err != nil {
				return fmt.Errorf("addon %q: chart not found at %s (run 'addon-prepare prepare' without --verify first)", a.Name, chartDir)
			}
			fmt.Printf("  [ok] %s: chart exists at %s\n", a.Name, chartDir)
			continue
		}

		// Check if chart already exists locally; skip pull unless --force is set.
		chartExists := false
		if _, err := os.Stat(filepath.Join(chartDir, "Chart.yaml")); err == nil {
			chartExists = true
		}

		src := a.Chart
		switch {
		case src.Path != "":
			// Local path — verify it exists
			fullPath := filepath.Join(addonsDir, src.Path)
			if _, err := os.Stat(filepath.Join(fullPath, "Chart.yaml")); err != nil {
				return fmt.Errorf("addon %q: chart not found at path %s", a.Name, fullPath)
			}
			fmt.Printf("  [ok] %s: using local chart at %s\n", a.Name, fullPath)

		case src.OCI != "":
			if chartExists && !force {
				fmt.Printf("  [skip] %s: chart already exists at %s (use --force to re-pull)\n", a.Name, chartDir)
				continue
			}
			fmt.Printf("  [pull] %s: OCI %s (version %s)\n", a.Name, src.OCI, src.Version)
			if err := pullOCI(src.OCI, src.Version, chartDir); err != nil {
				return fmt.Errorf("addon %q: OCI pull failed: %w", a.Name, err)
			}

		case src.Repo != "":
			if chartExists && !force {
				fmt.Printf("  [skip] %s: chart already exists at %s (use --force to re-pull)\n", a.Name, chartDir)
				continue
			}
			fmt.Printf("  [pull] %s: Helm repo %s chart=%s (version %s)\n", a.Name, src.Repo, src.RepoChart, src.Version)
			if err := pullHelmRepo(src.Repo, src.RepoChart, src.Version, chartDir); err != nil {
				return fmt.Errorf("addon %q: Helm repo pull failed: %w", a.Name, err)
			}

		case src.Git != "":
			if chartExists && !force {
				fmt.Printf("  [skip] %s: chart already exists at %s (use --force to re-pull)\n", a.Name, chartDir)
				continue
			}
			fmt.Printf("  [pull] %s: Git %s path=%s ref=%s\n", a.Name, src.Git, src.GitPath, src.GitRef)
			if err := pullGit(src.Git, src.GitPath, src.GitRef, chartDir); err != nil {
				return fmt.Errorf("addon %q: Git pull failed: %w", a.Name, err)
			}

		case src.URL != "":
			if chartExists && !force {
				fmt.Printf("  [skip] %s: chart already exists at %s (use --force to re-pull)\n", a.Name, chartDir)
				continue
			}
			fmt.Printf("  [pull] %s: URL %s\n", a.Name, src.URL)
			if err := pullURL(src.URL, chartDir); err != nil {
				return fmt.Errorf("addon %q: URL pull failed: %w", a.Name, err)
			}

		case src.TGZ != "":
			if chartExists && !force {
				fmt.Printf("  [skip] %s: chart already exists at %s (use --force to re-extract)\n", a.Name, chartDir)
				continue
			}
			fmt.Printf("  [extract] %s: tarball %s\n", a.Name, src.TGZ)
			if err := extractTGZ(src.TGZ, chartDir); err != nil {
				return fmt.Errorf("addon %q: tarball extract failed: %w", a.Name, err)
			}

		default:
			return fmt.Errorf("addon %q: no chart source specified (set one of: path, oci, repo, git, url, tgz)", a.Name)
		}
	}

	fmt.Println("Done. Charts are ready for go:embed.")
	return nil
}

// ---------------------------------------------------------------------------
// pull-chart — pull a single chart by name
// ---------------------------------------------------------------------------

func pullChartCmd() *cobra.Command {
	var name, oci, repo, chart, git, gitPath, gitRef, url, tgz, path, version string

	cmd := &cobra.Command{
		Use:   "pull-chart",
		Short: "Pull a single chart into the addons directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			chartDir := filepath.Join(addonsDir, name, "chart")
			if err := os.MkdirAll(chartDir, 0755); err != nil {
				return err
			}
			// Also create values dir
			if err := os.MkdirAll(filepath.Join(addonsDir, name, "values"), 0755); err != nil {
				return err
			}

			switch {
			case oci != "":
				return pullOCI(oci, version, chartDir)
			case repo != "":
				return pullHelmRepo(repo, chart, version, chartDir)
			case git != "":
				return pullGit(git, gitPath, gitRef, chartDir)
			case url != "":
				return pullURL(url, chartDir)
			case tgz != "":
				return extractTGZ(tgz, chartDir)
			case path != "":
				return copyDir(path, chartDir)
			default:
				return fmt.Errorf("specify one of: --oci, --repo, --git, --url, --tgz, --path")
			}
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Addon name (required)")
	cmd.Flags().StringVar(&oci, "oci", "", "OCI registry reference (e.g., oci://ghcr.io/fluent/helm-charts/fluent-bit)")
	cmd.Flags().StringVar(&repo, "repo", "", "Helm repository URL")
	cmd.Flags().StringVar(&chart, "chart", "", "Chart name in Helm repository (used with --repo)")
	cmd.Flags().StringVar(&git, "git", "", "Git repository URL")
	cmd.Flags().StringVar(&gitPath, "git-path", "", "Subdirectory in git repo containing the chart")
	cmd.Flags().StringVar(&gitRef, "git-ref", "main", "Git branch, tag, or commit")
	cmd.Flags().StringVar(&url, "url", "", "HTTP URL to chart tarball (.tgz)")
	cmd.Flags().StringVar(&tgz, "tgz", "", "Local path to chart tarball (.tgz)")
	cmd.Flags().StringVar(&path, "path", "", "Local path to chart directory")
	cmd.Flags().StringVar(&version, "version", "", "Chart version (for OCI and Helm repo)")

	return cmd
}

// ---------------------------------------------------------------------------
// validate — validate the manifest and check embedded charts
// ---------------------------------------------------------------------------

func validateCmd() *cobra.Command {
	var manifestPath string

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate the addon manifest and verify charts exist",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPrepare(manifestPath, true, false)
		},
	}

	cmd.Flags().StringVar(&manifestPath, "manifest", filepath.Join(addonsDir, "manifest.yaml"), "Path to addon manifest")

	return cmd
}

// ---------------------------------------------------------------------------
// schema — output JSON Schema for the manifest
// ---------------------------------------------------------------------------

func schemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Output JSON Schema for the addon manifest",
		RunE: func(cmd *cobra.Command, args []string) error {
			schema, err := addon.GenerateJSONSchema()
			if err != nil {
				return err
			}
			fmt.Println(string(schema))
			return nil
		},
	}
}

// ---------------------------------------------------------------------------
// Chart source handlers
// ---------------------------------------------------------------------------

func pullOCI(ref, version, destDir string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp("", "addon-oci-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	args := []string{"pull", ref, "--untar", "--untardir", tmpDir}
	if version != "" {
		args = append(args, "--version", version)
	}

	cmd := exec.Command("helm", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm pull: %w", err)
	}

	return copyFirstSubdir(tmpDir, destDir)
}

func pullHelmRepo(repoURL, chartName, version, destDir string) error {
	if chartName == "" {
		return fmt.Errorf("--chart is required with --repo")
	}

	tmpDir, err := os.MkdirTemp("", "addon-repo-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	repoName := fmt.Sprintf("addon-prepare-%d", os.Getpid())

	// Add repo
	addCmd := exec.Command("helm", "repo", "add", repoName, repoURL, "--force-update")
	addCmd.Stdout = os.Stdout
	addCmd.Stderr = os.Stderr
	if err := addCmd.Run(); err != nil {
		return fmt.Errorf("helm repo add: %w", err)
	}
	defer func() {
		exec.Command("helm", "repo", "remove", repoName).Run()
	}()

	// Pull
	args := []string{"pull", repoName + "/" + chartName, "--untar", "--untardir", tmpDir}
	if version != "" {
		args = append(args, "--version", version)
	}
	pullCmd := exec.Command("helm", args...)
	pullCmd.Stdout = os.Stdout
	pullCmd.Stderr = os.Stderr
	if err := pullCmd.Run(); err != nil {
		return fmt.Errorf("helm pull: %w", err)
	}

	return copyFirstSubdir(tmpDir, destDir)
}

func pullGit(repoURL, subPath, ref, destDir string) error {
	if subPath == "" {
		return fmt.Errorf("--git-path is required with --git")
	}
	if ref == "" {
		ref = "main"
	}

	tmpDir, err := os.MkdirTemp("", "addon-git-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	cloneCmd := exec.Command("git", "clone", "--depth", "1", "--branch", ref, repoURL, filepath.Join(tmpDir, "repo"))
	cloneCmd.Stdout = os.Stdout
	cloneCmd.Stderr = os.Stderr
	if err := cloneCmd.Run(); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}

	srcDir := filepath.Join(tmpDir, "repo", subPath)
	if _, err := os.Stat(filepath.Join(srcDir, "Chart.yaml")); err != nil {
		return fmt.Errorf("no Chart.yaml found at %s in repo", subPath)
	}

	return copyDir(srcDir, destDir)
}

func pullURL(chartURL, destDir string) error {
	tmpDir, err := os.MkdirTemp("", "addon-url-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	tgzPath := filepath.Join(tmpDir, "chart.tgz")

	resp, err := http.Get(chartURL)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	out, err := os.Create(tgzPath)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		return fmt.Errorf("download: %w", err)
	}
	out.Close()

	return extractTGZ(tgzPath, destDir)
}

func extractTGZ(tgzPath, destDir string) error {
	f, err := os.Open(tgzPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	// Extract to temp dir first to find the chart root
	tmpDir, err := os.MkdirTemp("", "addon-tgz-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}

		target := filepath.Join(tmpDir, hdr.Name)

		// Prevent path traversal
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(tmpDir)) {
			return fmt.Errorf("tar: path traversal detected: %s", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			out, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		}
	}

	return copyFirstSubdir(tmpDir, destDir)
}

// ---------------------------------------------------------------------------
// Filesystem helpers
// ---------------------------------------------------------------------------

// copyFirstSubdir copies the first subdirectory of src into dest.
// Helm tarballs extract to chartname/ as the root, so we need to
// copy the contents of that subdirectory.
func copyFirstSubdir(src, dest string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			return copyDir(filepath.Join(src, e.Name()), dest)
		}
	}
	return fmt.Errorf("no subdirectory found in %s", src)
}

// copyDir recursively copies src directory contents into dest.
func copyDir(src, dest string) error {
	// Clean destination
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0755); err != nil {
		return err
	}

	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		destPath := filepath.Join(dest, relPath)

		if info.IsDir() {
			return os.MkdirAll(destPath, 0755)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		return os.WriteFile(destPath, data, info.Mode())
	})
}
