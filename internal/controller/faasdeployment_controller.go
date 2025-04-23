/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http" // go-git에서 HTTPS 인증

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	automationv1alpha1 "github.com/faasplatform/event-driven-operator/api/v1alpha1"
)

// FaaSDeploymentReconciler reconciles a FaaSDeployment object
type FaaSDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=automation.faasplatform,resources=faasdeployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=automation.faasplatform,resources=faasdeployments/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=automation.faasplatform,resources=faasdeployments/finalizers,verbs=update

var templateRepoTable = map[string]string{
	"video-upload-processing": "https://github.com/hyeongwooo/Blueprint",
}

func (r *FaaSDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cr := &automationv1alpha1.FaaSDeployment{}
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		logger.Error(err, "❌ Failed to get FaaSDeployment CR")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	templateURL, ok := templateRepoTable[cr.Spec.Template]
	if !ok {
		logger.Error(nil, "❌ Template not found", "template", cr.Spec.Template)
		return ctrl.Result{}, nil
	}

	// Paths
	templatePath := filepath.Join("/tmp/templates", cr.Name)
	renderedPath := filepath.Join("/tmp/rendered", cr.Name)

	if err := cloneRepo(templateURL, "main", templatePath); err != nil {
		logger.Error(err, "❌ Failed to clone template repo")
		return ctrl.Result{}, err
	}

	values := map[string]interface{}{
		"User":       cr.Spec.User,
		"Event":      cr.Spec.Event,
		"Service":    cr.Spec.Service,
		"EventLogic": cr.Spec.EventLogic,
		"Template":   cr.Spec.Template,
	}

	files := []string{"sensor.yaml", "eventsource.yaml", "workflow.yaml", "knative.yaml"}
	if err := renderTemplates(templatePath, renderedPath, files, values); err != nil {
		logger.Error(err, "❌ Failed to render templates")
		return ctrl.Result{}, err
	}

	var userRepoTable = map[string]string{
		"user1": "https://github.com/hyeongwooo/user-1-service",
	}

	repoURL, ok := userRepoTable[cr.Spec.User]
	if !ok {
		logger.Error(nil, "❌ No Git repo mapping found for user", "user", cr.Spec.User)
		return ctrl.Result{}, nil
	}

	if err := commitToGitRepo(repoURL, renderedPath, cr.Name); err != nil {
		logger.Error(err, "❌ Failed to commit to Git repository")
		return ctrl.Result{}, err
	}

	logger.Info("✅ Successfully handled FaaSDeployment", "user", cr.Spec.User)
	return ctrl.Result{}, nil
}

func cloneRepo(url, branch, path string) error {
	_ = os.RemoveAll(path)
	_, err := git.PlainClone(path, false, &git.CloneOptions{
		URL:           url,
		ReferenceName: plumbing.NewBranchReferenceName(branch),
		SingleBranch:  true,
		Depth:         1,
	})
	return err
}

func renderTemplates(srcPath, dstPath string, filenames []string, values map[string]interface{}) error {
	if err := os.MkdirAll(dstPath, 0755); err != nil {
		return err
	}
	for _, filename := range filenames {
		tmplPath := filepath.Join(srcPath, filename)
		tmpl, err := template.ParseFiles(tmplPath)
		if err != nil {
			return err
		}

		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, values); err != nil {
			return err
		}

		dstFile := filepath.Join(dstPath, filename)
		if err := os.WriteFile(dstFile, buf.Bytes(), 0644); err != nil {
			return err
		}
	}
	return nil
}

func commitToGitRepo(repoURL, filesPath, commitMsg string) error {
	tmpPath := filepath.Join("/tmp/git-push", commitMsg)
	_ = os.RemoveAll(tmpPath)

	repo, err := git.PlainClone(tmpPath, false, &git.CloneOptions{
		URL:      repoURL,
		Progress: os.Stdout,
		Auth: &githttp.BasicAuth{
			Username: "git", // or anything except empty string
			Password: os.Getenv("GIT_TOKEN"),
		},
	})
	if err != nil {
		return err
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return err
	}

	files, _ := os.ReadDir(filesPath)
	for _, f := range files {
		src := filepath.Join(filesPath, f.Name())
		dst := filepath.Join(tmpPath, f.Name())
		data, _ := os.ReadFile(src)
		_ = os.WriteFile(dst, data, 0644)
		_, _ = worktree.Add(f.Name())
	}

	_, err = worktree.Commit(fmt.Sprintf("Deploy: %s", commitMsg), &git.CommitOptions{
		Author: &object.Signature{
			Name:  "FaaS Operator",
			Email: "faas@platform.io",
			When:  time.Now(),
		},
	})
	if err != nil {
		return err
	}

	return repo.Push(&git.PushOptions{
		Progress: os.Stdout,
		Auth: &githttp.BasicAuth{
			Username: "git",
			Password: os.Getenv("GIT_TOKEN"),
		},
	})
}

func (r *FaaSDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&automationv1alpha1.FaaSDeployment{}).
		Complete(r)
}
