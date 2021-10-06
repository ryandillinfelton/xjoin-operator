package controllers

import (
	"context"
	"github.com/go-errors/errors"
	"github.com/go-logr/logr"
	xjoin "github.com/redhatinsights/xjoin-operator/api/v1alpha1"
	"github.com/redhatinsights/xjoin-operator/controllers/common"
	"github.com/redhatinsights/xjoin-operator/controllers/components"
	"github.com/redhatinsights/xjoin-operator/controllers/config"
	. "github.com/redhatinsights/xjoin-operator/controllers/index"
	xjoinlogger "github.com/redhatinsights/xjoin-operator/controllers/log"
	"github.com/redhatinsights/xjoin-operator/controllers/parameters"
	"github.com/redhatinsights/xjoin-operator/controllers/utils"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"time"
)

const xjoinindexpipelineFinalizer = "finalizer.xjoin.indexpipeline.cloud.redhat.com"

type XJoinIndexPipelineReconciler struct {
	Client    client.Client
	Log       logr.Logger
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	Namespace string
	Test      bool
}

func NewXJoinIndexPipelineReconciler(
	client client.Client,
	scheme *runtime.Scheme,
	log logr.Logger,
	recorder record.EventRecorder,
	namespace string,
	isTest bool) *XJoinIndexPipelineReconciler {

	return &XJoinIndexPipelineReconciler{
		Client:    client,
		Log:       log,
		Scheme:    scheme,
		Recorder:  recorder,
		Namespace: namespace,
		Test:      isTest,
	}
}

func (r *XJoinIndexPipelineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("xjoin-indexpipeline-controller").
		For(&xjoin.XJoinIndexPipeline{}).
		WithLogger(mgr.GetLogger()).
		WithOptions(controller.Options{
			Log: mgr.GetLogger(),
		}).
		Complete(r)
}

// +kubebuilder:rbac:groups=xjoin.cloud.redhat.com,resources=xjoinindexpipelines;xjoinindexpipelines/status;xjoinindexpipelines/finalizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kafka.strimzi.io,resources=kafkaconnectors;kafkaconnectors/finalizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kafka.strimzi.io,resources=kafkatopics;kafkatopics/finalizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kafka.strimzi.io,resources=kafkaconnects;kafkas,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps;pods,verbs=get;list;watch

func (r *XJoinIndexPipelineReconciler) Reconcile(ctx context.Context, request ctrl.Request) (result ctrl.Result, err error) {
	reqLogger := xjoinlogger.NewLogger("controller_xjoinindexpipeline", "IndexPipeline", request.Name, "Namespace", request.Namespace)
	reqLogger.Info("Reconciling XJoinIndexPipeline")

	instance, err := utils.FetchXJoinIndexPipeline(r.Client, request.NamespacedName, ctx)
	if err != nil {
		if k8errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return result, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, errors.Wrap(err, 0)
	}

	p := parameters.BuildIndexParameters()

	configManager, err := config.NewManager(config.ManagerOptions{
		Client:         r.Client,
		Parameters:     p,
		ConfigMapNames: []string{"xjoin"},
		SecretNames:    nil,
		Namespace:      instance.Namespace,
		Spec:           instance.Spec,
		Context:        ctx,
	})
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, 0)
	}
	err = configManager.Parse()
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, 0)
	}

	if p.Pause.Bool() == true {
		return reconcile.Result{}, errors.Wrap(err, 0)
	}

	i := XJoinIndexPipelineIteration{
		Parameters: *p,
		Iteration: common.Iteration{
			Context:          ctx,
			Instance:         instance,
			OriginalInstance: instance.DeepCopy(),
			Client:           r.Client,
			Log:              reqLogger,
		},
	}

	if err = i.AddFinalizer(xjoinindexpipelineFinalizer); err != nil {
		return reconcile.Result{}, errors.Wrap(err, 0)
	}

	avroSchemaReferences, err := i.ParseAvroSchemaReferences()
	if err != nil {
		return result, errors.Wrap(err, 0)
	}

	componentManager := components.NewComponentManager(instance.Kind+"."+instance.Spec.Name, p.Version.String())
	componentManager.AddComponent(components.NewElasticsearchConnector())
	componentManager.AddComponent(components.NewElasticsearchIndex())
	componentManager.AddComponent(components.NewAvroSchema(i.GetInstance().Spec.AvroSchema, avroSchemaReferences))

	if instance.GetDeletionTimestamp() != nil {
		reqLogger.Info("Starting finalizer")
		err = componentManager.DeleteAll()
		if err != nil {
			reqLogger.Error(err, "error deleting components during finalizer")
			return
		}

		controllerutil.RemoveFinalizer(instance, xjoinindexpipelineFinalizer)
		ctx, cancel := utils.DefaultContext()
		defer cancel()
		err = r.Client.Update(ctx, instance)
		if err != nil {
			return reconcile.Result{}, errors.Wrap(err, 0)
		}

		reqLogger.Info("Successfully finalized")
		return reconcile.Result{}, nil
	}

	err = componentManager.CreateAll()
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, 0)
	}

	return reconcile.Result{RequeueAfter: time.Second * 30}, nil
}