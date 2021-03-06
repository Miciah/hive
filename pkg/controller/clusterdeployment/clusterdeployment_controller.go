/*
Copyright 2018 The Kubernetes Authors.

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

package clusterdeployment

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	log "github.com/sirupsen/logrus"

	kbatch "k8s.io/api/batch/v1"
	kapi "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	hivev1 "github.com/openshift/hive/pkg/apis/hive/v1alpha1"
	"github.com/openshift/hive/pkg/install"
)

const (
	installerImage   = "registry.svc.ci.openshift.org/openshift/origin-v4.0:installer"
	uninstallerImage = "registry.svc.ci.openshift.org/openshift/origin-v4.0:installer" // TODO
	hiveImage        = "hive-controller:latest"

	// serviceAccountName will be a service account that can run the installer and then
	// upload artifacts to the cluster's namespace.
	serviceAccountName = "cluster-installer"
	roleName           = "cluster-installer"
	roleBindingName    = "cluster-installer"

	// deleteAfterAnnotation is the annotation that contains a duration after which the cluster should be cleaned up.
	deleteAfterAnnotation = "hive.openshift.io/delete-after"
)

// Add creates a new ClusterDeployment Controller and adds it to the Manager with default RBAC. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return AddToManager(mgr, NewReconciler(mgr))
}

// NewReconciler returns a new reconcile.Reconciler
func NewReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileClusterDeployment{Client: mgr.GetClient(), scheme: mgr.GetScheme()}
}

// AddToManager adds a new Controller to mgr with r as the reconcile.Reconciler
func AddToManager(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("clusterdeployment-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to ClusterDeployment
	err = c.Watch(&source.Kind{Type: &hivev1.ClusterDeployment{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch for jobs created by a ClusterDeployment:
	err = c.Watch(&source.Kind{Type: &kbatch.Job{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &hivev1.ClusterDeployment{},
	})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileClusterDeployment{}

// ReconcileClusterDeployment reconciles a ClusterDeployment object
type ReconcileClusterDeployment struct {
	client.Client
	scheme *runtime.Scheme
}

// Reconcile reads that state of the cluster for a ClusterDeployment object and makes changes based on the state read
// and what is in the ClusterDeployment.Spec
//
// Automatically generate RBAC rules to allow the Controller to read and write Deployments
//
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=serviceaccounts;secrets;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=hive.openshift.io,resources=clusterdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=hive.openshift.io,resources=clusterdeployments/finalizers,verbs=get;list;watch;create;update;patch;delete
func (r *ReconcileClusterDeployment) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	// Fetch the ClusterDeployment instance
	cd := &hivev1.ClusterDeployment{}
	err := r.Get(context.TODO(), request.NamespacedName, cd)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}
	cdLog := log.WithFields(log.Fields{
		"clusterDeployment": cd.Name,
		"namespace":         cd.Namespace,
	})
	cdLog.Info("reconciling cluster deployment")
	cd = cd.DeepCopy()

	_, err = r.setupClusterInstallServiceAccount(cd.Namespace, cdLog)
	if err != nil {
		cdLog.WithError(err).Error("error setting up service account and role")
		return reconcile.Result{}, err
	}

	if cd.DeletionTimestamp != nil {
		if !HasFinalizer(cd, hivev1.FinalizerDeprovision) {
			return reconcile.Result{}, nil
		}
		return r.syncDeletedClusterDeployment(cd, cdLog)
	}

	// requeueAfter will be used to determine if cluster should be requeued after
	// reconcile has completed
	var requeueAfter time.Duration
	// Check for the delete-after annotation, and if the cluster has expired, delete it
	deleteAfter, ok := cd.Annotations[deleteAfterAnnotation]
	if ok {
		cdLog.Debugf("found delete after annotation: %s", deleteAfter)
		dur, err := time.ParseDuration(deleteAfter)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("error parsing %s as a duration: %v", deleteAfterAnnotation, err)
		}
		if !cd.CreationTimestamp.IsZero() {
			expiry := cd.CreationTimestamp.Add(dur)
			cdLog.Debugf("cluster expires at: %s", expiry)
			if time.Now().After(expiry) {
				cdLog.WithField("expiry", expiry).Info("cluster has expired, issuing delete")
				r.Delete(context.TODO(), cd)
				return reconcile.Result{}, nil
			}

			// We have an expiry time but we're not expired yet. Set requeueAfter for just after expiry time
			// so that we requeue cluster for deletion once reconcile has completed
			requeueAfter = expiry.Sub(time.Now()) + 60*time.Second
		}
	}

	if !HasFinalizer(cd, hivev1.FinalizerDeprovision) {
		cdLog.Debugf("adding clusterdeployment finalizer")
		return reconcile.Result{}, r.addClusterDeploymentFinalizer(cd)
	}

	job := install.GenerateInstallerJob(cd, serviceAccountName, installerImage, kapi.PullAlways,
		hiveImage, kapi.PullIfNotPresent)

	if err := controllerutil.SetControllerReference(cd, job, r.scheme); err != nil {
		cdLog.Errorf("error setting controller reference on job", err)
		return reconcile.Result{}, err
	}
	cdLog = cdLog.WithField("job", job.Name)

	// Check if the Job already exists for this ClusterDeployment:
	existingJob := &kbatch.Job{}
	err = r.Get(context.TODO(), types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, existingJob)
	if err != nil && errors.IsNotFound(err) {
		// If the ClusterDeployment is already installed, we do not need to create a new job:
		if cd.Status.Installed {
			cdLog.Debug("cluster is already installed, no job needed")
		} else {
			cdLog.Infof("creating install job")
			err = r.Create(context.TODO(), job)
			if err != nil {
				cdLog.Errorf("error creating job: %v", err)
				return reconcile.Result{}, err
			}
		}
	} else if err != nil {
		cdLog.Errorf("error getting job: %v", err)
		return reconcile.Result{}, err
	} else {
		cdLog.Infof("cluster job exists, successful: %v", cd.Status.Installed)
	}

	err = r.updateClusterDeploymentStatus(cd, existingJob, cdLog)
	if err != nil {
		cdLog.WithError(err).Errorf("error updating cluster deployment status")
		return reconcile.Result{}, err
	}

	cdLog.Debugf("reconcile complete")
	// Check for requeueAfter duration
	if requeueAfter != 0 {
		cdLog.Debugf("cluster will re-sync due to expiry time in: %v", requeueAfter)
		return reconcile.Result{Requeue: true, RequeueAfter: requeueAfter}, nil
	}
	return reconcile.Result{}, nil
}

func (r *ReconcileClusterDeployment) updateClusterDeploymentStatus(cd *hivev1.ClusterDeployment, job *kbatch.Job, cdLog log.FieldLogger) error {
	cdLog.Debug("updating cluster deployment status")
	origCD := cd
	cd = cd.DeepCopy()
	if job != nil {
		// Job exists, check it's status:
		cd.Status.Installed = isSuccessful(job)
	}

	if cd.Status.Installed {
		if cd.Status.ClusterUUID == "" {
			metadataCfgMap := &kapi.ConfigMap{}
			configMapName := fmt.Sprintf("%s-metadata", cd.Name)
			err := r.Get(context.TODO(), types.NamespacedName{Name: configMapName, Namespace: cd.Namespace}, metadataCfgMap)
			if err != nil {
				// This would be pretty strange for a cluster that is installed:
				cdLog.WithField("configmap", configMapName).WithError(err).Warn("error looking up metadata configmap")
				return err
			}

			// Dynamically parse the JSON to get the UUID we need:
			var objMap map[string]interface{}
			if err := json.Unmarshal([]byte(metadataCfgMap.Data["metadata.json"]), &objMap); err != nil {
				cdLog.WithError(err).Error("error reading json from metadata")
				return err
			}
			aws, ok := objMap["aws"].(map[string]interface{})
			if !ok {
				return fmt.Errorf("cluster metadata did not contain aws.identifier.tectonicClusterID")
			}
			identifier, ok := aws["identifier"].(map[string]interface{})
			if !ok {
				return fmt.Errorf("cluster metadata did not contain aws.identifier.tectonicClusterID")
			}
			cd.Status.ClusterUUID, ok = identifier["tectonicClusterID"].(string)
			if !ok {
				return fmt.Errorf("cluster metadata did not contain aws.identifier.tectonicClusterID")
			}
		}
	}

	// Update cluster deployment status if changed:
	if !reflect.DeepEqual(cd.Status, origCD.Status) {
		cdLog.Infof("status has changed, updating cluster deployment")
		err := r.Update(context.TODO(), cd)
		if err != nil {
			cdLog.Errorf("error updating cluster deployment: %v", err)
			return err
		}
	} else {
		cdLog.Infof("cluster deployment status unchanged")
	}

	return nil
}

func (r *ReconcileClusterDeployment) syncDeletedClusterDeployment(cd *hivev1.ClusterDeployment, cdLog log.FieldLogger) (reconcile.Result, error) {
	// Generate an uninstall job:
	uninstallJob, err := install.GenerateUninstallerJob(cd, installerImage, kapi.PullAlways)
	if err != nil {
		cdLog.Errorf("error generating uninstaller job: %v", err)
		return reconcile.Result{}, err
	}

	err = controllerutil.SetControllerReference(cd, uninstallJob, r.scheme)
	if err != nil {
		cdLog.Errorf("error setting controller reference on job: %v", err)
		return reconcile.Result{}, err
	}

	// Check if uninstall job already exists:
	existingJob := &kbatch.Job{}
	err = r.Get(context.TODO(), types.NamespacedName{Name: uninstallJob.Name, Namespace: uninstallJob.Namespace}, existingJob)
	if err != nil && errors.IsNotFound(err) {
		err = r.Create(context.TODO(), uninstallJob)
		if err != nil {
			cdLog.Errorf("error creating uninstall job: %v", err)
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	} else if err != nil {
		cdLog.Errorf("error getting uninstall job: %v", err)
		return reconcile.Result{}, err
	}

	// Uninstall job exists, check it's status and if successful, remove the finalizer:
	if isSuccessful(existingJob) {
		cdLog.Infof("uninstall job successful, removing finalizer")
		return reconcile.Result{}, r.removeClusterDeploymentFinalizer(cd)
	}

	cdLog.Infof("uninstall job not yet successful")
	return reconcile.Result{}, nil
}

func (r *ReconcileClusterDeployment) addClusterDeploymentFinalizer(cd *hivev1.ClusterDeployment) error {
	cd = cd.DeepCopy()
	AddFinalizer(cd, hivev1.FinalizerDeprovision)
	return r.Update(context.TODO(), cd)
}

func (r *ReconcileClusterDeployment) removeClusterDeploymentFinalizer(cd *hivev1.ClusterDeployment) error {
	cd = cd.DeepCopy()
	DeleteFinalizer(cd, hivev1.FinalizerDeprovision)
	return r.Update(context.TODO(), cd)
}

// setupClusterInstallServiceAccount ensures a service account exists which can upload
// the required artifacts after running the installer in a pod. (metadata, admin kubeconfig)
func (r *ReconcileClusterDeployment) setupClusterInstallServiceAccount(namespace string, cdLog log.FieldLogger) (*kapi.ServiceAccount, error) {
	// create new serviceaccount if it doesn't already exist
	currentSA := &kapi.ServiceAccount{}
	err := r.Client.Get(context.Background(), client.ObjectKey{Name: serviceAccountName, Namespace: namespace}, currentSA)
	if err != nil && !errors.IsNotFound(err) {
		return nil, fmt.Errorf("error checking for existing serviceaccount")
	}

	if errors.IsNotFound(err) {
		currentSA = &kapi.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceAccountName,
				Namespace: namespace,
			},
		}
		err = r.Client.Create(context.Background(), currentSA)

		if err != nil {
			return nil, fmt.Errorf("error creating serviceaccount: %v", err)
		}
		cdLog.WithField("name", serviceAccountName).Info("created service account")
	} else {
		cdLog.WithField("name", serviceAccountName).Debug("service account already exists")
	}

	expectedRole := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      roleName,
			Namespace: namespace,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"secrets", "configmaps"},
				Verbs:     []string{"create", "delete", "get", "list", "update"},
			},
			{
				APIGroups: []string{"hive.openshift.io"},
				Resources: []string{"clusterdeployments", "clusterdeployments/finalizers"},
				Verbs:     []string{"create", "delete", "get", "list", "update"},
			},
		},
	}
	currentRole := &rbacv1.Role{}
	err = r.Client.Get(context.Background(), client.ObjectKey{Name: roleName, Namespace: namespace}, currentRole)
	if err != nil && !errors.IsNotFound(err) {
		return nil, fmt.Errorf("error checking for existing role: %v", err)
	}
	if errors.IsNotFound(err) {
		err = r.Client.Create(context.Background(), expectedRole)
		if err != nil {
			return nil, fmt.Errorf("error creating role: %v", err)
		}
		cdLog.WithField("name", roleName).Info("created role")
	} else {
		cdLog.WithField("name", roleName).Debug("role already exists")
	}

	// create rolebinding for the serviceaccount
	currentRB := &rbacv1.RoleBinding{}
	err = r.Client.Get(context.Background(), client.ObjectKey{Name: roleBindingName, Namespace: namespace}, currentRB)

	if err != nil && !errors.IsNotFound(err) {
		return nil, fmt.Errorf("error checking for existing rolebinding: %v", err)
	}

	if errors.IsNotFound(err) {
		rb := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      roleBindingName,
				Namespace: namespace,
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "ServiceAccount",
					Name:      currentSA.Name,
					Namespace: namespace,
				},
			},
			RoleRef: rbacv1.RoleRef{
				Name: roleName,
				Kind: "Role",
			},
		}

		err = r.Client.Create(context.Background(), rb)
		if err != nil {
			return nil, fmt.Errorf("error creating rolebinding: %v", err)
		}
		cdLog.WithField("name", roleBindingName).Info("created rolebinding")
	} else {
		cdLog.WithField("name", roleBindingName).Debug("rolebinding already exists")
	}

	return currentSA, nil
}

// getJobConditionStatus gets the status of the condition in the job. If the
// condition is not found in the job, then returns False.
func getJobConditionStatus(job *kbatch.Job, conditionType kbatch.JobConditionType) kapi.ConditionStatus {
	for _, condition := range job.Status.Conditions {
		if condition.Type == conditionType {
			return condition.Status
		}
	}
	return kapi.ConditionFalse
}

func isSuccessful(job *kbatch.Job) bool {
	return getJobConditionStatus(job, kbatch.JobComplete) == kapi.ConditionTrue
}

func isFailed(job *kbatch.Job) bool {
	return getJobConditionStatus(job, kbatch.JobFailed) == kapi.ConditionTrue
}

// HasFinalizer returns true if the given object has the given finalizer
func HasFinalizer(object metav1.Object, finalizer string) bool {
	for _, f := range object.GetFinalizers() {
		if f == finalizer {
			return true
		}
	}
	return false
}

// AddFinalizer adds a finalizer to the given object
func AddFinalizer(object metav1.Object, finalizer string) {
	finalizers := sets.NewString(object.GetFinalizers()...)
	finalizers.Insert(finalizer)
	object.SetFinalizers(finalizers.List())
}

// DeleteFinalizer removes a finalizer from the given object
func DeleteFinalizer(object metav1.Object, finalizer string) {
	finalizers := sets.NewString(object.GetFinalizers()...)
	finalizers.Delete(finalizer)
	object.SetFinalizers(finalizers.List())
}
