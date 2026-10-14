/*
Copyright 2026.

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
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cachev1alpha1 "example.com/cache-operator/api/v1alpha1"
)

var _ = Describe("CacheCluster Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		cachecluster := &cachev1alpha1.CacheCluster{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind CacheCluster")
			err := k8sClient.Get(ctx, typeNamespacedName, cachecluster)
			if err != nil && errors.IsNotFound(err) {
				resource := &cachev1alpha1.CacheCluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					// TODO(user): Specify other spec details if needed.
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &cachev1alpha1.CacheCluster{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance CacheCluster")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &CacheClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})

		It("should create StatefulSet and Service", func() {
			// 使用显式 Spec 验证 Reconcile 会把 CacheCluster 转换为两个子资源。
			cacheCluster := &cachev1alpha1.CacheCluster{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, cacheCluster)).To(Succeed())
			cacheCluster.Spec.Replicas = 2
			cacheCluster.Spec.Image = "redis:7"
			cacheCluster.Spec.Storage.Size = resource.MustParse("1Gi")
			Expect(k8sClient.Update(ctx, cacheCluster)).To(Succeed())

			// 直接调用 Reconcile，隔离控制器逻辑，避免依赖后台 Manager。
			controllerReconciler := &CacheClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// 断言 StatefulSet 已创建，并检查副本数和镜像是否来自 CacheCluster.Spec。
			statefulSet := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, statefulSet)).To(Succeed())
			Expect(*statefulSet.Spec.Replicas).To(Equal(int32(2)))
			Expect(statefulSet.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(statefulSet.Spec.Template.Spec.Containers[0].Image).To(Equal("redis:7"))

			// 断言 Service 已创建，并检查它通过 Selector 关联缓存 Pod。
			service := &corev1.Service{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, service)).To(Succeed())
			Expect(service.Spec.Selector["cache.example.com/cluster"]).To(Equal(cacheCluster.Name))
			Expect(service.Spec.Ports).To(HaveLen(1))
			Expect(service.Spec.Ports[0].Port).To(Equal(int32(6379)))

			// 断言两个子资源都由当前 CacheCluster 控制，删除主资源时可以级联清理。
			Expect(metav1.IsControlledBy(statefulSet, cacheCluster)).To(BeTrue())
			Expect(metav1.IsControlledBy(service, cacheCluster)).To(BeTrue())
		})

		It("should update StatefulSet replicas when CacheCluster replicas change", func() {
			// 先读取测试用的 CacheCluster，并使用默认配置创建初始 StatefulSet。
			cacheCluster := &cachev1alpha1.CacheCluster{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, cacheCluster)).To(Succeed())
			controllerReconciler := &CacheClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Reconcile 更新 Status 后会产生新的 ResourceVersion，修改 Spec 前重新读取最新对象。
			Expect(k8sClient.Get(ctx, typeNamespacedName, cacheCluster)).To(Succeed())

			// 修改 CacheCluster 的期望副本数，模拟用户更新 CR 的 Spec。
			cacheCluster.Spec.Replicas = 3
			Expect(k8sClient.Update(ctx, cacheCluster)).To(Succeed())

			// 再次执行 Reconcile，让 StatefulSet 收敛到新的副本数。
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// 读取实际 StatefulSet，并断言副本数已经从初始值更新为 3。
			statefulSet := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, statefulSet)).To(Succeed())
			Expect(statefulSet.Spec.Replicas).NotTo(BeNil())
			Expect(*statefulSet.Spec.Replicas).To(Equal(int32(3)))
		})
	})
})

// TestDesiredStatefulSet verifies that CacheCluster Spec is converted into the expected StatefulSet.
func TestDesiredStatefulSet(t *testing.T) {
	// 准备一个包含副本数、镜像、存储和 StorageClass 配置的 CacheCluster。
	storageClassName := "local-path"
	cacheCluster := &cachev1alpha1.CacheCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo-cache",
			Namespace: "default",
		},
		Spec: cachev1alpha1.CacheClusterSpec{
			Replicas: 3,
			Image:    "redis:7",
			Storage: cachev1alpha1.StorageSpec{
				Size:             resource.MustParse("1Gi"),
				StorageClassName: &storageClassName,
			},
		},
	}

	// 调用纯构造方法；该方法不访问 Kubernetes API，因此测试不依赖集群。
	reconciler := &CacheClusterReconciler{}
	statefulSet := reconciler.desiredStatefulSet(cacheCluster)

	// 校验 StatefulSet 的基础元数据和服务名称。
	if statefulSet.Name != "demo-cache" {
		t.Fatalf("expected StatefulSet name demo-cache, got %q", statefulSet.Name)
	}
	if statefulSet.Namespace != "default" {
		t.Fatalf("expected StatefulSet namespace default, got %q", statefulSet.Namespace)
	}
	if statefulSet.Spec.ServiceName != "demo-cache" {
		t.Fatalf("expected service name demo-cache, got %q", statefulSet.Spec.ServiceName)
	}

	// 校验副本数、容器镜像和缓存端口。
	if statefulSet.Spec.Replicas == nil || *statefulSet.Spec.Replicas != 3 {
		t.Fatalf("expected 3 replicas, got %d", *statefulSet.Spec.Replicas)
	}
	if len(statefulSet.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(statefulSet.Spec.Template.Spec.Containers))
	}

	container := statefulSet.Spec.Template.Spec.Containers[0]
	if container.Name != "cache" {
		t.Fatalf("expected container name cache, got %q", container.Name)
	}
	if container.Image != "redis:7" {
		t.Fatalf("expected image redis:7, got %q", container.Image)
	}
	if len(container.VolumeMounts) != 1 || container.VolumeMounts[0].Name != "data" || container.VolumeMounts[0].MountPath != "/data" {
		t.Fatalf("expected data volume mounted at /data, got %#v", container.VolumeMounts)
	}
	if len(container.Ports) != 1 || container.Ports[0].ContainerPort != 6379 {
		t.Fatalf("expected one container port 6379, got %#v", container.Ports)
	}

	// 校验 StatefulSet Selector 与 Pod Labels 使用同一组标签。
	if statefulSet.Spec.Selector == nil {
		t.Fatal("expected StatefulSet selector to be configured")
	}
	if statefulSet.Spec.Selector.MatchLabels["cache.example.com/cluster"] != "demo-cache" {
		t.Fatalf("expected selector to target demo-cache, got %#v", statefulSet.Spec.Selector.MatchLabels)
	}
	if statefulSet.Spec.Template.Labels["cache.example.com/cluster"] != "demo-cache" {
		t.Fatalf("expected pod label to target demo-cache, got %#v", statefulSet.Spec.Template.Labels)
	}

	// 校验每个缓存实例都有名为 data 的 PVC 模板和 1Gi 存储请求。
	if len(statefulSet.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("expected 1 PVC template, got %d", len(statefulSet.Spec.VolumeClaimTemplates))
	}
	volumeClaim := statefulSet.Spec.VolumeClaimTemplates[0]
	if volumeClaim.Name != "data" {
		t.Fatalf("expected PVC template name data, got %q", volumeClaim.Name)
	}
	if volumeClaim.Spec.StorageClassName == nil || *volumeClaim.Spec.StorageClassName != "local-path" {
		t.Fatalf("expected StorageClass local-path, got %v", volumeClaim.Spec.StorageClassName)
	}
	storageRequest := volumeClaim.Spec.Resources.Requests["storage"]
	if storageRequest.String() != "1Gi" {
		t.Fatalf("expected storage request 1Gi, got %s", storageRequest.String())
	}
}

func TestDesiredService(t *testing.T) {
	// 准备一个用于生成 Service 的 CacheCluster。
	cacheCluster := &cachev1alpha1.CacheCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo-cache",
			Namespace: "default",
		},
	}

	// 调用纯构造方法；该方法只根据 CR 内容生成 Service，不访问 Kubernetes API。
	reconciler := &CacheClusterReconciler{}
	service := reconciler.desiredService(cacheCluster)

	// 校验 Service 的名称和 Namespace，确保它与 StatefulSet 使用相同的标识。
	if service.Name != "demo-cache" {
		t.Fatalf("expected Service name demo-cache, got %q", service.Name)
	}
	if service.Namespace != "default" {
		t.Fatalf("expected Service namespace default, got %q", service.Namespace)
	}

	// 校验 Service Selector 能匹配 desiredStatefulSet 设置的 Pod 标签。
	expectedClusterLabel := service.Spec.Selector["cache.example.com/cluster"]
	if expectedClusterLabel != "demo-cache" {
		t.Fatalf("expected cluster selector demo-cache, got %q", expectedClusterLabel)
	}
	if service.Spec.Selector["app.kubernetes.io/instance"] != "demo-cache" {
		t.Fatalf("expected instance selector demo-cache, got %q", service.Spec.Selector["app.kubernetes.io/instance"])
	}
	if service.Spec.Selector["app.kubernetes.io/managed-by"] != "cache-operator" {
		t.Fatalf("expected managed-by selector cache-operator, got %q", service.Spec.Selector["app.kubernetes.io/managed-by"])
	}

	// 校验 Service 暴露缓存容器使用的 6379 端口。
	if len(service.Spec.Ports) != 1 {
		t.Fatalf("expected 1 Service port, got %d", len(service.Spec.Ports))
	}
	if service.Spec.Ports[0].Name != "cache" {
		t.Fatalf("expected Service port name cache, got %q", service.Spec.Ports[0].Name)
	}
	if service.Spec.Ports[0].Port != 6379 {
		t.Fatalf("expected Service port 6379, got %d", service.Spec.Ports[0].Port)
	}
}

// TestDesiredRedisClusterInitJob verifies the Job command used to initialize Redis Cluster.
func TestDesiredRedisClusterInitJob(t *testing.T) {
	// 准备一个包含 6 个实例的 RedisCluster，形成 3 个 Master 和 3 个 Replica。
	cacheCluster := &cachev1alpha1.CacheCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo-cache",
			Namespace: "default",
		},
		Spec: cachev1alpha1.CacheClusterSpec{
			Mode:     "RedisCluster",
			Replicas: 6,
		},
	}

	// 构造初始化 Job；该方法不访问 Kubernetes API。
	reconciler := &CacheClusterReconciler{}
	job := reconciler.desiredRedisClusterInitJob(cacheCluster)

	// 校验 Job 的名称、Namespace、镜像和重启策略。
	if job.Name != "demo-cache-cluster-init" {
		t.Fatalf("expected Job name demo-cache-cluster-init, got %q", job.Name)
	}
	if job.Namespace != "default" {
		t.Fatalf("expected Job namespace default, got %q", job.Namespace)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyOnFailure {
		t.Fatalf("expected OnFailure restart policy, got %q", job.Spec.Template.Spec.RestartPolicy)
	}
	if len(job.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected 1 Job container, got %d", len(job.Spec.Template.Spec.Containers))
	}
	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != "redis:7" {
		t.Fatalf("expected redis:7 image, got %q", container.Image)
	}

	// 校验初始化命令包含 6 个稳定 StatefulSet Pod DNS 地址。
	if len(container.Args) != 1 {
		t.Fatalf("expected one initialization command, got %d", len(container.Args))
	}
	command := container.Args[0]
	if !strings.Contains(command, "redis-cli --cluster create") {
		t.Fatalf("expected redis cluster create command, got %q", command)
	}
	if !strings.Contains(command, "--cluster-replicas 1 --cluster-yes") {
		t.Fatalf("expected replica initialization flags, got %q", command)
	}
	for index := 0; index < 6; index++ {
		node := "demo-cache-" + string(rune('0'+index)) + ".demo-cache.default.svc.cluster.local:6379"
		if !strings.Contains(command, node) {
			t.Fatalf("expected node %s in initialization command, got %q", node, command)
		}
	}

}

// TestSetCacheClusterConditions verifies the condition set for Pending and Running phases.
func TestSetCacheClusterConditions(t *testing.T) {
	tests := []struct {
		name        string
		phase       string
		ready       metav1.ConditionStatus
		progressing metav1.ConditionStatus
		degraded    metav1.ConditionStatus
	}{
		{
			name:        "pending",
			phase:       "Pending",
			ready:       metav1.ConditionFalse,
			progressing: metav1.ConditionTrue,
			degraded:    metav1.ConditionFalse,
		},
		{
			name:        "running",
			phase:       "Running",
			ready:       metav1.ConditionTrue,
			progressing: metav1.ConditionFalse,
			degraded:    metav1.ConditionFalse,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// 准备指定 Phase 和 Generation 的 CacheCluster。
			cacheCluster := &cachev1alpha1.CacheCluster{}
			cacheCluster.Generation = 2
			cacheCluster.Status.Phase = test.phase

			// 根据当前 Phase 计算标准 Conditions。
			setCacheClusterConditions(cacheCluster)

			// 断言三类 Condition 的状态和版本都正确。
			assertConditionStatus(t, cacheCluster.Status.Conditions, "Ready", test.ready)
			assertConditionStatus(t, cacheCluster.Status.Conditions, "Progressing", test.progressing)
			assertConditionStatus(t, cacheCluster.Status.Conditions, "Degraded", test.degraded)
			for _, condition := range cacheCluster.Status.Conditions {
				if condition.ObservedGeneration != 2 {
					t.Fatalf("condition %q has observed generation %d, expected 2", condition.Type, condition.ObservedGeneration)
				}
			}
		})
	}
}

// assertConditionStatus finds a condition by type and verifies its status.
func assertConditionStatus(t *testing.T, conditions []metav1.Condition, conditionType string, expected metav1.ConditionStatus) {
	t.Helper()
	condition := apiMeta.FindStatusCondition(conditions, conditionType)
	if condition == nil {
		t.Fatalf("condition %q was not found", conditionType)
	}
	if condition.Status != expected {
		t.Fatalf("condition %q has status %q, expected %q", conditionType, condition.Status, expected)
	}
}
