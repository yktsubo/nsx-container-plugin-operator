/* Copyright © 2020 VMware, Inc. All Rights Reserved.
   SPDX-License-Identifier: Apache-2.0 */

package configmap

import (
	"context"
	"fmt"
	"os"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/cluster-network-operator/pkg/apply"
	k8sutil "github.com/openshift/cluster-network-operator/pkg/util/k8s"
	"github.com/pkg/errors"
	"github.com/vmware/nsx-container-plugin-operator/pkg/controller/sharedinfo"
	"github.com/vmware/nsx-container-plugin-operator/pkg/controller/statusmanager"
	operatortypes "github.com/vmware/nsx-container-plugin-operator/pkg/types"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("controller_configmap")

var appliedConfigMap *corev1.ConfigMap

// Add creates a new ConfigMap Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, status *statusmanager.StatusManager, sharedInfo *sharedinfo.SharedInfo) error {
	return add(mgr, newReconciler(mgr, status, sharedInfo))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, status *statusmanager.StatusManager, sharedInfo *sharedinfo.SharedInfo) reconcile.Reconciler {
	configv1.Install(mgr.GetScheme())
	return &ReconcileConfigMap{
		client:     mgr.GetClient(),
		scheme:     mgr.GetScheme(),
		status:     status,
		sharedInfo: sharedInfo,
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("configmap-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource ConfigMap
	err = c.Watch(&source.Kind{Type: &corev1.ConfigMap{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}
	// Watch for changes to primary resource Network CRD
	err = c.Watch(&source.Kind{Type: &configv1.Network{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileConfigMap implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileConfigMap{}

// ReconcileConfigMap reconciles a ConfigMap object
type ReconcileConfigMap struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client     client.Client
	scheme     *runtime.Scheme
	status     *statusmanager.StatusManager
	sharedInfo *sharedinfo.SharedInfo
}

// Reconcile reads that state of the cluster for a ConfigMap object and makes changes based on the state read
// and what is in the ConfigMap.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileConfigMap) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)

	// Check request namespace and name to ignore other changes
	if request.Namespace == operatortypes.OperatorNamespace && request.Name == operatortypes.ConfigMapName {
		reqLogger.Info("Reconciling nsx-ncp-operator ConfigMap change")
	} else if request.Namespace == "" && request.Name == operatortypes.NetworkCRDName {
		reqLogger.Info("Reconciling cluster Network CRD change")
	} else {
		return reconcile.Result{}, nil
	}

	// Fetch the ConfigMap instance
	instance := &corev1.ConfigMap{}
	instanceName := types.NamespacedName{
		Namespace: operatortypes.OperatorNamespace,
		Name:      operatortypes.ConfigMapName,
	}
	err := r.client.Get(context.TODO(), instanceName, instance)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			log.Info(fmt.Sprintf("%s ConfigMap is not found", operatortypes.ConfigMapName))
			r.status.SetDegraded(statusmanager.OperatorConfig, "NoOperatorConfig",
				fmt.Sprintf("%s ConfigMap is not found", operatortypes.ConfigMapName))
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		r.status.SetDegraded(statusmanager.OperatorConfig, "NoOperatorConfig",
			fmt.Sprintf("Failed to get operator ConfigMap: %v", err))
		return reconcile.Result{}, err
	}

	// Get network CRD configuration
	networkConfig := &configv1.Network{}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: operatortypes.NetworkCRDName}, networkConfig)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			log.Info("Cluster network CRD is not found")
			r.status.SetDegraded(statusmanager.ClusterConfig, "NoClusterConfig", "Cluster network CRD is not found")
			return reconcile.Result{}, nil
		}
		r.status.SetDegraded(statusmanager.ClusterConfig, "NoClusterConfig",
			fmt.Sprintf("Failed to get cluster network CRD: %v", err))
		return reconcile.Result{}, err
	}

	// Fill default configurations
	if err = FillDefaults(instance, &networkConfig.Spec); err != nil {
		r.status.SetDegraded(statusmanager.OperatorConfig, "FillDefaultsError",
			fmt.Sprintf("Failed to fill default configurations: %v", err))
		return reconcile.Result{}, err
	}

	// Validate configurations
	if err = Validate(instance, &networkConfig.Spec); err != nil {
		r.status.SetDegraded(statusmanager.OperatorConfig, "InvalidOperatorConfig",
			fmt.Sprintf("The operator configuration is invalid: %v", err))
		return reconcile.Result{}, err
	}

	// Compare with previous configurations
	if appliedConfigMap == nil {
		ncpConfigMap := &corev1.ConfigMap{}
		err = r.client.Get(context.TODO(), types.NamespacedName{Namespace: operatortypes.NsxNamespace, Name: operatortypes.NcpConfigMapName}, ncpConfigMap)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				log.Error(err, "Failed to get nsx-ncp ConfigMap")
			}
			ncpConfigMap = nil
		}
		agentConfigMap := &corev1.ConfigMap{}
		err = r.client.Get(context.TODO(), types.NamespacedName{Namespace: operatortypes.NsxNamespace, Name: operatortypes.NodeAgentConfigMapName}, agentConfigMap)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				log.Error(err, "Failed to get nsx-node-agent ConfigMap")
			}
			agentConfigMap = nil
		}
		lbSecret := &corev1.Secret{}
		err = r.client.Get(context.TODO(), types.NamespacedName{Namespace: operatortypes.NsxNamespace, Name: operatortypes.LbSecret}, lbSecret)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				log.Error(err, "Failed to get lb-secret")
			}
			lbSecret = nil
		}
		if ncpConfigMap != nil && agentConfigMap != nil {
			appliedConfigMap = &corev1.ConfigMap{}
			appliedConfigMap.Data = make(map[string]string)
			err = GenerateOperatorConfigMap(appliedConfigMap, ncpConfigMap, agentConfigMap, lbSecret)
			if err != nil {
				r.status.SetDegraded(statusmanager.OperatorConfig, "InternalError",
					fmt.Sprintf("Failed to generate operator ConfigMap: %v", err))
				return reconcile.Result{}, err
			}
		}
	}
	ncpNeedChange, agentNeedChange, err := NeedApplyChange(instance, appliedConfigMap)
	if err != nil {
		return reconcile.Result{}, err
	}

	if !ncpNeedChange && !agentNeedChange {
		// Check if NCP_IMAGE changes
		ncpImageChanged, err := r.isNcpImageChanged()
		if err != nil {
			return reconcile.Result{Requeue: true}, err
		}

		if !ncpImageChanged {
			log.Info("no new configuration needs to apply")
			r.status.SetNotDegraded(statusmanager.ClusterConfig)
			r.status.SetNotDegraded(statusmanager.OperatorConfig)
			return reconcile.Result{}, nil
		} else {
			log.Info("NCP image changed")
		}
	}

	// Render configurations
	objs, err := Render(instance)
	if err != nil {
		log.Error(err, "Failed to render configurations")
		r.status.SetDegraded(statusmanager.OperatorConfig, "RenderConfigError",
			fmt.Sprintf("Failed to render operator configuration: %v", err))
		return reconcile.Result{}, err
	}

	r.updateSharedInfoWithNsxNcpResources(objs)
	r.sharedInfo.NetworkConfig = networkConfig

	// Apply objects to K8s cluster
	for _, obj := range objs {
		// Mark the object to be GC'd if the owner is deleted
		err = controllerutil.SetControllerReference(networkConfig, obj, r.scheme)
		if err != nil {
			err = errors.Wrapf(err, "could not set reference for (%s) %s/%s", obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName())
			r.status.SetDegraded(statusmanager.OperatorConfig, "ApplyObjectsError",
				fmt.Sprintf("Failed to apply objects: %v", err))
			return reconcile.Result{}, err
		}

		if err = apply.ApplyObject(context.TODO(), r.client, obj); err != nil {
			log.Error(err, fmt.Sprintf("could not apply (%s) %s/%s", obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName()))
			r.status.SetDegraded(statusmanager.OperatorConfig, "ApplyOperatorConfig",
				fmt.Sprintf("Failed to apply operator configuration: %v", err))
			return reconcile.Result{}, err
		}
	}

	// Delete old NCP and nsx-node-agent pods
	if appliedConfigMap != nil && ncpNeedChange {
		err = deleteExistingPods(r.client, operatortypes.NsxNcpDeploymentName)
		if err != nil {
			r.status.SetDegraded(statusmanager.OperatorConfig, "DeleteOldPodsError",
				fmt.Sprintf("Deployment %s is not using the latest configuration updates because: %v",
					operatortypes.NsxNcpDeploymentName, err))
			return reconcile.Result{}, err
		}
	}
	if appliedConfigMap != nil && agentNeedChange {
		err = deleteExistingPods(r.client, operatortypes.NsxNodeAgentDsName)
		if err != nil {
			r.status.SetDegraded(statusmanager.OperatorConfig, "DeleteOldPodsError",
				fmt.Sprintf("DaemonSet %s is not using the latest configuration updates because: %v",
					operatortypes.NsxNodeAgentDsName, err))
			return reconcile.Result{}, err
		}
	}
	appliedConfigMap = instance
	r.sharedInfo.OperatorConfigMap = appliedConfigMap

	// Update network CRD status
	err = updateNetworkStatus(networkConfig, r)
	if err != nil {
		r.status.SetDegraded(statusmanager.ClusterConfig, "UpdateNetworkStatusError",
			fmt.Sprintf("Failed to update network status: %v", err))
		return reconcile.Result{}, err
	}

	r.status.SetNotDegraded(statusmanager.ClusterConfig)
	r.status.SetNotDegraded(statusmanager.OperatorConfig)
	return reconcile.Result{}, nil
}

func updateNetworkStatus(networkConfig *configv1.Network, r *ReconcileConfigMap) error {
	status := getNetworkCRD(networkConfig)
	// Render information
	networkConfig.Status = status
	data, err := k8sutil.ToUnstructured(networkConfig)
	if err != nil {
		log.Error(err, "Failed to render configurations")
		return err
	}

	if data != nil {
		if err := apply.ApplyObject(context.TODO(), r.client, data); err != nil {
			log.Error(err, fmt.Sprintf("Could not apply (%s) %s/%s", data.GroupVersionKind(),
				data.GetNamespace(), data.GetName()))
			return err
		} else {
			log.Error(err, "Retrieved data for updating network status is empty.")
			return err
		}
	}
	log.Info("Successfully updated Network Status")
	return nil
}

func getNetworkCRD(networkConfig *configv1.Network) configv1.NetworkStatus {
	// Values extracted from spec are serviceNetwork and clusterNetworkCIDR.
	// HostPrefix is ignored.
	status := configv1.NetworkStatus{}
	for _, snet := range networkConfig.Spec.ServiceNetwork {
		status.ServiceNetwork = append(status.ServiceNetwork, snet)
	}

	for _, cnet := range networkConfig.Spec.ClusterNetwork {
		status.ClusterNetwork = append(status.ClusterNetwork,
			configv1.ClusterNetworkEntry{
				CIDR: cnet.CIDR,
			})
	}
	status.NetworkType = networkConfig.Spec.NetworkType
	return status
}

func deleteExistingPods(c client.Client, component string) error {
	var period int64 = 0
	policy := metav1.DeletePropagationForeground
	label := map[string]string{"component": component}
	err := c.DeleteAllOf(context.TODO(), &corev1.Pod{}, client.InNamespace(operatortypes.NsxNamespace),
		client.MatchingLabels(label), client.PropagationPolicy(policy), client.GracePeriodSeconds(period))
	if err != nil {
		log.Error(err, fmt.Sprintf("Failed to delete pod %s", component))
		return err
	}
	log.Info(fmt.Sprintf("Successfully deleted pod %s", component))
	return nil
}

func (r *ReconcileConfigMap) updateSharedInfoWithNsxNcpResources(objs []*unstructured.Unstructured) {
	for _, obj := range objs {
		if obj.GetName() == operatortypes.NsxNodeAgentDsName {
			r.sharedInfo.NsxNodeAgentDsSpec = obj.DeepCopy()
		} else if obj.GetName() == operatortypes.NsxNcpBootstrapDsName {
			r.sharedInfo.NsxNcpBootstrapDsSpec = obj.DeepCopy()
		} else if obj.GetName() == operatortypes.NsxNcpDeploymentName {
			r.sharedInfo.NsxNcpDeploymentSpec = obj.DeepCopy()
		}
	}
	log.Info("Updated shared info with Nsx Ncp Resources")
}

func (r *ReconcileConfigMap) isNcpImageChanged() (bool, error) {
	ncpDeployment := &appsv1.Deployment{}
	err := r.client.Get(context.TODO(), types.NamespacedName{Namespace: operatortypes.NsxNamespace, Name: operatortypes.NsxNcpDeploymentName},
		ncpDeployment)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	prevImage := ncpDeployment.Spec.Template.Spec.Containers[0].Image
	currImage := os.Getenv(operatortypes.NcpImageEnv)
	if prevImage != currImage {
		return true, nil
	}
	return false, nil
}
