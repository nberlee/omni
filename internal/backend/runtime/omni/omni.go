// Copyright (c) 2024 Sidero Labs, Inc.
//
// Use of this software is governed by the Business Source License
// included in the LICENSE file.

// Package omni implements the internal service runtime.
package omni

import (
	"context"
	"fmt"
	"time"

	"github.com/cosi-project/runtime/pkg/controller"
	cosiruntime "github.com/cosi-project/runtime/pkg/controller/runtime"
	"github.com/cosi-project/runtime/pkg/controller/runtime/options"
	cosiresource "github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"go.uber.org/zap"

	"github.com/siderolabs/omni/client/api/common"
	"github.com/siderolabs/omni/client/pkg/constants"
	"github.com/siderolabs/omni/client/pkg/cosi/labels"
	omniresources "github.com/siderolabs/omni/client/pkg/omni/resources"
	"github.com/siderolabs/omni/client/pkg/omni/resources/omni"
	siderolinkresources "github.com/siderolabs/omni/client/pkg/omni/resources/siderolink"
	virtualres "github.com/siderolabs/omni/client/pkg/omni/resources/virtual"
	pkgruntime "github.com/siderolabs/omni/client/pkg/runtime"
	"github.com/siderolabs/omni/internal/backend/dns"
	"github.com/siderolabs/omni/internal/backend/resourcelogger"
	"github.com/siderolabs/omni/internal/backend/runtime"
	"github.com/siderolabs/omni/internal/backend/runtime/cosi"
	omnictrl "github.com/siderolabs/omni/internal/backend/runtime/omni/controllers/omni"
	"github.com/siderolabs/omni/internal/backend/runtime/omni/controllers/omni/etcdbackup/store"
	"github.com/siderolabs/omni/internal/backend/runtime/omni/controllers/omni/image"
	"github.com/siderolabs/omni/internal/backend/runtime/omni/validated"
	"github.com/siderolabs/omni/internal/backend/runtime/omni/virtual"
	"github.com/siderolabs/omni/internal/backend/runtime/omni/virtual/pkg/producers"
	"github.com/siderolabs/omni/internal/backend/runtime/talos"
	"github.com/siderolabs/omni/internal/backend/workloadproxy"
	"github.com/siderolabs/omni/internal/pkg/auth/actor"
	"github.com/siderolabs/omni/internal/pkg/config"
	newgroup "github.com/siderolabs/omni/internal/pkg/errgroup"
	"github.com/siderolabs/omni/internal/pkg/siderolink"
)

// Name is the runtime time.
var Name = common.Runtime_Omni.String()

// Runtime implements Omni internal runtime.
type Runtime struct {
	controllerRuntime  *cosiruntime.Runtime
	talosClientFactory *talos.ClientFactory
	storeFactory       store.Factory

	dnsService                   *dns.Service
	workloadProxyServiceRegistry *workloadproxy.ServiceRegistry
	resourceLogger               *resourcelogger.Logger

	// resource state for internal consumers
	state   state.State
	virtual *virtual.State

	logger *zap.Logger
}

// New creates a new Omni runtime.
func New(talosClientFactory *talos.ClientFactory, dnsService *dns.Service, workloadProxyServiceRegistry *workloadproxy.ServiceRegistry,
	resourceLogger *resourcelogger.Logger, linkCounterDeltaCh <-chan siderolink.LinkCounterDeltas, resourceState state.State,
	virtualState *virtual.State, metricsRegistry prometheus.Registerer, logger *zap.Logger,
) (*Runtime, error) {
	var opts []options.Option

	if !config.Config.DisableControllerRuntimeCache {
		opts = append(opts,
			safe.WithResourceCache[*omni.BackupData](),
			safe.WithResourceCache[*omni.Cluster](),
			safe.WithResourceCache[*omni.ClusterDestroyStatus](),
			safe.WithResourceCache[*omni.ClusterEndpoint](),
			safe.WithResourceCache[*omni.ClusterMachine](),
			safe.WithResourceCache[*omni.ClusterMachineConfigStatus](),
			safe.WithResourceCache[*omni.ClusterMachineIdentity](),
			safe.WithResourceCache[*omni.ClusterMachineStatus](),
			safe.WithResourceCache[*omni.ClusterMachineTalosVersion](),
			safe.WithResourceCache[*omni.ClusterStatus](),
			safe.WithResourceCache[*omni.ClusterSecrets](),
			safe.WithResourceCache[*omni.ClusterUUID](),
			safe.WithResourceCache[*omni.ControlPlaneStatus](),
			safe.WithResourceCache[*omni.EtcdBackupEncryption](),
			safe.WithResourceCache[*omni.EtcdBackupStatus](),
			safe.WithResourceCache[*omni.ExposedService](),
			safe.WithResourceCache[*omni.ImagePullRequest](),
			safe.WithResourceCache[*omni.ImagePullStatus](),
			safe.WithResourceCache[*omni.Kubeconfig](),
			safe.WithResourceCache[*omni.KubernetesStatus](),
			safe.WithResourceCache[*omni.KubernetesUpgradeStatus](),
			safe.WithResourceCache[*omni.KubernetesVersion](),
			safe.WithResourceCache[*omni.LoadBalancerConfig](),
			safe.WithResourceCache[*omni.LoadBalancerStatus](),
			safe.WithResourceCache[*omni.Machine](),
			safe.WithResourceCache[*omni.MachineConfigGenOptions](),
			safe.WithResourceCache[*omni.MachineLabels](),
			safe.WithResourceCache[*omni.MachineSet](),
			safe.WithResourceCache[*omni.MachineSetStatus](),
			safe.WithResourceCache[*omni.MachineSetNode](),
			safe.WithResourceCache[*omni.MachineStatus](),
			safe.WithResourceCache[*omni.MachineStatusSnapshot](),
			safe.WithResourceCache[*omni.SchematicConfiguration](),
			safe.WithResourceCache[*omni.TalosConfig](),
			safe.WithResourceCache[*omni.TalosExtensions](),
			safe.WithResourceCache[*omni.TalosUpgradeStatus](),
			safe.WithResourceCache[*omni.TalosVersion](),
			safe.WithResourceCache[*siderolinkresources.Config](),
			safe.WithResourceCache[*siderolinkresources.ConnectionParams](),
			safe.WithResourceCache[*siderolinkresources.Link](),
			options.WithWarnOnUncachedReads(false), // turn this to true to debug resource cache misses
		)
	}

	controllerRuntime, err := cosiruntime.NewRuntime(resourceState, logger, opts...)
	if err != nil {
		return nil, err
	}

	storeFactory, err := store.NewStoreFactory()
	if err != nil {
		return nil, err
	}

	metricsRegistry.MustRegister(storeFactory)

	backupController, err := omnictrl.NewEtcdBackupController(omnictrl.EtcdBackupControllerSettings{
		ClientMaker: func(ctx context.Context, clusterName string) (omnictrl.TalosClient, error) {
			return talosClientFactory.Get(ctx, clusterName)
		},
		StoreFactory: storeFactory,
		TickInterval: config.Config.EtcdBackup.TickInterval,
	})
	if err != nil {
		return nil, err
	}

	controllers := []controller.Controller{
		omnictrl.NewCertRefreshTickController(constants.CertificateValidityTime / 10), // issue ticks at 10% of the validity, as we refresh certificates at 50% of the validity
		omnictrl.NewClusterController(),
		omnictrl.NewMachineSetController(),
		&omnictrl.ClusterDestroyStatusController{},
		&omnictrl.ClusterMachineIdentityController{},
		&omnictrl.ClusterMachineStatusMetricsController{},
		omnictrl.NewClusterMachineController(),
		&omnictrl.ClusterMachineEncryptionController{},
		&omnictrl.ClusterStatusMetricsController{},
		&omnictrl.ClusterWorkloadProxyController{},
		omnictrl.NewEtcdBackupOverallStatusController(),
		backupController,
		omnictrl.NewImagePullStatusController(&image.TalosImageClient{
			TalosClientFactory: talosClientFactory,
			NodeResolver:       dnsService,
		}),
		&omnictrl.KubernetesStatusController{},
		&omnictrl.LoadBalancerController{},
		&omnictrl.MachineSetNodeController{},
		&omnictrl.MachineSetDestroyStatusController{},
		&omnictrl.MachineSetStatusController{},
		&omnictrl.MachineStatusController{},
		omnictrl.NewMachineStatusLinkController(linkCounterDeltaCh),
		&omnictrl.MachineStatusMetricsController{},
		&omnictrl.VersionsController{},
		omnictrl.NewClusterLoadBalancerController(
			config.Config.LoadBalancer.MinPort,
			config.Config.LoadBalancer.MaxPort,
		),
		&omnictrl.InstallationMediaController{},
		omnictrl.NewKeyPrunerController(
			config.Config.KeyPruner.Interval,
		),
		&omnictrl.OngoingTaskController{},
	}

	qcontrollers := []controller.QController{
		omnictrl.NewBackupDataController(),
		omnictrl.NewClusterBootstrapStatusController(storeFactory),
		omnictrl.NewClusterConfigVersionController(),
		omnictrl.NewClusterEndpointController(),
		omnictrl.NewClusterMachineConfigController(config.Config.DefaultConfigGenOptions),
		omnictrl.NewMachineConfigGenOptionsController(),
		omnictrl.NewClusterMachineConfigStatusController(),
		omnictrl.NewClusterMachineEncryptionKeyController(),
		omnictrl.NewClusterMachineStatusController(),
		omnictrl.NewClusterStatusController(),
		omnictrl.NewClusterUUIDController(),
		omnictrl.NewControlPlaneStatusController(),
		omnictrl.NewEtcdBackupEncryptionController(),
		omnictrl.NewKubeconfigController(constants.CertificateValidityTime),
		omnictrl.NewKubernetesUpgradeManifestStatusController(),
		omnictrl.NewKubernetesUpgradeStatusController(),
		omnictrl.NewMachineController(),
		omnictrl.NewMachineSetEtcdAuditController(talosClientFactory, time.Minute),
		omnictrl.NewRedactedClusterMachineConfigController(),
		omnictrl.NewSecretsController(storeFactory),
		omnictrl.NewTalosConfigController(constants.CertificateValidityTime),
		omnictrl.NewTalosExtensionsController(),
		omnictrl.NewTalosUpgradeStatusController(),
	}

	if config.Config.Auth.SAML.Enabled {
		controllers = append(controllers,
			&omnictrl.SAMLAssertionController{},
		)
	}

	for _, c := range controllers {
		if err = controllerRuntime.RegisterController(c); err != nil {
			return nil, err
		}

		if collector, ok := c.(prometheus.Collector); ok {
			metricsRegistry.MustRegister(collector)
		}
	}

	for _, c := range qcontrollers {
		if err = controllerRuntime.RegisterQController(c); err != nil {
			return nil, err
		}

		if collector, ok := c.(prometheus.Collector); ok {
			metricsRegistry.MustRegister(collector)
		}
	}

	//nolint:lll
	expvarCollector := collectors.NewExpvarCollector(map[string]*prometheus.Desc{
		"controller_crashes":         prometheus.NewDesc("omni_runtime_controller_crashes", "Number of controller-runtime controller crashes by controller name.", []string{"controller"}, nil),
		"controller_wakeups":         prometheus.NewDesc("omni_runtime_controller_wakeups", "Number of controller-runtime controller wakeups by controller name.", []string{"controller"}, nil),
		"controller_reads":           prometheus.NewDesc("omni_runtime_controller_reads", "Number of controller-runtime controller reads by controller name.", []string{"controller"}, nil),
		"controller_writes":          prometheus.NewDesc("omni_runtime_controller_writes", "Number of controller-runtime controller writes by controller name.", []string{"controller"}, nil),
		"reconcile_busy":             prometheus.NewDesc("omni_runtime_controller_busy_seconds", "Number of seconds controller-runtime controller is busy by controller name.", []string{"controller"}, nil),
		"reconcile_cycles":           prometheus.NewDesc("omni_runtime_controller_reconcile_cycles", "Number of reconcile cycles by controller name.", []string{"controller"}, nil),
		"reconcile_input_items":      prometheus.NewDesc("omni_runtime_controller_reconcile_input_items", "Number of reconciled input items by controller name.", []string{"controller"}, nil),
		"qcontroller_crashes":        prometheus.NewDesc("omni_runtime_qcontroller_crashes", "Number of queue controller crashes by controller name.", []string{"controller"}, nil),
		"qcontroller_requeues":       prometheus.NewDesc("omni_runtime_qcontroller_requeues", "Number of queue controller requeues by controller name.", []string{"controller"}, nil),
		"qcontroller_processed":      prometheus.NewDesc("omni_runtime_qcontroller_processed", "Number of queue controller processed items by controller name.", []string{"controller"}, nil),
		"qcontroller_mapped_in":      prometheus.NewDesc("omni_runtime_qcontroller_mapped_in", "Number of queue controller mapped in items by controller name.", []string{"controller"}, nil),
		"qcontroller_mapped_out":     prometheus.NewDesc("omni_runtime_qcontroller_mapped_out", "Number of queue controller mapped out items by controller name.", []string{"controller"}, nil),
		"qcontroller_queue_length":   prometheus.NewDesc("omni_runtime_qcontroller_queue_length", "Number of queue controller queue length by controller name.", []string{"controller"}, nil),
		"qcontroller_map_busy":       prometheus.NewDesc("omni_runtime_qcontroller_map_busy_seconds", "Number of seconds queue controller is busy by controller name.", []string{"controller"}, nil),
		"qcontroller_reconcile_busy": prometheus.NewDesc("omni_runtime_qcontroller_reconcile_busy_seconds", "Number of seconds queue controller reconcile is busy by controller name.", []string{"controller"}, nil),
	})

	metricsRegistry.MustRegister(expvarCollector)

	validationOptions := clusterValidationOptions(resourceState)
	validationOptions = append(validationOptions, relationLabelsValidationOptions()...)
	validationOptions = append(validationOptions, accessPolicyValidationOptions()...)
	validationOptions = append(validationOptions, aclValidationOptions(resourceState)...)
	validationOptions = append(validationOptions, roleValidationOptions()...)
	validationOptions = append(validationOptions, machineSetNodeValidationOptions(resourceState)...)
	validationOptions = append(validationOptions, machineSetValidationOptions(resourceState, storeFactory)...)
	validationOptions = append(validationOptions, identityValidationOptions()...)
	validationOptions = append(validationOptions, exposedServiceValidationOptions()...)
	validationOptions = append(validationOptions, configPatchValidationOptions(resourceState)...)
	validationOptions = append(validationOptions, etcdManualBackupValidationOptions()...)
	validationOptions = append(validationOptions, samlLabelRuleValidationOptions()...)
	validationOptions = append(validationOptions, s3ConfigValidationOptions()...)

	return &Runtime{
		controllerRuntime:            controllerRuntime,
		talosClientFactory:           talosClientFactory,
		storeFactory:                 storeFactory,
		dnsService:                   dnsService,
		workloadProxyServiceRegistry: workloadProxyServiceRegistry,
		resourceLogger:               resourceLogger,
		state:                        state.WrapCore(validated.NewState(resourceState, validationOptions...)),
		virtual:                      virtualState,
		logger:                       logger,
	}, nil
}

// Run starts underlying COSI controller runtime.
func (r *Runtime) Run(ctx context.Context, eg newgroup.EGroup) {
	makeWrap := func(f func(context.Context) error, msgOnError string) func() error {
		return func() error {
			if err := f(ctx); err != nil {
				return fmt.Errorf("%s: %w", msgOnError, err)
			}

			return nil
		}
	}

	if config.Config.WorkloadProxying.Enabled {
		newgroup.GoWithContext(ctx, eg, makeWrap(r.workloadProxyServiceRegistry.Start, "workload proxy service registry failed"))
	}

	if r.resourceLogger != nil {
		newgroup.GoWithContext(ctx, eg, makeWrap(r.resourceLogger.Start, "resource logger failed"))
	}

	newgroup.GoWithContext(ctx, eg, makeWrap(r.talosClientFactory.StartCacheManager, "talos client factory failed"))
	newgroup.GoWithContext(ctx, eg, makeWrap(r.dnsService.Start, "dns service failed"))
	newgroup.GoWithContext(ctx, eg, makeWrap(r.controllerRuntime.Run, "controller runtime failed"))

	newgroup.GoWithContext(ctx, eg, func() error { return r.storeFactory.Start(ctx, r.state, r.logger) })

	if r.virtual == nil {
		return
	}

	newgroup.GoWithContext(ctx, eg, func() error {
		r.virtual.RunComputed(ctx, virtualres.KubernetesUsageType, producers.NewKubernetesUsage, producers.KubernetesUsageResourceTransformer(r.state), r.logger)

		return nil
	})
}

// Watch implements runtime.Runtime.
func (r *Runtime) Watch(ctx context.Context, events chan<- runtime.WatchResponse, setters ...runtime.QueryOption) error {
	opts := runtime.NewQueryOptions(setters...)

	var (
		queries []cosiresource.LabelQuery
		err     error
	)

	if len(opts.LabelSelectors) > 0 {
		queries, err = labels.ParseSelectors(opts.LabelSelectors)
		if err != nil {
			return err
		}
	}

	return cosi.Watch(
		ctx,
		r.state,
		cosiresource.NewMetadata(
			opts.Namespace,
			opts.Resource,
			opts.Name,
			cosiresource.VersionUndefined,
		),
		events,
		opts.TailEvents,
		queries,
	)
}

// Get implements runtime.Runtime.
func (r *Runtime) Get(ctx context.Context, setters ...runtime.QueryOption) (any, error) {
	opts := runtime.NewQueryOptions(setters...)

	metadata := cosiresource.NewMetadata(opts.Namespace, opts.Resource, opts.Name, cosiresource.VersionUndefined)

	res, err := r.state.Get(ctx, metadata)
	if err != nil {
		return nil, err
	}

	return runtime.NewResource(res)
}

// List implements runtime.Runtime.
func (r *Runtime) List(ctx context.Context, setters ...runtime.QueryOption) (runtime.ListResult, error) {
	opts := runtime.NewQueryOptions(setters...)

	var listOptions []state.ListOption

	if len(opts.LabelSelectors) > 0 {
		queries, err := labels.ParseSelectors(opts.LabelSelectors)
		if err != nil {
			return runtime.ListResult{}, err
		}

		for _, query := range queries {
			listOptions = append(listOptions, state.WithLabelQuery(cosiresource.RawLabelQuery(query)))
		}
	}

	list, err := r.state.List(
		ctx,
		cosiresource.NewMetadata(opts.Namespace, opts.Resource, "", cosiresource.VersionUndefined),
		listOptions...,
	)
	if err != nil {
		return runtime.ListResult{}, err
	}

	items := make([]pkgruntime.ListItem, 0, len(list.Items))

	for _, item := range list.Items {
		res, err := runtime.NewResource(item)
		if err != nil {
			return runtime.ListResult{}, err
		}

		items = append(items, NewItem(res))
	}

	return runtime.ListResult{
		Items: items,
		Total: len(items),
	}, nil
}

// Create implements runtime.Runtime.
func (r *Runtime) Create(ctx context.Context, resource cosiresource.Resource, _ ...runtime.QueryOption) error {
	return r.state.Create(ctx, resource)
}

// Update implements runtime.Runtime.
func (r *Runtime) Update(ctx context.Context, resource cosiresource.Resource, _ ...runtime.QueryOption) error {
	if resource.Metadata().Version().String() == cosiresource.VersionUndefined.String() {
		current, err := r.state.Get(ctx, resource.Metadata())
		if err != nil {
			return err
		}

		resource.Metadata().SetVersion(current.Metadata().Version())
	}

	return r.state.Update(ctx, resource)
}

// Delete implements runtime.Runtime.
func (r *Runtime) Delete(ctx context.Context, setters ...runtime.QueryOption) error {
	opts := runtime.NewQueryOptions(setters...)

	md := cosiresource.NewMetadata(opts.Namespace, opts.Resource, opts.Name, cosiresource.VersionUndefined)

	_, err := r.state.Teardown(ctx, md)
	if err != nil {
		return err
	}

	if opts.TeardownOnly {
		return nil
	}

	if _, err = r.state.WatchFor(ctx, md, state.WithFinalizerEmpty()); err != nil {
		return err
	}

	if err = r.state.Destroy(ctx, md); err != nil && !state.IsNotFoundError(err) {
		return err
	}

	return nil
}

// State returns runtime state.
func (r *Runtime) State() state.State { //nolint:ireturn
	return r.state
}

// GetCOSIRuntime returns COSI  controller runtime.
func (r *Runtime) GetCOSIRuntime() *cosiruntime.Runtime {
	return r.controllerRuntime
}

// AdminTalosconfig returns the raw admin talosconfig for the cluster with the given name.
func (r *Runtime) AdminTalosconfig(ctx context.Context, clusterName string) ([]byte, error) {
	ctx = actor.MarkContextAsInternalActor(ctx)

	clusterEndpoint, err := safe.StateGet[*omni.ClusterEndpoint](ctx, r.state, omni.NewClusterEndpoint(omniresources.DefaultNamespace, clusterName).Metadata())
	if err != nil {
		return nil, err
	}

	endpoints := clusterEndpoint.TypedSpec().Value.GetManagementAddresses()

	talosConfig, err := safe.StateGet[*omni.TalosConfig](ctx, r.state, omni.NewTalosConfig(omniresources.DefaultNamespace, clusterName).Metadata())
	if err != nil {
		return nil, err
	}

	return omni.NewTalosClientConfig(talosConfig, endpoints...).Bytes()
}

type item struct {
	runtime.BasicItem[*runtime.Resource]
}

func (it *item) Field(name string) (string, bool) {
	val, ok := it.BasicItem.Field(name)
	if ok {
		return val, true
	}

	val, ok = runtime.ResourceField(it.BasicItem.Unwrap().Resource, name)
	if ok {
		return val, true
	}

	return "", false
}

func (it *item) Match(searchFor string) bool {
	return it.BasicItem.Match(searchFor) || runtime.MatchResource(it.BasicItem.Unwrap().Resource, searchFor)
}

func (it *item) Unwrap() any {
	return it.BasicItem.Unwrap()
}

// NewItem creates new runtime.ListItem from the resource.
func NewItem(res *runtime.Resource) pkgruntime.ListItem {
	return &item{BasicItem: runtime.MakeBasicItem(res.Metadata.ID, res.Metadata.Namespace, res)}
}