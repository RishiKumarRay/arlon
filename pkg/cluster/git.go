package cluster

import (
	"arlon.io/arlon/pkg/gitutils"
	"arlon.io/arlon/pkg/log"
	"bytes"
	"context"
	"embed"
	"fmt"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"io"
	"io/fs"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1types "k8s.io/client-go/kubernetes/typed/core/v1"
	"os"
	"path"
	"strings"
	"text/template"
)

//go:embed manifests/*
var content embed.FS

type RepoCreds struct {
	Url string
	Username string
	Password string
}

type inlineBundle struct {
	name string
	data []byte
}

// -----------------------------------------------------------------------------

func DeployToGit(
	kubeClient *kubernetes.Clientset,
	argocdNs string,
	arlonNs string,
	clusterName string,
	repoUrl string,
	repoBranch string,
	basePath string,
	profileName string,
) error {
	log := log.GetLogger()
	corev1 := kubeClient.CoreV1()
	secretsApi := corev1.Secrets(argocdNs)
	opts := metav1.ListOptions{
		LabelSelector: "argocd.argoproj.io/secret-type=repository",
	}
	secrets, err := secretsApi.List(context.Background(), opts)
	if err != nil {
		return fmt.Errorf("failed to list secrets: %s", err)
	}
	var creds *RepoCreds
	for _, repoSecret := range secrets.Items {
		if strings.Compare(repoUrl, string(repoSecret.Data["url"])) == 0 {
			creds = &RepoCreds{
				Url: string(repoSecret.Data["url"]),
				Username: string(repoSecret.Data["username"]),
				Password: string(repoSecret.Data["password"]),
			}
			break
		}
	}
	if creds == nil {
		return fmt.Errorf("did not find argocd repository matching %s (did you register it?)", repoUrl)
	}

	inlineBundles, err := getInlineBundles(profileName, corev1, arlonNs)
	if err != nil {
		return fmt.Errorf("failed to get inline bundles: %s", err)
	}
	tmpDir, err := os.MkdirTemp("", "arlon-")
	branchRef := plumbing.NewBranchReferenceName(repoBranch)
	auth := &http.BasicAuth{
		Username: creds.Username,
		Password: creds.Password,
	}
	repo, err := gogit.PlainCloneContext(context.Background(), tmpDir, false, &gogit.CloneOptions{
		URL:           repoUrl,
		Auth:          auth,
		RemoteName:    gogit.DefaultRemoteName,
		ReferenceName: branchRef,
		SingleBranch:  true,
		NoCheckout: false,
		Progress:   nil,
		Tags:       gogit.NoTags,
		CABundle:   nil,
	})
	if err != nil {
		return fmt.Errorf("failed to clone repository: %s", err)
	}
	mgmtPath := path.Join(basePath, clusterName, "mgmt")
	workloadPath := path.Join(basePath, clusterName, "workload")
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get repo worktree: %s", err)
	}
	err = copyManifests(wt, ".", mgmtPath)
	if err != nil {
		return fmt.Errorf("failed to copy embedded content: %s", err)
	}
	err = copyInlineBundles(wt, clusterName, repoUrl, mgmtPath, workloadPath, inlineBundles)
	if err != nil {
		return fmt.Errorf("failed to copy inline bundles: %s", err)
	}
	changed, err := gitutils.CommitChanges(tmpDir, wt)
	if err != nil {
		return fmt.Errorf("failed to commit changes: %s", err)
	}
	if !changed {
		log.Info("no changed files, skipping commit & push")
		return nil
	}
	err = repo.Push(&gogit.PushOptions{
		RemoteName: gogit.DefaultRemoteName,
		Auth:       auth,
		Progress:   nil,
		CABundle:   nil,
	})
	if err != nil {
		return fmt.Errorf("failed to push to remote repository: %s", err)
	}
	log.Info("succesfully pushed working tree", "tmpDir", tmpDir)
	return nil
}

// -----------------------------------------------------------------------------

func copyManifests(wt *gogit.Worktree, root string, mgmtPath string) error {
	log := log.GetLogger()
	items, err := content.ReadDir(root)
	if err != nil {
		return fmt.Errorf("failed to read embedded directory: %s", err)
	}
	for _, item := range items {
		filePath := path.Join(root, item.Name())
		if item.IsDir() {
			if err := copyManifests(wt, filePath, mgmtPath); err != nil {
				return err
			}
		} else {
			src, err := content.Open(filePath)
			if err != nil {
				return fmt.Errorf("failed to open embedded file %s: %s", filePath, err)
			}
			// remove manifests/ prefix
			components := strings.Split(filePath, "/")
			dstPath := path.Join(components[1:]...)
			dstPath = path.Join(mgmtPath, dstPath)
			dst, err := wt.Filesystem.Create(dstPath)
			if err != nil {
				_ = src.Close()
				return fmt.Errorf("failed to create destination file %s: %s", dstPath, err)
			}
			_, err = io.Copy(dst, src)
			_ = src.Close()
			_ = dst.Close()
			if err != nil {
				return fmt.Errorf("failed to copy embedded file: %s", err)
			}
			log.V(1).Info("copied embedded file", "destination", dstPath)
		}
	}
	return nil
}

// -----------------------------------------------------------------------------

func getInlineBundles(
	profileName string,
	corev1 corev1types.CoreV1Interface,
	arlonNs string,
) (inlineBundles []inlineBundle, err error) {

	log := log.GetLogger()
	if profileName == "" {
		return
	}
	configMapsApi := corev1.ConfigMaps(arlonNs)
	secretsApi := corev1.Secrets(arlonNs)
	profileConfigMap, err := configMapsApi.Get(context.Background(), profileName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get profile configmap: %s", err)
	}
	if profileConfigMap.Labels["arlon-type"] != "profile" {
		return nil, fmt.Errorf("profile configmap does not have expected label")
	}
	bundles := profileConfigMap.Data["bundles"]
	if bundles == "" {
		return nil, fmt.Errorf("profile has no bundles")
	}
	bundleItems := strings.Split(bundles, ",")
	for _, bundleName := range bundleItems {
		secr, err := secretsApi.Get(context.Background(), bundleName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to get bundle secret %s: %s", bundleName, err)
		}
		if secr.Labels["bundle-type"] != "inline" {
			continue
		}
		inlineBundles = append(inlineBundles, inlineBundle{
			name: bundleName,
			data: secr.Data["data"],
		})
		log.V(1).Info("adding inline bundle", "bundleName", bundleName)
	}
	return
}

// -----------------------------------------------------------------------------

const appTmpl = `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: {{.ClusterName}}-{{.BundleName}}
  namespace: {{.AppNamespace}}
spec:
  syncPolicy:
    automated:
      prune: true
  destination:
    name: {{.ClusterName}}
    namespace: {{.DestinationNamespace}}
  project: default
  source:
    repoURL: {{.RepoUrl}}
    path: {{.WorkloadPath}}/{{.BundleName}}
    targetRevision: HEAD
`

type AppSettings struct {
	ClusterName string
	BundleName string
	WorkloadPath string
	AppNamespace string
	DestinationNamespace string
	RepoUrl string
}

func copyInlineBundles(
	wt *gogit.Worktree,
	clusterName string,
	repoUrl string,
	mgmtPath string,
	workloadPath string,
	bundles []inlineBundle,
) error {
	if len(bundles) == 0 {
		return nil
	}
	tmpl, err := template.New("app").Parse(appTmpl)
	if err != nil {
		return fmt.Errorf("failed to create app template: %s", err)
	}
	for _, bundle := range bundles {
		dirPath := path.Join(workloadPath, bundle.name)
		err := wt.Filesystem.MkdirAll(dirPath, fs.ModeDir | 0700)
		if err != nil {
			return fmt.Errorf("failed to create directory in working tree: %s", err)
		}
		bundleFileName := fmt.Sprintf("%s.yaml", bundle.name)
		bundlePath := path.Join(dirPath, bundleFileName)
		dst, err := wt.Filesystem.Create(bundlePath)
		if err != nil {
			return fmt.Errorf("failed to create file in working tree: %s", err)
		}
		if bundle.data == nil {
			return fmt.Errorf("inline bundle %s has no data", bundle.name)
		}
		_, err = io.Copy(dst, bytes.NewReader(bundle.data))
		if err != nil {
			dst.Close()
			return fmt.Errorf("failed to copy inline bundle %s: %s", bundle.name, err)
		}
		dst.Close()
		appPath := path.Join(mgmtPath, "templates", bundleFileName)
		dst, err = wt.Filesystem.Create(appPath)
		if err != nil {
			return fmt.Errorf("failed to create application file %s: %s", appPath, err)
		}
		app := AppSettings{ClusterName: clusterName, BundleName: bundle.name,
			WorkloadPath: workloadPath, AppNamespace: "argocd",
			DestinationNamespace: "default", RepoUrl: repoUrl}
		err = tmpl.Execute(dst, &app)
		if err != nil {
			dst.Close()
			return fmt.Errorf("failed to render application template %s: %s", appPath, err)
		}
		dst.Close()
	}
	return nil
}
