/*
Copyright 2026.

	// 使用稳定标签连接 StatefulSet selector、Pod template、Service 和后续关联资源。
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
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cachev1alpha1 "example.com/cache-operator/api/v1alpha1"
)

// CacheClusterReconciler reconciles a CacheCluster object
type CacheClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}
const cacheClusterFinalizer = "cache.example.com/finalizer"
// +kubebuilder:rbac:groups=cache.example.com,resources=cacheclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cache.example.com,resources=cacheclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cache.example.com,resources=cacheclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//
// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the CacheCluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.1/pkg/reconcile
func (r *CacheClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var cacheCluster cachev1alpha1.CacheCluster
	if err := r.Get(ctx, req.NamespacedName, &cacheCluster); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("CacheCluster resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Finalizer logic: if the CacheCluster is marked for deletion, perform cleanup.
	logger.Info("CacheCluster marked for deletion, performing cleanup")
	if !cacheCluster.DeletionTimestamp.IsZero() {
		if err := r.cleanupCacheCluster(ctx, &cacheCluster); err != nil {
			return ctrl.Result{}, err
		}

		// Remove finalizer to allow deletion to proceed.
		logger.Info("Removing CacheCluster finalizer")
		controllerutil.RemoveFinalizer(&cacheCluster, cacheClusterFinalizer)
		if err := r.Update(ctx, &cacheCluster); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("CacheCluster finalizer removed, resource can be deleted")
		return ctrl.Result{}, nil
	}

	// Add finalizer if it doesn't exist yet. This ensures that the cleanup logic is executed before the resource is deleted.
	logger.Info("Adding CacheCluster finalizer if not present")
	if !controllerutil.ContainsFinalizer(&cacheCluster, cacheClusterFinalizer) {
		logger.Info("Adding CacheCluster finalizer")
		controllerutil.AddFinalizer(&cacheCluster, cacheClusterFinalizer)
		if err := r.Update(ctx, &cacheCluster); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	logger.Info("StartingReconciling CacheCluster", "namespace", cacheCluster.Namespace, "name", cacheCluster.Name)

	// Redis Cluster 使用 1 个副本副本策略时，至少需要 3 个 Master 和 3 个 Replica。
	if cacheCluster.Spec.Mode == "RedisCluster" && cacheCluster.Spec.Replicas < 6 {
		return ctrl.Result{}, fmt.Errorf("redis cluster mode requires at least 6 replicas, got %d", cacheCluster.Spec.Replicas)
	}

	// 先构造期望的 StatefulSet，并设置 CacheCluster 为其控制者。
	statefulSet := r.desiredStatefulSet(&cacheCluster)
	if err := controllerutil.SetControllerReference(&cacheCluster, statefulSet, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	// 使用 CreateOrUpdate 保证 Reconcile 可重复执行，并持续收敛实际状态。
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, statefulSet, func() error {
		statefulSet.Spec = r.desiredStatefulSet(&cacheCluster).Spec
		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("StatefulSet reconciled", "name", statefulSet.Name, "namespace", statefulSet.Namespace)

	// 先构造期望的 Service，并设置 CacheCluster 为其控制者。
	service := r.desiredService(&cacheCluster)
	if err := controllerutil.SetControllerReference(&cacheCluster, service, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	// 使用 CreateOrUpdate 保证 Service 也能持续收敛。
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		service.Spec = r.desiredService(&cacheCluster).Spec
		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Service reconciled", "name", service.Name, "namespace", service.Namespace)

	// RedisCluster 模式需要额外创建一次性 Job，负责执行 redis-cli 集群初始化。
	if cacheCluster.Spec.Mode == "RedisCluster" {
		if statefulSet.Status.ReadyReplicas < cacheCluster.Spec.Replicas {
			logger.Info("Waiting for RedisCluster StatefulSet replicas before initialization")
			return ctrl.Result{}, nil
		}

		initJob := r.desiredRedisClusterInitJob(&cacheCluster)
		if err := controllerutil.SetControllerReference(&cacheCluster, initJob, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}

		// Job Spec 创建后不可直接更新；已存在时交给 Job 自身继续执行或保留完成结果。
		existingJob := &batchv1.Job{}
		err := r.Get(ctx, client.ObjectKeyFromObject(initJob), existingJob)
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, initJob); err != nil {
				return ctrl.Result{}, err
			}
			logger.Info("RedisCluster initialization Job created", "name", initJob.Name, "namespace", initJob.Namespace)
		} else if err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.updateCacheClusterStatus(ctx, &cacheCluster); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("CacheCluster status updated", "name", cacheCluster.Name, "namespace", cacheCluster.Namespace)

	return ctrl.Result{}, nil
}

// desiredStatefulSet builds the StatefulSet that represents one CacheCluster.
func (r *CacheClusterReconciler) desiredStatefulSet(cacheCluster *cachev1alpha1.CacheCluster) *appsv1.StatefulSet {
	// 使用稳定标签连接 StatefulSet selector、Pod template 和后续关联资源。
	labels := map[string]string{
		"app.kubernetes.io/name":       "cache-operator",
		"app.kubernetes.io/instance":   cacheCluster.Name,
		"app.kubernetes.io/managed-by": "cache-operator",
		"cache.example.com/cluster":    cacheCluster.Name,
	}
	// CR 通过 API Server 创建时会应用默认值；这里保留代码层默认值，便于单元测试和直接调用。
	image := cacheCluster.Spec.Image
	if image == "" {
		image = "redis:7"
	}
	// Standalone 和 RedisCluster 共用 StatefulSet 基础结构，差异只体现在容器启动参数。
	replicas := cacheCluster.Spec.Replicas
	if replicas < 1 {
		replicas = 1
	}
	storageSize := cacheCluster.Spec.Storage.Size
	if storageSize.IsZero() {
		storageSize = resource.MustParse("1Gi")
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cacheCluster.Name,
			Namespace: cacheCluster.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: cacheCluster.Name,
			Replicas:    &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			PodManagementPolicy: appsv1.ParallelPodManagement,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:      "cache",
							Image:     image,
							Args:      redisContainerArgs(cacheCluster.Spec.Mode),
							Resources: cacheCluster.Spec.Resources,
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "data",
									MountPath: "/data",
								},
							},
							Ports: []corev1.ContainerPort{{
								Name:          "cache",
								ContainerPort: 6379,
								Protocol:      corev1.ProtocolTCP,
							}},
						},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "data", Labels: labels},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						StorageClassName: cacheCluster.Spec.Storage.StorageClassName,
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceStorage: storageSize},
						},
					},
				},
			},
		},
	}
}

// redisContainerArgs returns the Redis startup arguments for the selected mode.
func redisContainerArgs(mode string) []string {
	if mode != "RedisCluster" {
		return nil
	}

	// RedisCluster 模式要求节点持久化 nodes.conf，集群初始化由后续 Job 负责。
	return []string{
		"--cluster-enabled", "yes",
		"--cluster-config-file", "/data/nodes.conf",
		"--cluster-node-timeout", "5000",
		"--appendonly", "yes",
	}
}

// desiredRedisClusterInitJob builds the one-shot Job that initializes Redis Cluster slots and replicas.
func (r *CacheClusterReconciler) desiredRedisClusterInitJob(cacheCluster *cachev1alpha1.CacheCluster) *batchv1.Job {
	labels := map[string]string{
		"app.kubernetes.io/name":       "cache-operator",
		"app.kubernetes.io/instance":   cacheCluster.Name,
		"app.kubernetes.io/managed-by": "cache-operator",
		"cache.example.com/cluster":    cacheCluster.Name,
	}

	// 使用 StatefulSet Pod 的稳定 DNS 生成 redis-cli --cluster create 参数。
	nodes := make([]string, 0, cacheCluster.Spec.Replicas)
	for index := int32(0); index < cacheCluster.Spec.Replicas; index++ {
		nodes = append(nodes, fmt.Sprintf(
			"%s-%d.%s.%s.svc.cluster.local:6379",
			cacheCluster.Name,
			index,
			cacheCluster.Name,
			cacheCluster.Namespace,
		))
	}

	command := fmt.Sprintf(
		"redis-cli --cluster create %s --cluster-replicas 1 --cluster-yes",
		strings.Join(nodes, " "),
	)
	backoffLimit := int32(6)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cacheCluster.Name + "-cluster-init",
			Namespace: cacheCluster.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						{
							Name:    "redis-cluster-init",
							Image:   "redis:7",
							Command: []string{"sh", "-c"},
							Args:    []string{command},
						},
					},
				},
			},
		},
	}
}

func (r *CacheClusterReconciler) desiredService(cacheCluster *cachev1alpha1.CacheCluster) *corev1.Service {
	labels := map[string]string{
		"app.kubernetes.io/name":       "cache-operator",
		"app.kubernetes.io/instance":   cacheCluster.Name,
		"app.kubernetes.io/managed-by": "cache-operator",
		"cache.example.com/cluster":    cacheCluster.Name,
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cacheCluster.Name,
			Namespace: cacheCluster.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 labels,
			Ports: []corev1.ServicePort{
				{
					Name: "cache",
					Port: 6379,
				},
			},
		},
	}
}

// Get Statefulset status and update cacheCluster status
func (r *CacheClusterReconciler) updateCacheClusterStatus(ctx context.Context, cacheCluster *cachev1alpha1.CacheCluster) error {
	statefulSet := &appsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: cacheCluster.Namespace, Name: cacheCluster.Name}, statefulSet); err != nil {
		return err
	}

	// 在修改 Status 前保存旧值，避免没有变化时重复调用 Status().Update。
	oldStatus := cacheCluster.Status.DeepCopy()
	if statefulSet.Spec.Replicas == nil {
		return fmt.Errorf("statefulset %s/%s has nil replicas", statefulSet.Namespace, statefulSet.Name)
	}

	cacheCluster.Status.Replicas = *statefulSet.Spec.Replicas
	cacheCluster.Status.ReadyReplicas = statefulSet.Status.ReadyReplicas

	// 根据 StatefulSet 的就绪副本数计算当前阶段。
	if cacheCluster.Status.ReadyReplicas == cacheCluster.Status.Replicas {
		cacheCluster.Status.Phase = "Running"
	} else {
		cacheCluster.Status.Phase = "Pending"
	}

	// 记录本次成功观察到的 CacheCluster Spec 版本。
	cacheCluster.Status.ObservedGeneration = cacheCluster.Generation

	// 根据当前阶段设置 Ready、Progressing 和 Degraded 三类标准 Condition。
	setCacheClusterConditions(cacheCluster)

	if equality.Semantic.DeepEqual(oldStatus, &cacheCluster.Status) {
		return nil
	}

	return r.Status().Update(ctx, cacheCluster)
}

// setCacheClusterConditions updates the standard conditions without changing transition time unnecessarily.
func setCacheClusterConditions(cacheCluster *cachev1alpha1.CacheCluster) {
	if cacheCluster.Status.Phase == "Running" {
		apiMeta.SetStatusCondition(&cacheCluster.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "StatefulSetReady",
			Message:            "All cache replicas are ready",
			ObservedGeneration: cacheCluster.Generation,
		})
		apiMeta.SetStatusCondition(&cacheCluster.Status.Conditions, metav1.Condition{
			Type:               "Progressing",
			Status:             metav1.ConditionFalse,
			Reason:             "StatefulSetReady",
			Message:            "CacheCluster is not progressing",
			ObservedGeneration: cacheCluster.Generation,
		})
		apiMeta.SetStatusCondition(&cacheCluster.Status.Conditions, metav1.Condition{
			Type:               "Degraded",
			Status:             metav1.ConditionFalse,
			Reason:             "StatefulSetReady",
			Message:            "CacheCluster has no detected degradation",
			ObservedGeneration: cacheCluster.Generation,
		})
		return
	}

	apiMeta.SetStatusCondition(&cacheCluster.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "StatefulSetNotReady",
		Message:            "One or more cache replicas are not ready",
		ObservedGeneration: cacheCluster.Generation,
	})
	apiMeta.SetStatusCondition(&cacheCluster.Status.Conditions, metav1.Condition{
		Type:               "Progressing",
		Status:             metav1.ConditionTrue,
		Reason:             "WaitingForReplicas",
		Message:            "Waiting for cache replicas to become ready",
		ObservedGeneration: cacheCluster.Generation,
	})
	apiMeta.SetStatusCondition(&cacheCluster.Status.Conditions, metav1.Condition{
		Type:               "Degraded",
		Status:             metav1.ConditionFalse,
		Reason:             "StatefulSetProgressing",
		Message:            "CacheCluster is progressing without a detected failure",
		ObservedGeneration: cacheCluster.Generation,
	})
}

// cleanupCacheCluster cleans up resources associated with the CacheCluster before it is deleted.
func (r *CacheClusterReconciler) cleanupCacheCluster(ctx context.Context, cacheCluster *cachev1alpha1.CacheCluster) error {
	// Implement the cleanup logic here, such as deleting associated StatefulSets, Services, and Jobs.
	// Return an error if any cleanup operation fails.
	logger := log.FromContext(ctx)
	logger.Info("Cleaning up CacheCluster resources", "cacheCluster", cacheCluster.Name)
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *CacheClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cachev1alpha1.CacheCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&batchv1.Job{}).
		Named("cachecluster").
		Complete(r)
}
