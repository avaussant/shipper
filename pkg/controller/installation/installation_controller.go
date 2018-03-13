package installation

import (
	"fmt"
	"time"

	"github.com/golang/glog"
	shipperV1 "github.com/bookingcom/shipper/pkg/apis/shipper/v1"
	shipper "github.com/bookingcom/shipper/pkg/client/clientset/versioned"
	shipperInformers "github.com/bookingcom/shipper/pkg/client/informers/externalversions"
	shipperListers "github.com/bookingcom/shipper/pkg/client/listers/shipper/v1"
	"github.com/bookingcom/shipper/pkg/clusterclientstore"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// Controller is a Kubernetes controller that processes InstallationTarget
// objects.
type Controller struct {
	shipperclientset shipper.Interface
	// the Kube clients for each of the target clusters
	clusterClientStore clusterclientstore.ClientProvider

	workqueue                 workqueue.RateLimitingInterface
	installationTargetsSynced cache.InformerSynced
	installationTargetsLister shipperListers.InstallationTargetLister
	clusterLister             shipperListers.ClusterLister
	releaseLister             shipperListers.ReleaseLister
	dynamicClientBuilderFunc  DynamicClientBuilderFunc
}

// NewController returns a new Installation controller.
func NewController(
	shipperclientset shipper.Interface,
	shipperInformerFactory shipperInformers.SharedInformerFactory,
	store clusterclientstore.ClientProvider,
	dynamicClientBuilderFunc DynamicClientBuilderFunc,
) *Controller {

	// Management Cluster InstallationTarget informer
	installationTargetInformer := shipperInformerFactory.Shipper().V1().InstallationTargets()
	clusterInformer := shipperInformerFactory.Shipper().V1().Clusters()
	releaseInformer := shipperInformerFactory.Shipper().V1().Releases()

	controller := &Controller{
		shipperclientset:          shipperclientset,
		clusterClientStore:        store,
		clusterLister:             clusterInformer.Lister(),
		releaseLister:             releaseInformer.Lister(),
		installationTargetsLister: installationTargetInformer.Lister(),
		installationTargetsSynced: installationTargetInformer.Informer().HasSynced,
		dynamicClientBuilderFunc:  dynamicClientBuilderFunc,
		workqueue:                 workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "InstallationTargets"),
	}

	installationTargetInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueInstallationTarget,
		UpdateFunc: func(oldObj, newObj interface{}) {
			controller.enqueueInstallationTarget(newObj)
		},
	})

	return controller
}

// Run starts Installation controller workers and blocks until stopCh is
// closed.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	glog.V(2).Info("Starting Installation controller")
	defer glog.V(2).Info("Shutting down Installation controller")

	if !cache.WaitForCacheSync(stopCh, c.installationTargetsSynced) {
		runtime.HandleError(fmt.Errorf("failed to wait for caches to sync"))
		return
	}

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	glog.V(4).Info("Started Installation controller")

	<-stopCh
}

func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.workqueue.Done(obj)
		var (
			key string
			ok  bool
		)
		if key, ok = obj.(string); !ok {
			// Do not attempt to process invalid objects again.
			c.workqueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("invalid object key: %#v", obj))
			return nil
		}
		if err := c.syncOne(key); err != nil {
			return fmt.Errorf("error syncing %q: %s", key, err)
		}

		// Do not requeue this object because it's already processed.
		c.workqueue.Forget(obj)
		glog.Infof("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}

	return true
}

func (c *Controller) syncOne(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	it, err := c.installationTargetsLister.InstallationTargets(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			runtime.HandleError(fmt.Errorf("InstallationTarget %q has been deleted", key))
			return nil
		}
		return err
	}

	return c.processInstallation(it.DeepCopy())
}

func (c *Controller) enqueueInstallationTarget(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		runtime.HandleError(err)
		return
	}
	c.workqueue.AddRateLimited(key)
}

// processInstallation attempts to install the related release on all target clusters.
func (c *Controller) processInstallation(it *shipperV1.InstallationTarget) error {
	if n := len(it.OwnerReferences); n != 1 {
		return fmt.Errorf("expected exactly one owner for InstallationTarget %q, got %d", it.GetName(), n)
	}

	owner := it.OwnerReferences[0]

	release, err := c.releaseLister.Releases(it.GetNamespace()).Get(owner.Name)
	if err != nil {
		return err
	} else if release.GetUID() != owner.UID {
		return fmt.Errorf(
			"the owner Release for InstallationTarget %q is gone; expected UID %s but got %s",
			it.GetName(),
			owner.UID,
			release.GetUID(),
		)
	}

	handler := NewInstaller(release)

	// The strategy here is try our best to install as many objects as possible
	// in all target clusters. It is not the Installation Controller job to
	// reason about a target cluster status.
	clusterStatuses := make([]*shipperV1.ClusterInstallationStatus, 0, len(it.Spec.Clusters))
	for _, clusterName := range it.Spec.Clusters {
		status := &shipperV1.ClusterInstallationStatus{Name: clusterName}
		clusterStatuses = append(clusterStatuses, status)

		var cluster *shipperV1.Cluster
		if cluster, err = c.clusterLister.Get(clusterName); err != nil {
			status.Status = shipperV1.InstallationStatusFailed
			status.Message = err.Error()
			glog.Warningf("Get Cluster %q for InstallationTarget: %s", clusterName, metaKey(it), err)
			continue
		}

		var client kubernetes.Interface
		var restConfig *rest.Config
		client, restConfig, err = c.GetClusterAndConfig(clusterName)
		if err != nil {
			status.Status = shipperV1.InstallationStatusFailed
			status.Message = err.Error()
			glog.Warningf("Get config for Cluster %q for InstallationTarget %q: %s", clusterName, metaKey(it), err)
			continue
		}

		if err = handler.installRelease(cluster, client, restConfig, c.dynamicClientBuilderFunc); err != nil {
			status.Status = shipperV1.InstallationStatusFailed
			status.Message = err.Error()
			glog.Warningf("Install InstallationTarget %q for Cluster %q: %s", metaKey(it), clusterName, err)
			continue
		}

		status.Status = shipperV1.InstallationStatusInstalled
	}

	it.Status.Clusters = clusterStatuses

	_, err = c.shipperclientset.ShipperV1().InstallationTargets(it.Namespace).Update(it)
	if err != nil {
		glog.Warningf("Update InstallationTarget %q: %s", metaKey(it), err)
		return err
	}

	return nil
}

func (c *Controller) GetClusterAndConfig(clusterName string) (kubernetes.Interface, *rest.Config, error) {
	var client kubernetes.Interface
	var referenceConfig *rest.Config
	var err error

	if client, err = c.clusterClientStore.GetClient(clusterName); err != nil {
		return nil, nil, err
	}

	if referenceConfig, err = c.clusterClientStore.GetConfig(clusterName); err != nil {
		return nil, nil, err
	}

	// the client store is just like an informer cache: it's a shared pointer to a read-only struct, so copy it before mutating
	copy := rest.CopyConfig(referenceConfig)

	return client, copy, nil
}

func metaKey(it *shipperV1.InstallationTarget) string {
	key, _ := cache.MetaNamespaceKeyFunc(it)
	return key
}
