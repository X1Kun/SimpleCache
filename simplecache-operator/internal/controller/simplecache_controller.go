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
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	cachev1 "x1kun.com/simplecache-operator/api/v1"
)

// SimpleCacheReconciler reconciles a SimpleCache object
type SimpleCacheReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=cache.x1kun.com,resources=simplecaches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cache.x1kun.com,resources=simplecaches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cache.x1kun.com,resources=simplecaches/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the SimpleCache object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.1/pkg/reconcile
func (r *SimpleCacheReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// 1. 初始化日志记录器
	log := log.FromContext(ctx)

	// 2. 去K8s数据库里，取出SimpleCache的CRD
	var cacheResource cachev1.SimpleCache
	if err := r.Get(ctx, req.NamespacedName, &cacheResource); err != nil {
		// 找不到此资源，直接返回，不报错
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 3. 打印日志，证明operator成功进行
	log.Info("Reconcile succeeded",
		"集群名字", cacheResource.Name,
		"期望的节点数量(Size)", cacheResource.Spec.Size,
		"使用的镜像(Image)", cacheResource.Spec.Image,
	)

	// 4. 动态生成并拼接 peers 字符串
	// 例如 size=3 时，拼接出：http://name-0.name-svc...:8001,http://name-1.name-svc...:8001...
	var peers []string
	svcName := cacheResource.Name + "-svc" // 假定我们配套的 Service 叫这个名字
	for i := int32(0); i < cacheResource.Spec.Size; i++ {
		peerURL := fmt.Sprintf("http://%s-%d.%s.%s.svc.cluster.local:8001",
			cacheResource.Name, i, svcName, cacheResource.Namespace)
		peers = append(peers, peerURL)
	}
	peersStr := strings.Join(peers, ",")

	labels := map[string]string{"app": cacheResource.Name}

	// 5. 在内存里创建 Headless Service
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: cacheResource.Namespace,
		},
		Spec: corev1.ServiceSpec{
			// 无头服务
			ClusterIP: "None",
			Selector:  labels,
			Ports: []corev1.ServicePort{
				{Name: "peer", Port: 8001},
				{Name: "api", Port: 9999},
			},
		},
	}
	// 对这个 Service 建立 owner 映射
	ctrl.SetControllerReference(&cacheResource, svc, r.Scheme)

	// 查看 Service 是否存在，不存在则创建 Headless Service
	foundSvc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}, foundSvc)
	if err != nil && apierrors.IsNotFound(err) {
		log.Info("不存在 Service，开始自动创建网络！", "Name", svc.Name)
		if err = r.Create(ctx, svc); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 6. 在内存里创建 StatefulSet
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cacheResource.Name,
			Namespace: cacheResource.Namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &cacheResource.Spec.Size,
			// 绑定无头服务
			ServiceName: svcName,
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "cache",
						Image:   cacheResource.Spec.Image,
						Command: []string{"./geecache-server", "-port=8001", "-api=1"},
						Env: []corev1.EnvVar{
							{
								Name: "POD_NAME",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
								},
							},
							{
								Name:  "SELF_ADDR",
								Value: fmt.Sprintf("http://$(POD_NAME).%s.%s.svc.cluster.local:8001", svcName, cacheResource.Namespace),
							},
							{
								Name:  "PEERS",
								Value: peersStr, // 动态计算的 peers
							},
						},
					}},
				},
			},
		},
	}

	// 7. 建立owner映射：这个 StatefulSet 是与 SimpleCache 挂钩的。如果 SimpleCache 被删了，K8s 会自动把这个 StatefulSet 也删掉（级联删除）
	if err = ctrl.SetControllerReference(&cacheResource, sts, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	// 8. K8s 检查 StatefulSet 是否被创建
	foundSts := &appsv1.StatefulSet{}
	err = r.Get(ctx, types.NamespacedName{Name: sts.Name, Namespace: sts.Namespace}, foundSts)

	if err != nil && apierrors.IsNotFound(err) {
		// 如果没有，立刻向 K8s 发送请求创建它
		log.Info("目前不存在此 StatefulSet，开始自动创建！", "Name", sts.Name)
		err = r.Create(ctx, sts)
		if err != nil {
			return ctrl.Result{}, err
		}
		// 创建成功
		return ctrl.Result{}, nil
	} else if err != nil {
		// 发生了网络错误等其他意外
		return ctrl.Result{}, err
	}

	var existingPeers string
	for _, env := range foundSts.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "PEERS" {
			existingPeers = env.Value
			break
		}
	}
	existingImage := foundSts.Spec.Template.Spec.Containers[0].Image

	// 9. 检查 StatefulSet 和现在的 cache 是否匹配
	if *foundSts.Spec.Replicas != cacheResource.Spec.Size || existingPeers != peersStr || existingImage != cacheResource.Spec.Image {
		log.Info("检测到配置或镜像差异，开始执行网络拓扑更新或滚动发布！", "当前镜像", existingImage, "期望镜像", cacheResource.Spec.Image)
		// 更新 StatefulSet 的副本数量
		foundSts.Spec.Replicas = &cacheResource.Spec.Size
		// 更新 StatefulSet 的镜像版本
		foundSts.Spec.Template.Spec.Containers[0].Image = cacheResource.Spec.Image
		// 更新 StatefulSet 的 peer 环境变量
		for i, env := range foundSts.Spec.Template.Spec.Containers[0].Env {
			if env.Name == "PEERS" {
				// peersStr 是我们在第 2 步 for 循环里最新算出来的 5 个节点的字符串
				foundSts.Spec.Template.Spec.Containers[0].Env[i].Value = peersStr
				break
			}
		}
		err = r.Update(ctx, foundSts)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	// 返回成功，等待下一次风吹草动
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SimpleCacheReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cachev1.SimpleCache{}).
		Named("simplecache").
		Complete(r)
}
