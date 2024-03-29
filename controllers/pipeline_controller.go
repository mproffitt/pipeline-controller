package controllers

import (
	"context"
	"fmt"

	clusterctrlv1alpha1 "github.com/weaveworks/cluster-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/weaveworks/pipeline-controller/api/v1alpha1"
	"github.com/weaveworks/pipeline-controller/pkg/conditions"
)

// PipelineReconciler reconciles a Pipeline object
type PipelineReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	targetScheme   *runtime.Scheme
	ControllerName string
	recorder       record.EventRecorder
}

func NewPipelineReconciler(
	c client.Client,
	s *runtime.Scheme,
	controllerName string,
) *PipelineReconciler {
	targetScheme := runtime.NewScheme()

	return &PipelineReconciler{
		Client:         c,
		Scheme:         s,
		targetScheme:   targetScheme,
		ControllerName: controllerName,
	}
}

//+kubebuilder:rbac:groups=pipelines.weave.works,resources=pipelines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=pipelines.weave.works,resources=pipelines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=pipelines.weave.works,resources=pipelines/finalizers,verbs=update
//+kubebuilder:rbac:groups=gitops.weave.works,resources=gitopsclusters,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

func (r *PipelineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.Info("starting reconciliation")

	var pipeline v1alpha1.Pipeline
	if err := r.Get(ctx, req.NamespacedName, &pipeline); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Examine if the object is under deletion
	if !pipeline.ObjectMeta.DeletionTimestamp.IsZero() {
		// Stop reconciliation as the object is being deleted
		return ctrl.Result{}, nil
	}

	for _, env := range pipeline.Spec.Environments {
		for _, target := range env.Targets {
			// check cluster only if ref is defined
			if target.ClusterRef != nil {
				cluster, err := r.getCluster(ctx, pipeline, *target.ClusterRef)
				if err != nil {

					// emit the event whatever problem there was
					r.emitEventf(
						&pipeline,
						corev1.EventTypeWarning,
						"GetClusterError", "Failed to get cluster %s/%s for pipeline %s/%s: %s",
						target.ClusterRef.Namespace, target.ClusterRef.Name,
						pipeline.GetNamespace(), pipeline.GetName(),
						err,
					)

					// not found -- fine, maybe things are happening out of order; make a note and wait until the cluster exists (or something else happens).
					if apierrors.IsNotFound(err) {
						if err := r.setStatusCondition(ctx, pipeline, fmt.Sprintf("Target cluster '%s' not found", target.ClusterRef.String()),
							v1alpha1.TargetClusterNotFoundReason); err != nil {
							r.emitEventf(
								&pipeline,
								corev1.EventTypeWarning,
								"SetStatusConditionError", "Failed to set status for pipeline %s/%s: %s",
								pipeline.GetNamespace(), pipeline.GetName(),
								err,
							)
							return ctrl.Result{}, err
						}
						// do not requeue immediately, when the cluster is created the watcher should trigger a reconciliation
						return ctrl.Result{RequeueAfter: v1alpha1.DefaultRequeueInterval}, nil
					}

					// some other error -- this _is_ unexpected, so return it to controller-runtime.
					return ctrl.Result{}, err
				}

				if !conditions.IsReady(cluster.Status.Conditions) {
					err := r.setStatusCondition(
						ctx, pipeline,
						fmt.Sprintf("Target cluster '%s' not ready", target.ClusterRef.String()),
						v1alpha1.TargetClusterNotReadyReason,
					)
					if err != nil {
						r.emitEventf(
							&pipeline,
							corev1.EventTypeWarning,
							"SetStatusConditionError", "Failed to set status for pipeline %s/%s: %s",
							pipeline.GetNamespace(), pipeline.GetName(),
							err,
						)
						return ctrl.Result{}, err
					}
					// do not requeue immediately, when the cluster is created the watcher should trigger a reconciliation
					return ctrl.Result{RequeueAfter: v1alpha1.DefaultRequeueInterval}, nil
				}
			}
		}
	}

	newCondition := metav1.Condition{
		Type:    conditions.ReadyCondition,
		Status:  metav1.ConditionTrue,
		Reason:  v1alpha1.ReconciliationSucceededReason,
		Message: trimString("All clusters checked", v1alpha1.MaxConditionMessageLength),
	}
	pipeline.Status.ObservedGeneration = pipeline.Generation
	apimeta.SetStatusCondition(&pipeline.Status.Conditions, newCondition)
	if err := r.patchStatus(ctx, client.ObjectKeyFromObject(&pipeline), pipeline.Status); err != nil {
		r.emitEventf(
			&pipeline,
			corev1.EventTypeWarning,
			"SetStatus", "Failed to patch status for pipeline %s/%s: %s",
			pipeline.GetNamespace(), pipeline.GetName(),
			err,
		)
		return ctrl.Result{Requeue: true}, err
	}
	r.emitEventf(
		&pipeline,
		corev1.EventTypeNormal,
		"Updated", "Updated pipeline %s/%s",
		pipeline.GetNamespace(), pipeline.GetName(),
	)

	return ctrl.Result{}, nil
}

func (r *PipelineReconciler) setStatusCondition(ctx context.Context, p v1alpha1.Pipeline, msg, reason string) error {
	newCondition := metav1.Condition{
		Type:    conditions.ReadyCondition,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: trimString(msg, v1alpha1.MaxConditionMessageLength),
	}
	p.Status.ObservedGeneration = p.Generation
	apimeta.SetStatusCondition(&p.Status.Conditions, newCondition)
	if err := r.patchStatus(ctx, client.ObjectKeyFromObject(&p), p.Status); err != nil {
		return fmt.Errorf("failed patching Pipeline: %w", err)
	}
	return nil
}

func (r *PipelineReconciler) patchStatus(ctx context.Context, n types.NamespacedName, newStatus v1alpha1.PipelineStatus) error {
	var pipeline v1alpha1.Pipeline
	if err := r.Get(ctx, n, &pipeline); err != nil {
		return err
	}

	patch := client.MergeFrom(pipeline.DeepCopy())
	pipeline.Status = newStatus
	return r.Status().Patch(ctx, &pipeline, patch, client.FieldOwner(r.ControllerName))
}

func (r *PipelineReconciler) getCluster(ctx context.Context, p v1alpha1.Pipeline, clusterRef v1alpha1.CrossNamespaceClusterReference) (*clusterctrlv1alpha1.GitopsCluster, error) {
	cluster := &clusterctrlv1alpha1.GitopsCluster{}
	namespace := clusterRef.Namespace
	if clusterRef.Namespace == "" {
		namespace = p.Namespace
	}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: clusterRef.Name}, cluster); err != nil {
		return nil, err
	}
	return cluster, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PipelineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	const (
		gitopsClusterIndexKey string = ".spec.environment.ClusterRef" // this is arbitrary, but let's make it suggest what it's indexing.
	)
	// Index the Pipelines by the GitopsCluster references they (may) point at.
	if err := mgr.GetCache().IndexField(context.TODO(), &v1alpha1.Pipeline{}, gitopsClusterIndexKey,
		r.indexClusterKind("GitopsCluster")); err != nil {
		return fmt.Errorf("failed setting index fields: %w", err)
	}

	if r.recorder == nil {
		r.recorder = mgr.GetEventRecorderFor(r.ControllerName)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Pipeline{}).
		Watches(
			&clusterctrlv1alpha1.GitopsCluster{},
			handler.EnqueueRequestsFromMapFunc(r.requestsForCluster(gitopsClusterIndexKey)),
		).
		Complete(r)
}

func (r *PipelineReconciler) emitEventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	if r.recorder == nil {
		return
	}

	r.recorder.Eventf(object, eventtype, reason, messageFmt, args...)
}

func trimString(str string, limit int) string {
	if len(str) <= limit {
		return str
	}

	return str[0:limit] + "..."
}
