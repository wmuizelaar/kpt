// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package engine

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"unicode"

	"github.com/GoogleContainerTools/kpt/internal/builtins"
	kptfile "github.com/GoogleContainerTools/kpt/pkg/api/kptfile/v1"
	"github.com/GoogleContainerTools/kpt/pkg/fn"
	api "github.com/GoogleContainerTools/kpt/porch/api/porch/v1alpha1"
	configapi "github.com/GoogleContainerTools/kpt/porch/api/porchconfig/v1alpha1"
	"github.com/GoogleContainerTools/kpt/porch/pkg/cache"
	"github.com/GoogleContainerTools/kpt/porch/pkg/kpt"
	"github.com/GoogleContainerTools/kpt/porch/pkg/meta"
	"github.com/GoogleContainerTools/kpt/porch/pkg/repository"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/kustomize/kyaml/comments"
	"sigs.k8s.io/kustomize/kyaml/kio"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

var tracer = otel.Tracer("engine")

type CaDEngine interface {
	// ObjectCache() is a cache of all our objects.
	ObjectCache() cache.ObjectCache

	UpdatePackageResources(ctx context.Context, repositoryObj *configapi.Repository, oldPackage *PackageRevision, old, new *api.PackageRevisionResources) (*PackageRevision, error)
	ListFunctions(ctx context.Context, repositoryObj *configapi.Repository) ([]*Function, error)

	ListPackageRevisions(ctx context.Context, repositorySpec *configapi.Repository, filter repository.ListPackageRevisionFilter) ([]*PackageRevision, error)
	CreatePackageRevision(ctx context.Context, repositoryObj *configapi.Repository, obj *api.PackageRevision, parent *PackageRevision) (*PackageRevision, error)
	UpdatePackageRevision(ctx context.Context, repositoryObj *configapi.Repository, oldPackage *PackageRevision, old, new *api.PackageRevision, parent *PackageRevision) (*PackageRevision, error)
	DeletePackageRevision(ctx context.Context, repositoryObj *configapi.Repository, obj *PackageRevision) error

	ListPackages(ctx context.Context, repositorySpec *configapi.Repository, filter repository.ListPackageFilter) ([]*Package, error)
	CreatePackage(ctx context.Context, repositoryObj *configapi.Repository, obj *api.Package) (*Package, error)
	UpdatePackage(ctx context.Context, repositoryObj *configapi.Repository, oldPackage *Package, old, new *api.Package) (*Package, error)
	DeletePackage(ctx context.Context, repositoryObj *configapi.Repository, obj *Package) error
}

type Package struct {
	repoPackage repository.Package
}

func (p *Package) GetPackage() *api.Package {
	return p.repoPackage.GetPackage()
}

func (p *Package) KubeObjectName() string {
	return p.repoPackage.KubeObjectName()
}

type PackageRevision struct {
	repoPackageRevision repository.PackageRevision
	packageRevisionMeta meta.PackageRevisionMeta
}

func (p *PackageRevision) GetPackageRevision(ctx context.Context) (*api.PackageRevision, error) {
	repoPkgRev, err := p.repoPackageRevision.GetPackageRevision(ctx)
	if err != nil {
		return nil, err
	}
	var isLatest bool
	if val, found := repoPkgRev.Labels[api.LatestPackageRevisionKey]; found && val == api.LatestPackageRevisionValue {
		isLatest = true
	}
	repoPkgRev.Labels = p.packageRevisionMeta.Labels
	if isLatest {
		if repoPkgRev.Labels == nil {
			repoPkgRev.Labels = make(map[string]string)
		}
		repoPkgRev.Labels[api.LatestPackageRevisionKey] = api.LatestPackageRevisionValue
	}
	repoPkgRev.Annotations = p.packageRevisionMeta.Annotations
	return repoPkgRev, nil
}

func (p *PackageRevision) KubeObjectName() string {
	return p.repoPackageRevision.KubeObjectName()
}

func (p *PackageRevision) GetResources(ctx context.Context) (*api.PackageRevisionResources, error) {
	return p.repoPackageRevision.GetResources(ctx)
}

type Function struct {
	RepoFunction repository.Function
}

func (f *Function) Name() string {
	return f.RepoFunction.Name()
}

func (f *Function) GetFunction() (*api.Function, error) {
	return f.RepoFunction.GetFunction()
}

func NewCaDEngine(opts ...EngineOption) (CaDEngine, error) {
	engine := &cadEngine{}
	for _, opt := range opts {
		if err := opt.apply(engine); err != nil {
			return nil, err
		}
	}
	return engine, nil
}

type cadEngine struct {
	cache              *cache.Cache
	renderer           fn.Renderer
	runtime            fn.FunctionRuntime
	credentialResolver repository.CredentialResolver
	referenceResolver  ReferenceResolver
	userInfoProvider   repository.UserInfoProvider
	metadataStore      meta.MetadataStore
}

var _ CaDEngine = &cadEngine{}

type mutation interface {
	Apply(ctx context.Context, resources repository.PackageResources) (repository.PackageResources, *api.Task, error)
}

// ObjectCache is a cache of all our objects.
func (cad *cadEngine) ObjectCache() cache.ObjectCache {
	return cad.cache.ObjectCache()
}

func (cad *cadEngine) OpenRepository(ctx context.Context, repositorySpec *configapi.Repository) (repository.Repository, error) {
	ctx, span := tracer.Start(ctx, "cadEngine::OpenRepository", trace.WithAttributes())
	defer span.End()

	return cad.cache.OpenRepository(ctx, repositorySpec)
}

func (cad *cadEngine) ListPackageRevisions(ctx context.Context, repositorySpec *configapi.Repository, filter repository.ListPackageRevisionFilter) ([]*PackageRevision, error) {
	ctx, span := tracer.Start(ctx, "cadEngine::ListPackageRevisions", trace.WithAttributes())
	defer span.End()

	repo, err := cad.cache.OpenRepository(ctx, repositorySpec)
	if err != nil {
		return nil, err
	}
	pkgRevs, err := repo.ListPackageRevisions(ctx, filter)
	if err != nil {
		return nil, err
	}

	var packageRevisions []*PackageRevision
	for _, pr := range pkgRevs {
		pkgRevMeta, err := cad.metadataStore.Get(ctx, types.NamespacedName{
			Name:      pr.KubeObjectName(),
			Namespace: pr.KubeObjectNamespace(),
		})
		if err != nil {
			// If a PackageRev CR doesn't exist, we treat the
			// Packagerevision as not existing.
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		packageRevisions = append(packageRevisions, &PackageRevision{
			repoPackageRevision: pr,
			packageRevisionMeta: pkgRevMeta,
		})
	}
	return packageRevisions, nil
}

func buildPackageConfig(ctx context.Context, obj *api.PackageRevision, parent *PackageRevision) (*builtins.PackageConfig, error) {
	config := &builtins.PackageConfig{}

	parentPath := ""

	var parentConfig *unstructured.Unstructured
	if parent != nil {
		parentObj, err := parent.GetPackageRevision(ctx)
		if err != nil {
			return nil, err
		}
		parentPath = parentObj.Spec.PackageName

		resources, err := parent.GetResources(ctx)
		if err != nil {
			return nil, fmt.Errorf("error getting resources from parent package %q: %w", parentObj.Name, err)
		}
		configMapObj, err := ExtractContextConfigMap(resources.Spec.Resources)
		if err != nil {
			return nil, fmt.Errorf("error getting configuration from parent package %q: %w", parentObj.Name, err)
		}
		parentConfig = configMapObj

		if parentConfig != nil {
			// TODO: Should we support kinds other than configmaps?
			var parentConfigMap corev1.ConfigMap
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(parentConfig.Object, &parentConfigMap); err != nil {
				return nil, fmt.Errorf("error parsing ConfigMap from parent configuration: %w", err)
			}
			if s := parentConfigMap.Data[builtins.ConfigKeyPackagePath]; s != "" {
				parentPath = s + "/" + parentPath
			}
		}
	}

	if parentPath == "" {
		config.PackagePath = obj.Spec.PackageName
	} else {
		config.PackagePath = parentPath + "/" + obj.Spec.PackageName
	}

	return config, nil
}

func (cad *cadEngine) CreatePackageRevision(ctx context.Context, repositoryObj *configapi.Repository, obj *api.PackageRevision, parent *PackageRevision) (*PackageRevision, error) {
	ctx, span := tracer.Start(ctx, "cadEngine::CreatePackageRevision", trace.WithAttributes())
	defer span.End()

	packageConfig, err := buildPackageConfig(ctx, obj, parent)
	if err != nil {
		return nil, err
	}

	// Validate package lifecycle. Cannot create a final package
	switch obj.Spec.Lifecycle {
	case "":
		// Set draft as default
		obj.Spec.Lifecycle = api.PackageRevisionLifecycleDraft
	case api.PackageRevisionLifecycleDraft, api.PackageRevisionLifecycleProposed:
		// These values are ok
	case api.PackageRevisionLifecyclePublished:
		// TODO: generate errors that can be translated to correct HTTP responses
		return nil, fmt.Errorf("cannot create a package revision with lifecycle value 'Final'")
	default:
		return nil, fmt.Errorf("unsupported lifecycle value: %s", obj.Spec.Lifecycle)
	}

	repo, err := cad.cache.OpenRepository(ctx, repositoryObj)
	if err != nil {
		return nil, err
	}
	draft, err := repo.CreatePackageRevision(ctx, obj)
	if err != nil {
		return nil, err
	}

	if err := cad.applyTasks(ctx, draft, repositoryObj, obj, packageConfig); err != nil {
		return nil, err
	}

	if err := draft.UpdateLifecycle(ctx, obj.Spec.Lifecycle); err != nil {
		return nil, err
	}

	// Updates are done.
	repoPkgRev, err := draft.Close(ctx)
	if err != nil {
		return nil, err
	}
	pkgRevMeta := meta.PackageRevisionMeta{
		Name:        repoPkgRev.KubeObjectName(),
		Namespace:   repoPkgRev.KubeObjectNamespace(),
		Labels:      obj.Labels,
		Annotations: obj.Annotations,
	}
	pkgRevMeta, err = cad.metadataStore.Create(ctx, pkgRevMeta, repositoryObj)
	if err != nil {
		return nil, err
	}
	return &PackageRevision{
		repoPackageRevision: repoPkgRev,
		packageRevisionMeta: pkgRevMeta,
	}, nil
}

func (cad *cadEngine) applyTasks(ctx context.Context, draft repository.PackageDraft, repositoryObj *configapi.Repository, obj *api.PackageRevision, packageConfig *builtins.PackageConfig) error {
	var mutations []mutation

	// Unless first task is Init or Clone, insert Init to create an empty package.
	tasks := obj.Spec.Tasks
	if len(tasks) == 0 || (tasks[0].Type != api.TaskTypeInit && tasks[0].Type != api.TaskTypeClone) {
		mutations = append(mutations, &initPackageMutation{
			name: obj.Spec.PackageName,
			task: &api.Task{
				Init: &api.PackageInitTaskSpec{
					Subpackage:  "",
					Description: fmt.Sprintf("%s description", obj.Spec.PackageName),
				},
			},
		})
	}

	for i := range tasks {
		task := &tasks[i]
		mutation, err := cad.mapTaskToMutation(ctx, obj, task, repositoryObj.Spec.Deployment, packageConfig)
		if err != nil {
			return err
		}
		mutations = append(mutations, mutation)
	}

	// Render package after creation.
	mutations = cad.conditionalAddRender(mutations)

	baseResources := repository.PackageResources{}
	if err := applyResourceMutations(ctx, draft, baseResources, mutations); err != nil {
		return err
	}

	return nil
}

type RepositoryOpener interface {
	OpenRepository(ctx context.Context, repositorySpec *configapi.Repository) (repository.Repository, error)
}

func (cad *cadEngine) mapTaskToMutation(ctx context.Context, obj *api.PackageRevision, task *api.Task, isDeployment bool, packageConfig *builtins.PackageConfig) (mutation, error) {
	switch task.Type {
	case api.TaskTypeInit:
		if task.Init == nil {
			return nil, fmt.Errorf("init not set for task of type %q", task.Type)
		}
		return &initPackageMutation{
			name: obj.Spec.PackageName,
			task: task,
		}, nil
	case api.TaskTypeClone:
		if task.Clone == nil {
			return nil, fmt.Errorf("clone not set for task of type %q", task.Type)
		}
		return &clonePackageMutation{
			task:               task,
			namespace:          obj.Namespace,
			name:               obj.Spec.PackageName,
			isDeployment:       isDeployment,
			repoOpener:         cad,
			credentialResolver: cad.credentialResolver,
			referenceResolver:  cad.referenceResolver,
			packageConfig:      packageConfig,
		}, nil

	case api.TaskTypeUpdate:
		if task.Update == nil {
			return nil, fmt.Errorf("update not set for task of type %q", task.Type)
		}
		cloneTask := findCloneTask(obj)
		if cloneTask == nil {
			return nil, fmt.Errorf("upstream source not found for package rev %q; only cloned packages can be updated", obj.Spec.PackageName)
		}
		return &updatePackageMutation{
			cloneTask:         cloneTask,
			updateTask:        task,
			namespace:         obj.Namespace,
			repoOpener:        cad,
			referenceResolver: cad.referenceResolver,
			pkgName:           obj.Spec.PackageName,
		}, nil

	case api.TaskTypePatch:
		return buildPatchMutation(ctx, task)

	case api.TaskTypeEdit:
		if task.Edit == nil {
			return nil, fmt.Errorf("edit not set for task of type %q", task.Type)
		}
		return &editPackageMutation{
			task:              task,
			namespace:         obj.Namespace,
			repoOpener:        cad,
			referenceResolver: cad.referenceResolver,
		}, nil

	case api.TaskTypeEval:
		if task.Eval == nil {
			return nil, fmt.Errorf("eval not set for task of type %q", task.Type)
		}
		// TODO: We should find a different way to do this. Probably a separate
		// task for render.
		if task.Eval.Image == "render" {
			return &renderPackageMutation{
				renderer: cad.renderer,
				runtime:  cad.runtime,
			}, nil
		} else {
			return &evalFunctionMutation{
				runtime: cad.runtime,
				task:    task,
			}, nil
		}

	default:
		return nil, fmt.Errorf("task of type %q not supported", task.Type)
	}
}

func (cad *cadEngine) UpdatePackageRevision(ctx context.Context, repositoryObj *configapi.Repository, oldPackage *PackageRevision, oldObj, newObj *api.PackageRevision, parent *PackageRevision) (*PackageRevision, error) {
	ctx, span := tracer.Start(ctx, "cadEngine::UpdatePackageRevision", trace.WithAttributes())
	defer span.End()

	repo, err := cad.cache.OpenRepository(ctx, repositoryObj)
	if err != nil {
		return nil, err
	}

	// Validate package lifecycle. Can only update a draft.
	switch lifecycle := oldObj.Spec.Lifecycle; lifecycle {
	default:
		return nil, fmt.Errorf("invalid original lifecycle value: %q", lifecycle)
	case api.PackageRevisionLifecycleDraft, api.PackageRevisionLifecycleProposed:
		// Draft or proposed can be updated.
	case api.PackageRevisionLifecyclePublished:
		// Only metadata (currently labels and annotations) can be updated for published packages.
		repoPkgRev := oldPackage.repoPackageRevision

		pkgRevMeta := meta.PackageRevisionMeta{
			Name:        repoPkgRev.KubeObjectName(),
			Namespace:   repoPkgRev.KubeObjectNamespace(),
			Labels:      newObj.Labels,
			Annotations: newObj.Annotations,
		}
		cad.metadataStore.Update(ctx, pkgRevMeta)

		return &PackageRevision{
			repoPackageRevision: repoPkgRev,
			packageRevisionMeta: pkgRevMeta,
		}, nil
	}
	switch lifecycle := newObj.Spec.Lifecycle; lifecycle {
	default:
		return nil, fmt.Errorf("invalid desired lifecycle value: %q", lifecycle)
	case api.PackageRevisionLifecycleDraft, api.PackageRevisionLifecycleProposed, api.PackageRevisionLifecyclePublished:
		// These values are ok
	}

	if isRecloneAndReplay(oldObj, newObj) {
		packageConfig, err := buildPackageConfig(ctx, newObj, parent)
		if err != nil {
			return nil, err
		}
		repoPkgRev, err := cad.recloneAndReplay(ctx, repo, repositoryObj, newObj, packageConfig)
		if err != nil {
			return nil, err
		}
		return &PackageRevision{
			repoPackageRevision: repoPkgRev,
		}, nil
	}

	var mutations []mutation
	if len(oldObj.Spec.Tasks) > len(newObj.Spec.Tasks) {
		return nil, fmt.Errorf("removing tasks is not yet supported")
	}
	for i := range oldObj.Spec.Tasks {
		oldTask := &oldObj.Spec.Tasks[i]
		newTask := &newObj.Spec.Tasks[i]
		if oldTask.Type != newTask.Type {
			return nil, fmt.Errorf("changing task types is not yet supported")
		}
	}
	if len(newObj.Spec.Tasks) > len(oldObj.Spec.Tasks) {
		if len(newObj.Spec.Tasks) > len(oldObj.Spec.Tasks)+1 {
			return nil, fmt.Errorf("can only append one task at a time")
		}

		newTask := newObj.Spec.Tasks[len(newObj.Spec.Tasks)-1]
		if newTask.Type != api.TaskTypeUpdate {
			return nil, fmt.Errorf("appended task is type %q, must be type %q", newTask.Type, api.TaskTypeUpdate)
		}
		if newTask.Update == nil {
			return nil, fmt.Errorf("update not set for updateTask of type %q", newTask.Type)
		}

		cloneTask := findCloneTask(oldObj)
		if cloneTask == nil {
			return nil, fmt.Errorf("upstream source not found for package rev %q; only cloned packages can be updated", oldObj.Spec.PackageName)
		}

		mutation := &updatePackageMutation{
			cloneTask:         cloneTask,
			updateTask:        &newTask,
			repoOpener:        cad,
			referenceResolver: cad.referenceResolver,
			namespace:         repositoryObj.Namespace,
			pkgName:           oldObj.GetName(),
		}
		mutations = append(mutations, mutation)
	}

	// Re-render if we are making changes.
	mutations = cad.conditionalAddRender(mutations)

	draft, err := repo.UpdatePackageRevision(ctx, oldPackage.repoPackageRevision)
	if err != nil {
		return nil, err
	}

	// If any of the fields in the API that are projections from the Kptfile
	// must be updated in the Kptfile as well.
	kfPatchTask, created, err := createKptfilePatchTask(ctx, oldPackage.repoPackageRevision, newObj)
	if err != nil {
		return nil, err
	}
	if created {
		kfPatchMutation, err := buildPatchMutation(ctx, kfPatchTask)
		if err != nil {
			return nil, err
		}
		mutations = append(mutations, kfPatchMutation)
	}

	// Re-render if we are making changes.
	mutations = cad.conditionalAddRender(mutations)

	// TODO: Handle the case if alongside lifecycle change, tasks are changed too.
	// Update package contents only if the package is in draft state
	if oldObj.Spec.Lifecycle == api.PackageRevisionLifecycleDraft {
		apiResources, err := oldPackage.GetResources(ctx)
		if err != nil {
			return nil, fmt.Errorf("cannot get package resources: %w", err)
		}
		resources := repository.PackageResources{
			Contents: apiResources.Spec.Resources,
		}

		if err := applyResourceMutations(ctx, draft, resources, mutations); err != nil {
			return nil, err
		}
	}

	if err := draft.UpdateLifecycle(ctx, newObj.Spec.Lifecycle); err != nil {
		return nil, err
	}

	// Updates are done.
	repoPkgRev, err := draft.Close(ctx)
	if err != nil {
		return nil, err
	}

	pkgRevMeta := meta.PackageRevisionMeta{
		Name:        repoPkgRev.KubeObjectName(),
		Namespace:   repoPkgRev.KubeObjectNamespace(),
		Labels:      newObj.Labels,
		Annotations: newObj.Annotations,
	}
	cad.metadataStore.Update(ctx, pkgRevMeta)

	return &PackageRevision{
		repoPackageRevision: repoPkgRev,
		packageRevisionMeta: pkgRevMeta,
	}, nil
}

func createKptfilePatchTask(ctx context.Context, oldPackage repository.PackageRevision, newObj *api.PackageRevision) (*api.Task, bool, error) {
	kf, err := oldPackage.GetKptfile(ctx)
	if err != nil {
		return nil, false, err
	}

	var orgKfString string
	{
		var buf bytes.Buffer
		d := yaml.NewEncoder(&buf)
		if err := d.Encode(kf); err != nil {
			return nil, false, err
		}
		orgKfString = buf.String()
	}

	var readinessGates []kptfile.ReadinessGate
	for _, rg := range newObj.Spec.ReadinessGates {
		readinessGates = append(readinessGates, kptfile.ReadinessGate{
			ConditionType: rg.ConditionType,
		})
	}

	var conditions []kptfile.Condition
	for _, c := range newObj.Status.Conditions {
		conditions = append(conditions, kptfile.Condition{
			Type:    c.Type,
			Status:  convertStatusToKptfile(c.Status),
			Reason:  c.Reason,
			Message: c.Message,
		})
	}

	if kf.Info == nil && len(readinessGates) > 0 {
		kf.Info = &kptfile.PackageInfo{}
	}
	if len(readinessGates) > 0 {
		kf.Info.ReadinessGates = readinessGates
	}

	if kf.Status == nil && len(conditions) > 0 {
		kf.Status = &kptfile.Status{}
	}
	if len(conditions) > 0 {
		kf.Status.Conditions = conditions
	}

	var newKfString string
	{
		var buf bytes.Buffer
		d := yaml.NewEncoder(&buf)
		if err := d.Encode(kf); err != nil {
			return nil, false, err
		}
		newKfString = buf.String()
	}

	patchSpec, err := GeneratePatch(kptfile.KptFileName, orgKfString, newKfString)
	if err != nil {
		return nil, false, err
	}
	// If patch is empty, don't create a Task.
	if patchSpec.Contents == "" {
		return nil, false, nil
	}

	return &api.Task{
		Type: api.TaskTypePatch,
		Patch: &api.PackagePatchTaskSpec{
			Patches: []api.PatchSpec{
				patchSpec,
			},
		},
	}, true, nil
}

func convertStatusToKptfile(s api.ConditionStatus) kptfile.ConditionStatus {
	switch s {
	case api.ConditionTrue:
		return kptfile.ConditionTrue
	case api.ConditionFalse:
		return kptfile.ConditionFalse
	case api.ConditionUnknown:
		return kptfile.ConditionUnknown
	default:
		panic(fmt.Errorf("unknown condition status: %v", s))
	}
}

// conditionalAddRender adds a render mutation to the end of the mutations slice if the last
// entry is not already a render mutation.
func (cad *cadEngine) conditionalAddRender(mutations []mutation) []mutation {
	if len(mutations) == 0 {
		return mutations
	}

	lastMutation := mutations[len(mutations)-1]
	_, isRender := lastMutation.(*renderPackageMutation)
	if isRender {
		return mutations
	}

	return append(mutations, &renderPackageMutation{
		renderer: cad.renderer,
		runtime:  cad.runtime,
	})
}

func (cad *cadEngine) DeletePackageRevision(ctx context.Context, repositoryObj *configapi.Repository, oldPackage *PackageRevision) error {
	ctx, span := tracer.Start(ctx, "cadEngine::DeletePackageRevision", trace.WithAttributes())
	defer span.End()

	repo, err := cad.cache.OpenRepository(ctx, repositoryObj)
	if err != nil {
		return err
	}

	if err := repo.DeletePackageRevision(ctx, oldPackage.repoPackageRevision); err != nil {
		return err
	}

	namespacedName := types.NamespacedName{
		Name:      oldPackage.repoPackageRevision.KubeObjectName(),
		Namespace: oldPackage.repoPackageRevision.KubeObjectNamespace(),
	}
	if _, err := cad.metadataStore.Delete(ctx, namespacedName); err != nil {
		return err
	}

	return nil
}

func (cad *cadEngine) ListPackages(ctx context.Context, repositorySpec *configapi.Repository, filter repository.ListPackageFilter) ([]*Package, error) {
	ctx, span := tracer.Start(ctx, "cadEngine::ListPackages", trace.WithAttributes())
	defer span.End()

	repo, err := cad.cache.OpenRepository(ctx, repositorySpec)
	if err != nil {
		return nil, err
	}

	pkgs, err := repo.ListPackages(ctx, filter)
	if err != nil {
		return nil, err
	}
	var packages []*Package
	for _, p := range pkgs {
		packages = append(packages, &Package{
			repoPackage: p,
		})
	}

	return packages, nil
}

func (cad *cadEngine) CreatePackage(ctx context.Context, repositoryObj *configapi.Repository, obj *api.Package) (*Package, error) {
	ctx, span := tracer.Start(ctx, "cadEngine::CreatePackage", trace.WithAttributes())
	defer span.End()

	repo, err := cad.cache.OpenRepository(ctx, repositoryObj)
	if err != nil {
		return nil, err
	}
	pkg, err := repo.CreatePackage(ctx, obj)
	if err != nil {
		return nil, err
	}

	return &Package{
		repoPackage: pkg,
	}, nil
}

func (cad *cadEngine) UpdatePackage(ctx context.Context, repositoryObj *configapi.Repository, oldPackage *Package, oldObj, newObj *api.Package) (*Package, error) {
	ctx, span := tracer.Start(ctx, "cadEngine::UpdatePackage", trace.WithAttributes())
	defer span.End()

	// TODO
	var pkg *Package
	return pkg, fmt.Errorf("Updating packages is not yet supported")
}

func (cad *cadEngine) DeletePackage(ctx context.Context, repositoryObj *configapi.Repository, oldPackage *Package) error {
	ctx, span := tracer.Start(ctx, "cadEngine::DeletePackage", trace.WithAttributes())
	defer span.End()

	repo, err := cad.cache.OpenRepository(ctx, repositoryObj)
	if err != nil {
		return err
	}

	if err := repo.DeletePackage(ctx, oldPackage.repoPackage); err != nil {
		return err
	}

	return nil
}

func (cad *cadEngine) UpdatePackageResources(ctx context.Context, repositoryObj *configapi.Repository, oldPackage *PackageRevision, old, new *api.PackageRevisionResources) (*PackageRevision, error) {
	ctx, span := tracer.Start(ctx, "cadEngine::UpdatePackageResources", trace.WithAttributes())
	defer span.End()

	rev, err := oldPackage.repoPackageRevision.GetPackageRevision(ctx)
	if err != nil {
		return nil, err
	}

	// Validate package lifecycle. Can only update a draft.
	switch lifecycle := rev.Spec.Lifecycle; lifecycle {
	default:
		return nil, fmt.Errorf("invalid original lifecycle value: %q", lifecycle)
	case api.PackageRevisionLifecycleDraft:
		// Only draf can be updated.
	case api.PackageRevisionLifecycleProposed, api.PackageRevisionLifecyclePublished:
		// TODO: generate errors that can be translated to correct HTTP responses
		return nil, fmt.Errorf("cannot update a package revision with lifecycle value %q; package must be Draft", lifecycle)
	}

	repo, err := cad.cache.OpenRepository(ctx, repositoryObj)
	if err != nil {
		return nil, err
	}

	draft, err := repo.UpdatePackageRevision(ctx, oldPackage.repoPackageRevision)
	if err != nil {
		return nil, err
	}

	mutations := []mutation{
		&mutationReplaceResources{
			newResources: new,
			oldResources: old,
		},
		&renderPackageMutation{
			renderer: cad.renderer,
			runtime:  cad.runtime,
		},
	}

	apiResources, err := oldPackage.repoPackageRevision.GetResources(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot get package resources: %w", err)
	}
	resources := repository.PackageResources{
		Contents: apiResources.Spec.Resources,
	}

	if err := applyResourceMutations(ctx, draft, resources, mutations); err != nil {
		return nil, err
	}

	// No lifecycle change when updating package resources; updates are done.
	repoPkgRev, err := draft.Close(ctx)
	if err != nil {
		return nil, err
	}
	return &PackageRevision{
		repoPackageRevision: repoPkgRev,
	}, nil
}

func applyResourceMutations(ctx context.Context, draft repository.PackageDraft, baseResources repository.PackageResources, mutations []mutation) error {
	for _, m := range mutations {
		applied, task, err := m.Apply(ctx, baseResources)
		if err != nil {
			return err
		}
		if err := draft.UpdateResources(ctx, &api.PackageRevisionResources{
			Spec: api.PackageRevisionResourcesSpec{
				Resources: applied.Contents,
			},
		}, task); err != nil {
			return err
		}
		baseResources = applied
	}

	return nil
}

func (cad *cadEngine) ListFunctions(ctx context.Context, repositoryObj *configapi.Repository) ([]*Function, error) {
	ctx, span := tracer.Start(ctx, "cadEngine::ListFunctions", trace.WithAttributes())
	defer span.End()

	repo, err := cad.cache.OpenRepository(ctx, repositoryObj)
	if err != nil {
		return nil, err
	}

	fns, err := repo.ListFunctions(ctx)
	if err != nil {
		return nil, err
	}

	var functions []*Function
	for _, f := range fns {
		functions = append(functions, &Function{
			RepoFunction: f,
		})
	}

	return functions, nil
}

type updatePackageMutation struct {
	cloneTask         *api.Task
	updateTask        *api.Task
	repoOpener        RepositoryOpener
	referenceResolver ReferenceResolver
	namespace         string
	pkgName           string
}

func (m *updatePackageMutation) Apply(ctx context.Context, resources repository.PackageResources) (repository.PackageResources, *api.Task, error) {
	ctx, span := tracer.Start(ctx, "updatePackageMutation::Apply", trace.WithAttributes())
	defer span.End()

	currUpstreamPkgRef, err := m.currUpstream()
	if err != nil {
		return repository.PackageResources{}, nil, err
	}

	targetUpstream := m.updateTask.Update.Upstream
	if targetUpstream.Type == api.RepositoryTypeGit || targetUpstream.Type == api.RepositoryTypeOCI {
		return repository.PackageResources{}, nil, fmt.Errorf("update is not supported for non-porch upstream packages")
	}

	originalResources, err := (&PackageFetcher{
		repoOpener:        m.repoOpener,
		referenceResolver: m.referenceResolver,
	}).FetchResources(ctx, currUpstreamPkgRef, m.namespace)
	if err != nil {
		return repository.PackageResources{}, nil, fmt.Errorf("error fetching the resources for package %s with ref %+v",
			m.pkgName, *currUpstreamPkgRef)
	}

	upstreamRevision, err := (&PackageFetcher{
		repoOpener:        m.repoOpener,
		referenceResolver: m.referenceResolver,
	}).FetchRevision(ctx, targetUpstream.UpstreamRef, m.namespace)
	if err != nil {
		return repository.PackageResources{}, nil, fmt.Errorf("error fetching revision for target upstream %s", targetUpstream.UpstreamRef.Name)
	}
	upstreamResources, err := upstreamRevision.GetResources(ctx)
	if err != nil {
		return repository.PackageResources{}, nil, fmt.Errorf("error fetching resources for target upstream %s", targetUpstream.UpstreamRef.Name)
	}

	klog.Infof("performing pkg upgrade operation for pkg %s resource counts local[%d] original[%d] upstream[%d]",
		m.pkgName, len(resources.Contents), len(originalResources.Spec.Resources), len(upstreamResources.Spec.Resources))

	// May be have packageUpdater part of engine to make it easy for testing ?
	updatedResources, err := (&defaultPackageUpdater{}).Update(ctx,
		resources,
		repository.PackageResources{
			Contents: originalResources.Spec.Resources,
		},
		repository.PackageResources{
			Contents: upstreamResources.Spec.Resources,
		})
	if err != nil {
		return repository.PackageResources{}, nil, fmt.Errorf("error updating the package to revision %s", targetUpstream.UpstreamRef.Name)
	}

	newUpstream, newUpstreamLock, err := upstreamRevision.GetLock()
	if err != nil {
		return repository.PackageResources{}, nil, fmt.Errorf("error fetching the resources for package revisions %s", targetUpstream.UpstreamRef.Name)
	}
	if err := kpt.UpdateKptfileUpstream("", updatedResources.Contents, newUpstream, newUpstreamLock); err != nil {
		return repository.PackageResources{}, nil, fmt.Errorf("failed to apply upstream lock to package %q: %w", m.pkgName, err)
	}

	// ensure merge-key comment is added to newly added resources.
	result, err := ensureMergeKey(ctx, updatedResources)
	if err != nil {
		klog.Infof("failed to add merge key comments: %v", err)
	}
	return result, m.updateTask, nil
}

// Currently assumption is that downstream packages will be forked from a porch package.
// As per current implementation, upstream package ref is stored in a new update task but this may
// change so the logic of figuring out current upstream will live in this function.
func (m *updatePackageMutation) currUpstream() (*api.PackageRevisionRef, error) {
	if m.cloneTask == nil || m.cloneTask.Clone == nil {
		return nil, fmt.Errorf("package %s does not have original upstream info", m.pkgName)
	}
	upstream := m.cloneTask.Clone.Upstream
	if upstream.Type == api.RepositoryTypeGit || upstream.Type == api.RepositoryTypeOCI {
		return nil, fmt.Errorf("upstream package must be porch native package. Found it to be %s", upstream.Type)
	}
	return upstream.UpstreamRef, nil
}

func findCloneTask(pr *api.PackageRevision) *api.Task {
	if len(pr.Spec.Tasks) == 0 {
		return nil
	}
	firstTask := pr.Spec.Tasks[0]
	if firstTask.Type == api.TaskTypeClone {
		return &firstTask
	}
	return nil
}

func writeResourcesToDirectory(dir string, resources repository.PackageResources) error {
	for k, v := range resources.Contents {
		p := filepath.Join(dir, k)
		dir := filepath.Dir(p)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %q: %w", dir, err)
		}
		if err := os.WriteFile(p, []byte(v), 0644); err != nil {
			return fmt.Errorf("failed to write file %q: %w", dir, err)
		}
	}
	return nil
}

func loadResourcesFromDirectory(dir string) (repository.PackageResources, error) {
	// TODO: return abstraction instead of loading everything
	result := repository.PackageResources{
		Contents: map[string]string{},
	}
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("cannot compute relative path %q, %q, %w", dir, path, err)
		}

		contents, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("cannot read file %q: %w", dir, err)
		}
		result.Contents[rel] = string(contents)
		return nil
	}); err != nil {
		return repository.PackageResources{}, err
	}

	return result, nil
}

type mutationReplaceResources struct {
	newResources *api.PackageRevisionResources
	oldResources *api.PackageRevisionResources
}

func (m *mutationReplaceResources) Apply(ctx context.Context, resources repository.PackageResources) (repository.PackageResources, *api.Task, error) {
	ctx, span := tracer.Start(ctx, "mutationReplaceResources::Apply", trace.WithAttributes())
	defer span.End()

	patch := &api.PackagePatchTaskSpec{}

	old := resources.Contents
	new, err := healConfig(old, m.newResources.Spec.Resources)
	if err != nil {
		return repository.PackageResources{}, nil, fmt.Errorf("failed to heal resources: %w", err)
	}

	for k, newV := range new {
		oldV, ok := old[k]
		// New config or changed config
		if !ok {
			patchSpec := api.PatchSpec{
				File:      k,
				PatchType: api.PatchTypeCreateFile,
				Contents:  newV,
			}
			patch.Patches = append(patch.Patches, patchSpec)
		} else if newV != oldV {
			patchSpec, err := GeneratePatch(k, oldV, newV)
			if err != nil {
				return repository.PackageResources{}, nil, fmt.Errorf("error generating patch: %w", err)
			}

			patch.Patches = append(patch.Patches, patchSpec)
		}
	}
	for k := range old {
		// Deleted config
		if _, ok := new[k]; !ok {
			patchSpec := api.PatchSpec{
				File:      k,
				PatchType: api.PatchTypeDeleteFile,
			}
			patch.Patches = append(patch.Patches, patchSpec)
		}
	}
	task := &api.Task{
		Type:  api.TaskTypePatch,
		Patch: patch,
	}

	return repository.PackageResources{Contents: new}, task, nil
}

func healConfig(old, new map[string]string) (map[string]string, error) {
	// Copy comments from old config to new
	oldResources, err := (&packageReader{
		input: repository.PackageResources{Contents: old},
		extra: map[string]string{},
	}).Read()
	if err != nil {
		return nil, fmt.Errorf("failed to read old packge resources: %w", err)
	}

	var filter kio.FilterFunc = func(r []*yaml.RNode) ([]*yaml.RNode, error) {
		for _, n := range r {
			for _, original := range oldResources {
				if n.GetNamespace() == original.GetNamespace() &&
					n.GetName() == original.GetName() &&
					n.GetApiVersion() == original.GetApiVersion() &&
					n.GetKind() == original.GetKind() {
					comments.CopyComments(original, n)
				}
			}
		}
		return r, nil
	}

	out := &packageWriter{
		output: repository.PackageResources{
			Contents: map[string]string{},
		},
	}

	extra := map[string]string{}

	if err := (kio.Pipeline{
		Inputs: []kio.Reader{&packageReader{
			input: repository.PackageResources{Contents: new},
			extra: extra,
		}},
		Filters:               []kio.Filter{filter},
		Outputs:               []kio.Writer{out},
		ContinueOnEmptyResult: true,
	}).Execute(); err != nil {
		return nil, err
	}

	healed := out.output.Contents

	for k, v := range extra {
		healed[k] = v
	}

	return healed, nil
}

// isRecloneAndReplay determines if an update should be handled using reclone-and-replay semantics.
// We detect this by checking if both old and new versions start by cloning a package, but the version has changed.
// We may expand this scope in future.
func isRecloneAndReplay(oldObj, newObj *api.PackageRevision) bool {
	oldTasks := oldObj.Spec.Tasks
	newTasks := newObj.Spec.Tasks
	if len(oldTasks) == 0 || len(newTasks) == 0 {
		return false
	}

	if oldTasks[0].Type != api.TaskTypeClone || newTasks[0].Type != api.TaskTypeClone {
		return false
	}

	if reflect.DeepEqual(oldTasks[0], newTasks[0]) {
		return false
	}
	return true
}

// recloneAndReplay performs an update by recloning the upstream package and replaying all tasks.
// This is more like a git rebase operation than the "classic" kpt update algorithm, which is more like a git merge.
func (cad *cadEngine) recloneAndReplay(ctx context.Context, repo repository.Repository, repositoryObj *configapi.Repository, newObj *api.PackageRevision, packageConfig *builtins.PackageConfig) (repository.PackageRevision, error) {
	ctx, span := tracer.Start(ctx, "cadEngine::recloneAndReplay", trace.WithAttributes())
	defer span.End()

	// For reclone and replay, we create a new package every time
	// the version should be in newObj so we will overwrite.
	draft, err := repo.CreatePackageRevision(ctx, newObj)
	if err != nil {
		return nil, err
	}

	if err := cad.applyTasks(ctx, draft, repositoryObj, newObj, packageConfig); err != nil {
		return nil, err
	}

	if err := draft.UpdateLifecycle(ctx, newObj.Spec.Lifecycle); err != nil {
		return nil, err
	}

	return draft.Close(ctx)
}

// ExtractContextConfigMap returns the package-context configmap, if found
func ExtractContextConfigMap(resources map[string]string) (*unstructured.Unstructured, error) {
	var matches []*unstructured.Unstructured

	for itemPath, itemContents := range resources {
		ext := path.Ext(itemPath)
		ext = strings.ToLower(ext)

		parse := false
		switch ext {
		case ".yaml", ".yml":
			parse = true

		default:
			klog.Warningf("ignoring non-yaml file %s", itemPath)
		}

		if !parse {
			continue
		}
		// TODO: Use https://github.com/kubernetes-sigs/kustomize/blob/a5b61016bb40c30dd1b0a78290b28b2330a0383e/kyaml/kio/byteio_reader.go#L170 or similar?
		for _, s := range strings.Split(itemContents, "\n---\n") {
			if isWhitespace(s) {
				continue
			}

			o := &unstructured.Unstructured{}
			if err := yaml.Unmarshal([]byte(s), &o); err != nil {
				return nil, fmt.Errorf("error parsing yaml from %s: %w", itemPath, err)
			}

			// TODO: sync with kpt logic; skip objects marked with the local-only annotation

			configMapGK := schema.GroupKind{Kind: "ConfigMap"}
			if o.GroupVersionKind().GroupKind() == configMapGK {
				if o.GetName() == builtins.PkgContextName {
					matches = append(matches, o)
				}
			}
		}
	}
	if len(matches) == 0 {
		return nil, nil
	}

	if len(matches) > 1 {
		return nil, fmt.Errorf("found multiple configmaps matching name %q", builtins.PkgContextFile)
	}

	return matches[0], nil
}

func isWhitespace(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
