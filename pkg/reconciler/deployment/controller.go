package deployment

import (
	"context"
	"time"

	clusterclient "github.com/kcp-dev/kcp/pkg/client/clientset/versioned"
	"github.com/kcp-dev/kcp/pkg/client/informers/externalversions"
	clusterlisters "github.com/kcp-dev/kcp/pkg/client/listers/cluster/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appsv1lister "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const resyncPeriod = 10 * time.Hour

// NewController returns a new Controller which splits new Deployment objects
// into N virtual Deployments labeled for each Cluster that exists at the time
// the Deployment is created.
func NewController(cfg *rest.Config) *Controller {
	kubeClient := kubernetes.NewForConfigOrDie(cfg)
	stopCh := make(chan struct{}) // TODO: hook this up to SIGTERM/SIGINT

	c := &Controller{
		kubeClient:   kubeClient,
		stopCh:       stopCh,
		queue:        workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		clusterQueue: workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
	}

	csif := externalversions.NewSharedInformerFactoryWithOptions(clusterclient.NewForConfigOrDie(cfg), resyncPeriod)
	csif.Cluster().V1alpha1().Clusters().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueueCluster(obj) },
		UpdateFunc: func(_, obj interface{}) { c.enqueueCluster(obj) },
		DeleteFunc: func(obj interface{}) { c.enqueueCluster(obj) },
	})
	csif.WaitForCacheSync(stopCh)
	csif.Start(stopCh)
	c.clusterLister = csif.Cluster().V1alpha1().Clusters().Lister()

	sif := informers.NewSharedInformerFactoryWithOptions(kubeClient, resyncPeriod)
	sif.Apps().V1().Deployments().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueue(obj) },
		UpdateFunc: func(_, obj interface{}) { c.enqueue(obj) },
	})
	sif.WaitForCacheSync(stopCh)
	sif.Start(stopCh)
	c.indexer = sif.Apps().V1().Deployments().Informer().GetIndexer()
	c.lister = sif.Apps().V1().Deployments().Lister()

	return c
}

type Controller struct {
	kubeClient          kubernetes.Interface
	stopCh              chan struct{}
	queue, clusterQueue workqueue.RateLimitingInterface
	lister              appsv1lister.DeploymentLister
	clusterLister       clusterlisters.ClusterLister
	indexer             cache.Indexer
}

func (c *Controller) enqueueCluster(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	c.clusterQueue.AddRateLimited(key)
}

func (c *Controller) enqueue(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	c.queue.AddRateLimited(key)
}

func (c *Controller) Start(numThreads int) {
	defer c.queue.ShutDown()
	defer c.clusterQueue.ShutDown()
	for i := 0; i < numThreads; i++ {
		go wait.Until(c.startWorker, time.Second, c.stopCh)
		go wait.Until(c.startClusterWorker, time.Second, c.stopCh)
	}
	klog.Infof("Starting workers")
	<-c.stopCh
	klog.Infof("Stopping workers")
}

func (c *Controller) startWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) startClusterWorker() {
	for c.processNextClusterWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	// Wait until there is a new item in the working queue
	k, quit := c.queue.Get()
	if quit {
		return false
	}
	key := k.(string)

	// No matter what, tell the queue we're done with this key, to unblock
	// other workers.
	defer c.queue.Done(key)

	err := c.process(key)
	handleErr(err, key, c.queue)
	return true
}

func (c *Controller) processNextClusterWorkItem() bool {
	// Wait until there is a new item in the working queue
	k, quit := c.clusterQueue.Get()
	if quit {
		return false
	}
	key := k.(string)

	// No matter what, tell the queue we're done with this key, to unblock
	// other workers.
	defer c.queue.Done(key)

	err := c.processCluster(key)
	handleErr(err, key, c.clusterQueue)
	return true
}

func handleErr(err error, key string, queue workqueue.RateLimitingInterface) {
	// Reconcile worked, nothing else to do for this workqueue item.
	if err == nil {
		queue.Forget(key)
		return
	}

	// Re-enqueue up to 5 times.
	num := queue.NumRequeues(key)
	if num < 5 {
		klog.Errorf("Error reconciling key %q, retrying... (#%d): %v", key, num, err)
		queue.AddRateLimited(key)
		return
	}

	// Give up and report error elsewhere.
	queue.Forget(key)
	runtime.HandleError(err)
	klog.Infof("Dropping key %q after failed retries: %v", key, err)
}

func (c *Controller) process(key string) error {
	obj, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		return err
	}

	if !exists {
		klog.Infof("Object with key %q was deleted", key)
		return nil
	}
	current := obj.(*appsv1.Deployment)
	previous := current.DeepCopy()

	ctx := context.TODO()
	if err := c.reconcile(ctx, current); err != nil {
		return err
	}

	// If the object being reconciled changed as a result, update it.
	if !equality.Semantic.DeepEqual(previous, current) {
		_, uerr := c.kubeClient.AppsV1().Deployments(current.Namespace).Update(ctx, current, metav1.UpdateOptions{})
		return uerr
	}
	if !equality.Semantic.DeepEqual(previous.Status, current.Status) {
		_, uerr := c.kubeClient.AppsV1().Deployments(current.Namespace).UpdateStatus(ctx, current, metav1.UpdateOptions{})
		return uerr
	}
	return err
}

// processCluster triggers a full rebalance of all roots.
func (c *Controller) processCluster(string) error {
	// Get all deployments, filter out non-roots, and enqueue a
	// reconciliation of all remaining roots.
	//
	// This will trigger a rebalance of replicas across available clusters,
	// taking into account new clusters, updated (newly ready or not
	// ready), and deleted clusters.
	ds, err := c.lister.List(labels.Everything())
	if err != nil {
		return err
	}
	for _, d := range ds {
		if d.Labels == nil {
			d.Labels = map[string]string{}
		}
		if d.Labels[ownedByLabel] == "" {
			c.enqueue(d)
		}
	}
	return nil
}
