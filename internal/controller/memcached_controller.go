/*
Copyright 2023.

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
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/util/intstr"
	"os"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cachev1alpha1 "github.com/example/directpv-operator/api/v1alpha1"
)

const deployerFinalizer = "cache.example.com/finalizer"

// Definitions to manage status conditions
const (
	// typeAvailableDeployer represents the status of the Deployment reconciliation
	typeAvailableDeployer = "Available"
	// typeDegradedDeployer represents the status used when the custom resource is deleted and the finalizer operations are must to occur.
	typeDegradedDeployer = "Degraded"
)

// DeployerReconciler reconciles a Deployer object
type DeployerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// The following markers are used to generate the rules permissions (RBAC) on config/rbac using controller-gen
// when the command <make manifests> is executed.
// To know more about markers see: https://book.kubebuilder.io/reference/markers.html

//+kubebuilder:rbac:groups=cache.example.com,resources=deployers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cache.example.com,resources=deployers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cache.example.com,resources=deployers/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=directpvdrives,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=directpv.min.io,resources=directpvdrives,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=directpvvolumes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=directpv.min.io,resources=directpvvolumes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=directpv.min.io,namespace=directpv,resources=directpvdrives,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.

// It is essential for the controller's reconciliation loop to be idempotent. By following the Operator
// pattern you will create Controllers which provide a reconcile function
// responsible for synchronizing resources until the desired state is reached on the cluster.
// Breaking this recommendation goes against the design principles of controller-runtime.
// and may lead to unforeseen consequences such as resources becoming stuck and requiring manual intervention.
// For further info:
// - About Operator Pattern: https://kubernetes.io/docs/concepts/extend-kubernetes/operator/
// - About Controllers: https://kubernetes.io/docs/concepts/architecture/controller/
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *DeployerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Deployer instance
	// The purpose is check if the Custom Resource for the Kind Deployer
	// is applied on the cluster if not we return nil to stop the reconciliation
	deployer := &cachev1alpha1.Deployer{}
	deployer.Namespace = "directpv"
	err := r.Get(ctx, req.NamespacedName, deployer)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If the custom resource is not found then, it usually means that it was deleted or not created
			// In this way, we will stop the reconciliation
			log.Info("deployer resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get deployer")
		return ctrl.Result{}, err
	}

	// Let's just set the status as Unknown when no status are available
	if deployer.Status.Conditions == nil || len(deployer.Status.Conditions) == 0 {
		meta.SetStatusCondition(&deployer.Status.Conditions, metav1.Condition{Type: typeAvailableDeployer, Status: metav1.ConditionUnknown, Reason: "Reconciling", Message: "Starting reconciliation"})
		if err = r.Status().Update(ctx, deployer); err != nil {
			log.Error(err, "Failed to update Deployer status")
			return ctrl.Result{}, err
		}

		// Let's re-fetch the deployer Custom Resource after update the status
		// so that we have the latest state of the resource on the cluster and we will avoid
		// raise the issue "the object has been modified, please apply
		// your changes to the latest version and try again" which would re-trigger the reconciliation
		// if we try to update it again in the following operations
		if err := r.Get(ctx, req.NamespacedName, deployer); err != nil {
			log.Error(err, "Failed to re-fetch deployer")
			return ctrl.Result{}, err
		}
	}

	// Let's add a finalizer. Then, we can define some operations which should
	// occurs before the custom resource to be deleted.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers
	if !controllerutil.ContainsFinalizer(deployer, deployerFinalizer) {
		log.Info("Adding Finalizer for Deployer")
		if ok := controllerutil.AddFinalizer(deployer, deployerFinalizer); !ok {
			log.Error(err, "Failed to add finalizer into the custom resource")
			return ctrl.Result{Requeue: true}, nil
		}

		if err = r.Update(ctx, deployer); err != nil {
			log.Error(err, "Failed to update custom resource to add finalizer")
			return ctrl.Result{}, err
		}
	}

	// Check if the Memcached instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	isMemcachedMarkedToBeDeleted := deployer.GetDeletionTimestamp() != nil
	if isMemcachedMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(deployer, deployerFinalizer) {
			log.Info("Performing Finalizer Operations for Deployer before delete CR")

			// Let's add here an status "Downgrade" to define that this resource begin its process to be terminated.
			meta.SetStatusCondition(&deployer.Status.Conditions, metav1.Condition{Type: typeDegradedDeployer,
				Status: metav1.ConditionUnknown, Reason: "Finalizing",
				Message: fmt.Sprintf("Performing finalizer operations for the custom resource: %s ", deployer.Name)})

			if err := r.Status().Update(ctx, deployer); err != nil {
				log.Error(err, "Failed to update Memcached status")
				return ctrl.Result{}, err
			}

			// Perform all operations required before remove the finalizer and allow
			// the Kubernetes API to remove the custom resource.
			r.doFinalizerOperationsForDeployer(deployer)

			// TODO(user): If you add operations to the doFinalizerOperationsForDeployer method
			// then you need to ensure that all worked fine before deleting and updating the Downgrade status
			// otherwise, you should requeue here.

			// Re-fetch the deployer Custom Resource before update the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raise the issue "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err := r.Get(ctx, req.NamespacedName, deployer); err != nil {
				log.Error(err, "Failed to re-fetch memcached")
				return ctrl.Result{}, err
			}

			meta.SetStatusCondition(&deployer.Status.Conditions, metav1.Condition{Type: typeDegradedDeployer,
				Status: metav1.ConditionTrue, Reason: "Finalizing",
				Message: fmt.Sprintf("Finalizer operations for custom resource %s name were successfully accomplished", deployer.Name)})

			if err := r.Status().Update(ctx, deployer); err != nil {
				log.Error(err, "Failed to update Memcached status")
				return ctrl.Result{}, err
			}

			log.Info("Removing Finalizer for Memcached after successfully perform the operations")
			if ok := controllerutil.RemoveFinalizer(deployer, deployerFinalizer); !ok {
				log.Error(err, "Failed to remove finalizer for Memcached")
				return ctrl.Result{Requeue: true}, nil
			}

			if err := r.Update(ctx, deployer); err != nil {
				log.Error(err, "Failed to remove finalizer for Memcached")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Check if the daemonset already exists, if not create a new one
	foundDaemonSet := &appsv1.DaemonSet{}
	err = r.Get(ctx, types.NamespacedName{Name: deployer.Name, Namespace: "directpv"}, foundDaemonSet)
	if err != nil && apierrors.IsNotFound(err) {
		// Define a new DaemonSet
		daemonSet, err := r.daemonSetForDeployer(deployer)
		if err != nil {
			log.Error(err, "Failed to define new DaemonSet resource for Deployer")

			// The following implementation will update the status
			meta.SetStatusCondition(&deployer.Status.Conditions, metav1.Condition{Type: typeAvailableDeployer,
				Status: metav1.ConditionFalse, Reason: "Reconciling",
				Message: fmt.Sprintf("Failed to create DaemonSet for the custom resource (%s): (%s)", deployer.Name, err)})

			if err := r.Status().Update(ctx, deployer); err != nil {
				log.Error(err, "Failed to update Deployer status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}
		log.Info("Creating a new DaemonSet...",
			"DaemonSet.Namespace", daemonSet.Namespace, "DaemonSet.Name", daemonSet.Name)
		if err = r.Create(ctx, daemonSet); err != nil {
			log.Error(err, "Failed to create new DaemonSet",
				"DaemonSet.Namespace", daemonSet.Namespace, "DaemonSet.Name", daemonSet.Name)
			return ctrl.Result{}, err
		}
		// DaemonSet created successfully
	} else if err != nil {
		log.Error(err, "Failed to get DaemonSet")
		// Let's return the error for the reconciliation be re-trigged again
		return ctrl.Result{}, err
	}

	// Check if the deployment already exists, if not create a new one
	foundDeployment := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: deployer.Name, Namespace: "directpv"}, foundDeployment)
	if err != nil && apierrors.IsNotFound(err) {
		// Define a new deployment
		dep, err := r.deploymentForDeployer(deployer)
		if err != nil {
			log.Error(err, "Failed to define new Deployment resource for Memcached")

			// The following implementation will update the status
			meta.SetStatusCondition(&deployer.Status.Conditions, metav1.Condition{Type: typeAvailableDeployer,
				Status: metav1.ConditionFalse, Reason: "Reconciling",
				Message: fmt.Sprintf("Failed to create Deployment for the custom resource (%s): (%s)", deployer.Name, err)})

			if err := r.Status().Update(ctx, deployer); err != nil {
				log.Error(err, "Failed to update Memcached status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		log.Info("Creating a new Deployment",
			"Deployment.Namespace", dep.Namespace, "Deployment.Name", dep.Name)
		if err = r.Create(ctx, dep); err != nil {
			log.Error(err, "Failed to create new Deployment",
				"Deployment.Namespace", dep.Namespace, "Deployment.Name", dep.Name)
			return ctrl.Result{}, err
		}

		// Deployment created successfully
		// We will requeue the reconciliation so that we can ensure the state
		// and move forward for the next operations
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	} else if err != nil {
		log.Error(err, "Failed to get Deployment")
		// Let's return the error for the reconciliation be re-trigged again
		return ctrl.Result{}, err
	}

	// The CRD API is defining that the Memcached type, have a MemcachedSpec.Size field
	// to set the quantity of Deployment instances is the desired state on the cluster.
	// Therefore, the following code will ensure the Deployment size is the same as defined
	// via the Size spec of the Custom Resource which we are reconciling.
	size := deployer.Spec.Size
	if *foundDeployment.Spec.Replicas != size {
		foundDeployment.Spec.Replicas = &size
		if err = r.Update(ctx, foundDeployment); err != nil {
			log.Error(err, "Failed to update Deployment",
				"Deployment.Namespace", foundDeployment.Namespace, "Deployment.Name", foundDeployment.Name)

			// Re-fetch the memcached Custom Resource before update the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raise the issue "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err := r.Get(ctx, req.NamespacedName, deployer); err != nil {
				log.Error(err, "Failed to re-fetch memcached")
				return ctrl.Result{}, err
			}

			// The following implementation will update the status
			meta.SetStatusCondition(&deployer.Status.Conditions, metav1.Condition{Type: typeAvailableDeployer,
				Status: metav1.ConditionFalse, Reason: "Resizing",
				Message: fmt.Sprintf("Failed to update the size for the custom resource (%s): (%s)", deployer.Name, err)})

			if err := r.Status().Update(ctx, deployer); err != nil {
				log.Error(err, "Failed to update Memcached status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		// Now, that we update the size we want to requeue the reconciliation
		// so that we can ensure that we have the latest state of the resource before
		// update. Also, it will help ensure the desired state on the cluster
		return ctrl.Result{Requeue: true}, nil
	}

	// The following implementation will update the status
	meta.SetStatusCondition(&deployer.Status.Conditions, metav1.Condition{Type: typeAvailableDeployer,
		Status: metav1.ConditionTrue, Reason: "Reconciling",
		Message: fmt.Sprintf("Deployment for custom resource (%s) with %d replicas created successfully", deployer.Name, size)})

	if err := r.Status().Update(ctx, deployer); err != nil {
		log.Error(err, "Failed to update Memcached status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// finalizeMemcached will perform the required operations before delete the CR.
func (r *DeployerReconciler) doFinalizerOperationsForDeployer(cr *cachev1alpha1.Deployer) {
	// TODO(user): Add the cleanup steps that the operator
	// needs to do before the CR can be deleted. Examples
	// of finalizers include performing backups and deleting
	// resources that are not owned by this CR, like a PVC.

	// Note: It is not recommended to use finalizers with the purpose of delete resources which are
	// created and managed in the reconciliation. These ones, such as the Deployment created on this reconcile,
	// are defined as depended of the custom resource. See that we use the method ctrl.SetControllerReference.
	// to set the ownerRef which means that the Deployment will be deleted by the Kubernetes API.
	// More info: https://kubernetes.io/docs/tasks/administer-cluster/use-cascading-deletion/

	// The following implementation will raise an event
	r.Recorder.Event(cr, "Warning", "Deleting",
		fmt.Sprintf("Custom Resource %s is being deleted from the namespace %s",
			cr.Name,
			cr.Namespace))
}

// nameSpaceForDeployer returns a NameSpace Object.
func (r *DeployerReconciler) nameSpaceForDeployer(memcached *cachev1alpha1.Deployer) (*corev1.Namespace, error) {
	var namespace = &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Namespace",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "directpv",
		},
	}
	return namespace, nil
}

// daemonSetForDeployer returns a Deployer DaemonSet Object.
func (r *DeployerReconciler) daemonSetForDeployer(
	memcached *cachev1alpha1.Deployer) (*appsv1.DaemonSet, error) {
	ls := labelsForMemcached(memcached.Name)
	controllerImage, err := imageForDeployer()
	if err != nil {
		return nil, err
	}
	registrarImage, err := imageForRegistrar()
	if err != nil {
		return nil, err
	}
	livenessProbeImage, err := imageForLivenessProbe()
	if err != nil {
		return nil, err
	}
	hostPathTypeToBeUsed := corev1.HostPathDirectoryOrCreate
	healthZContainerPortName := "healthz"
	mountPropagationMode := corev1.MountPropagationNone
	var daemonset = &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node-server",
			Namespace: memcached.Namespace,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: ls,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ls,
				},
				Spec: corev1.PodSpec{
					SecurityContext:    &corev1.PodSecurityContext{},
					ServiceAccountName: "directpv-min-io",
					Volumes: []corev1.Volume{
						{
							Name: "socket-dir",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/kubelet/plugins/directpv-min-io",
									Type: &hostPathTypeToBeUsed,
								},
							},
						},
						{
							Name: "mountpoint-dir",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/kubelet/pods",
									Type: &hostPathTypeToBeUsed,
								},
							},
						},
						{
							Name: "registration-dir",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/kubelet/plugins_registry",
									Type: &hostPathTypeToBeUsed,
								},
							},
						},
						{
							Name: "plugins-dir",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/kubelet/plugins",
									Type: &hostPathTypeToBeUsed,
								},
							},
						},
						{
							Name: "directpv-common-root",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/directpv/",
									Type: &hostPathTypeToBeUsed,
								},
							},
						},
						{
							Name: "sysfs",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/sys",
									Type: &hostPathTypeToBeUsed,
								},
							},
						},
						{
							Name: "devfs",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/dev",
									Type: &hostPathTypeToBeUsed,
								},
							},
						},
						{
							Name: "run-udev-data-dir",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/run/udev/data",
									Type: &hostPathTypeToBeUsed,
								},
							},
						},
						{
							Name: "direct-csi-common-root",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/direct-csi/",
									Type: &hostPathTypeToBeUsed,
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Image:           registrarImage,
							Name:            "node-driver-registrar",
							ImagePullPolicy: corev1.PullIfNotPresent,
							SecurityContext: &corev1.SecurityContext{
								Privileged: &[]bool{true}[0],
							},
							Args: []string{
								"--v=3",
								"--csi-address=unix:///csi/csi.sock",
								"--kubelet-registration-path=/var/lib/kubelet/plugins/directpv-min-io/csi.sock",
							},
							Env: []corev1.EnvVar{
								{
									Name: "KUBE_NODE_NAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											APIVersion: "v1",
											FieldPath:  "spec.nodeName",
										},
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:             "socket-dir",
									MountPath:        "/csi",
									MountPropagation: &mountPropagationMode,
								},
								{
									Name:             "registration-dir",
									MountPath:        "/registration",
									MountPropagation: &mountPropagationMode,
								},
							},
						},
						{
							Image:           controllerImage,
							Name:            "node-server",
							ImagePullPolicy: corev1.PullIfNotPresent,
							SecurityContext: &corev1.SecurityContext{
								Privileged: &[]bool{true}[0],
							},
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: 30443,
									Name:          "readinessport",
								},
								{
									ContainerPort: 9898,
									Name:          "healthz",
								},
								{
									ContainerPort: 10443,
									Name:          "metrics",
								},
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path:   "/healthz",
										Port:   intstr.FromString(healthZContainerPortName),
										Scheme: "HTTP",
									},
								},
								InitialDelaySeconds: 60,
								TimeoutSeconds:      10,
								PeriodSeconds:       10,
								SuccessThreshold:    1,
								FailureThreshold:    5,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path:   "/ready",
										Port:   intstr.FromString("readinessport"),
										Scheme: "HTTP",
									},
								},
								InitialDelaySeconds: 60,
								TimeoutSeconds:      10,
								PeriodSeconds:       10,
								SuccessThreshold:    1,
								FailureThreshold:    5,
							},
							Args: []string{
								"node-server",
								"-v=3",
								"--identity=directpv-min-io",
								"--csi-endpoint=$(CSI_ENDPOINT)",
								"--kube-node-name=$(KUBE_NODE_NAME)",
								"--readiness-port=30443",
								"--metrics-port=10443",
							},
							Env: []corev1.EnvVar{
								{
									Name:  "CSI_ENDPOINT",
									Value: "unix:///csi/csi.sock",
								},
								{
									Name: "KUBE_NODE_NAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											APIVersion: "v1",
											FieldPath:  "spec.nodeName",
										},
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "socket-dir",
									MountPath: "/csi",
								},
								{
									Name:      "mountpoint-dir",
									MountPath: "/var/lib/kubelet/pods",
								},
								{
									Name:      "plugins-dir",
									MountPath: "/var/lib/kubelet/plugins",
								},
								{
									Name:      "directpv-common-root",
									MountPath: "/var/lib/directpv/",
								},
								{
									Name:      "sysfs",
									MountPath: "/sys",
								},
								{
									Name:      "devfs",
									MountPath: "/dev",
								},
								{
									Name:      "run-udev-data-dir",
									MountPath: "/run/udev/data",
								},
								{
									Name:      "direct-csi-common-root",
									MountPath: "/var/lib/direct-csi/",
								},
							},
						},
						{
							Image:           controllerImage,
							Name:            "node-controller",
							ImagePullPolicy: corev1.PullIfNotPresent,
							SecurityContext: &corev1.SecurityContext{
								Privileged: &[]bool{true}[0],
							},
							Args: []string{
								"node-controller",
								"-v=3",
								"--kube-node-name=$(KUBE_NODE_NAME)",
							},
							Env: []corev1.EnvVar{
								{
									Name: "KUBE_NODE_NAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											APIVersion: "v1",
											FieldPath:  "spec.nodeName",
										},
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "socket-dir",
									MountPath: "/csi",
								},
								{
									Name:      "mountpoint-dir",
									MountPath: "/var/lib/kubelet/pods",
								},
								{
									Name:      "plugins-dir",
									MountPath: "/var/lib/kubelet/plugins",
								},
								{
									Name:      "directpv-common-root",
									MountPath: "/var/lib/directpv/",
								},
								{
									Name:      "sysfs",
									MountPath: "/sys",
								},
								{
									Name:      "devfs",
									MountPath: "/dev",
								},
								{
									Name:      "run-udev-data-dir",
									MountPath: "/run/udev/data",
								},
								{
									Name:      "direct-csi-common-root",
									MountPath: "/var/lib/direct-csi/",
								},
							},
						},
						{
							Image:           livenessProbeImage,
							Name:            "liveness-probe",
							ImagePullPolicy: corev1.PullIfNotPresent,
							SecurityContext: &corev1.SecurityContext{
								Privileged: &[]bool{true}[0],
							},
							Args: []string{
								"--csi-address=/csi/csi.sock",
								"--health-port=9898",
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "socket-dir",
									MountPath: "/csi",
								},
							},
						},
					},
				},
			},
		},
	}
	if err := ctrl.SetControllerReference(memcached, daemonset, r.Scheme); err != nil {
		return nil, err
	}
	return daemonset, nil
}

// deploymentForDeployer returns a Deployer Deployment object
func (r *DeployerReconciler) deploymentForDeployer(
	memcached *cachev1alpha1.Deployer) (*appsv1.Deployment, error) {
	ls := labelsForMemcached(memcached.Name)
	replicas := memcached.Spec.Size

	// Get the images
	controllerImage, err := imageForDeployer()
	if err != nil {
		return nil, err
	}
	resizerImage, err := imageForResizer()
	if err != nil {
		return nil, err
	}
	provisionerImage, err := imageForProvisioner()
	if err != nil {
		return nil, err
	}
	hostPathTypeToBeUsed := corev1.HostPathDirectoryOrCreate
	var dep = &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      memcached.Name,
			Namespace: memcached.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: ls,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ls,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "directpv-min-io",
					SecurityContext:    &corev1.PodSecurityContext{},
					Volumes: []corev1.Volume{
						{
							Name: "socket-dir",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/kubelet/plugins/controller-controller",
									Type: &hostPathTypeToBeUsed,
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Image: provisionerImage,
							Name:  "csi-provisioner",
							Args: []string{
								"--v=3",
								"--timeout=300s",
								"--csi-address=$(CSI_ENDPOINT)",
								"--leader-election",
								"--feature-gates=Topology=true",
								"--strict-topology",
							},
							Env: []corev1.EnvVar{
								{
									Name:  "CSI_ENDPOINT",
									Value: "unix:///csi/csi.sock",
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "socket-dir",
									MountPath: "/csi",
								},
							},
						},
						{
							Image:           controllerImage,
							Name:            "controller",
							ImagePullPolicy: corev1.PullIfNotPresent,
							SecurityContext: &corev1.SecurityContext{
								Privileged: &[]bool{true}[0],
							},
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: 30443,
									Name:          "readinessport",
								},
								{
									ContainerPort: 9898,
									Name:          "healthz",
								},
							},
							Args: []string{
								"controller",
								"--identity=directpv-min-io",
								"-v=3",
								"--csi-endpoint=$(CSI_ENDPOINT)",
								"--kube-node-name=$(KUBE_NODE_NAME)",
								"--readiness-port=30443",
							},
							Env: []corev1.EnvVar{
								{
									Name:  "CSI_ENDPOINT",
									Value: "unix:///csi/csi.sock",
								},
								{
									Name: "KUBE_NODE_NAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											APIVersion: "v1",
											FieldPath:  "spec.nodeName",
										},
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "socket-dir",
									MountPath: "/csi",
								},
							},
						},
						{
							Image: resizerImage,
							Name:  "csi-resizer",
							Args:  []string{"--v=3", "--timeout=300s", "--csi-address=$(CSI_ENDPOINT)", "--leader-election"},
							Env: []corev1.EnvVar{
								{
									Name:  "CSI_ENDPOINT",
									Value: "unix:///csi/csi.sock",
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "socket-dir",
									MountPath: "/csi",
								},
							},
						},
					},
				},
			},
		},
	} // Set the ownerRef for the Deployment
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/
	if err := ctrl.SetControllerReference(memcached, dep, r.Scheme); err != nil {
		return nil, err
	}
	return dep, nil
}

// labelsForMemcached returns the labels for selecting the resources
// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
func labelsForMemcached(name string) map[string]string {
	var imageTag string
	image, err := imageForDeployer()
	if err == nil {
		imageTag = strings.Split(image, ":")[1]
	}
	return map[string]string{"app.kubernetes.io/name": "Memcached",
		"app.kubernetes.io/instance":   name,
		"app.kubernetes.io/version":    imageTag,
		"app.kubernetes.io/part-of":    "directpv-operator",
		"app.kubernetes.io/created-by": "controller-manager",
	}
}

// imageForMemcached gets the Operand image which is managed by this controller
// from the DIRECTPV_IMAGE environment variable defined in the config/manager/manager.yaml
func imageForDeployer() (string, error) {
	var imageEnvVar = "DIRECTPV_IMAGE"
	image, found := os.LookupEnv(imageEnvVar)
	if !found {
		return "", fmt.Errorf("Unable to find %s environment variable with the image", imageEnvVar)
	}
	return image, nil
}

// imageForResizer gets the resizer image
func imageForResizer() (string, error) {
	var imageEnvVar = "CSI_RESIZER"
	image, found := os.LookupEnv(imageEnvVar)
	if !found {
		return "", fmt.Errorf("Unable to find #{imageEnvVar} environment variable with the image")
	}
	return image, nil
}

// imageForProvisioner gets the provisioner image
func imageForProvisioner() (string, error) {
	var imageEnvVar = "CSI_PROVISIONER"
	image, found := os.LookupEnv(imageEnvVar)
	if !found {
		return "", fmt.Errorf("Unable to find #{imageEnvVar} environment variable with the image")
	}
	return image, nil
}

// imageForRegistrar gets the provisioner image
func imageForRegistrar() (string, error) {
	var imageEnvVar = "CSI_NODE_DRIVER_REGISTRAR"
	image, found := os.LookupEnv(imageEnvVar)
	if !found {
		return "", fmt.Errorf("Unable to find #{imageEnvVar} environment variable with the image")
	}
	return image, nil
}

// imageForLivenessProbe gets the liveness probe image
func imageForLivenessProbe() (string, error) {
	var imageEnvVar = "LIVENESS_PROBE"
	image, found := os.LookupEnv(imageEnvVar)
	if !found {
		return "", fmt.Errorf("Unable to find #{imageEnvVar} environment variable with the image")
	}
	return image, nil
}

// SetupWithManager sets up the controller with the Manager.
// Note that the Deployment will be also watched in order to ensure its
// desirable state on the cluster
func (r *DeployerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cachev1alpha1.Deployer{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}
