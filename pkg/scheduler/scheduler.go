package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

// Options configures scheduler behavior.
type Options struct {
	SchedulerName          string
	GangLabel              string
	MinAvailableAnnotation string
}

// BatchScheduler implements a minimal gang-style scheduler.
type BatchScheduler struct {
	client                 kubernetes.Interface
	podInformer            coreinformers.PodInformer
	nodeInformer           coreinformers.NodeInformer
	queue                  workqueue.RateLimitingInterface
	schedulerName          string
	gangLabel              string
	minAvailableAnnotation string
}

// New constructs the scheduler and sets up informers.
func New(ctx context.Context, cfg *rest.Config, opts Options) (*BatchScheduler, error) {
	if opts.SchedulerName == "" {
		return nil, errors.New("scheduler name is required")
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create clientset: %w", err)
	}

	factory := informers.NewSharedInformerFactory(client, 30*time.Second)
	podInformer := factory.Core().V1().Pods()
	nodeInformer := factory.Core().V1().Nodes()

	s := &BatchScheduler{
		client:                 client,
		podInformer:            podInformer,
		nodeInformer:           nodeInformer,
		queue:                  workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "batch-scheduler"),
		schedulerName:          opts.SchedulerName,
		gangLabel:              opts.GangLabel,
		minAvailableAnnotation: opts.MinAvailableAnnotation,
	}

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    s.enqueueIfUnscheduled,
		UpdateFunc: func(_, newObj interface{}) { s.enqueueIfUnscheduled(newObj) },
	})

	return s, nil
}

// Run starts informers and worker loops.
func (s *BatchScheduler) Run(ctx context.Context) error {
	klog.InfoS("starting batch scheduler", "scheduler", s.schedulerName)
	defer s.queue.ShutDown()

	go s.podInformer.Informer().Run(ctx.Done())
	go s.nodeInformer.Informer().Run(ctx.Done())

	if ok := cache.WaitForCacheSync(ctx.Done(), s.podInformer.Informer().HasSynced, s.nodeInformer.Informer().HasSynced); !ok {
		return fmt.Errorf("failed to sync informers")
	}

	worker := func() {
		for s.processNextItem(ctx) {
		}
	}

	go wait.Until(worker, time.Second, ctx.Done())

	<-ctx.Done()
	return nil
}

func (s *BatchScheduler) enqueueIfUnscheduled(obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		return
	}
	if pod.Spec.SchedulerName != s.schedulerName {
		return
	}
	if pod.Spec.NodeName != "" || pod.DeletionTimestamp != nil {
		return
	}
	key, err := cache.MetaNamespaceKeyFunc(pod)
	if err != nil {
		klog.ErrorS(err, "failed to compute key")
		return
	}
	s.queue.Add(key)
}

func (s *BatchScheduler) processNextItem(ctx context.Context) bool {
	obj, shutdown := s.queue.Get()
	if shutdown {
		return false
	}
	defer s.queue.Done(obj)

	key, ok := obj.(string)
	if !ok {
		s.queue.Forget(obj)
		return true
	}

	if err := s.schedulePod(ctx, key); err != nil {
		klog.ErrorS(err, "failed to schedule", "key", key)
		s.queue.AddRateLimited(key)
	} else {
		s.queue.Forget(obj)
	}
	return true
}

func (s *BatchScheduler) schedulePod(ctx context.Context, key string) error {
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	pod, err := s.podInformer.Lister().Pods(ns).Get(name)
	if err != nil {
		return err
	}
	if pod.Spec.NodeName != "" || pod.DeletionTimestamp != nil {
		return nil
	}

	gangID := pod.Labels[s.gangLabel]
	if gangID == "" {
		gangID = pod.Name // treat single pod as its own gang
	}

	gangPods, err := s.podsForGang(ns, gangID)
	if err != nil {
		return err
	}
	if len(gangPods) == 0 {
		return nil
	}

	minAvailable := s.resolveMinAvailable(gangPods)
	ready := filterUnboundPods(gangPods)
	if len(ready) < minAvailable {
		return fmt.Errorf("gang %s not ready, need %d pods, have %d", gangID, minAvailable, len(ready))
	}

	plan, err := s.planGang(ctx, ready)
	if err != nil {
		return err
	}

	for i, p := range ready {
		nodeName := plan[i]
		if nodeName == "" {
			return fmt.Errorf("missing node assignment for pod %s", p.Name)
		}
		if err := s.bindPod(ctx, p, nodeName); err != nil {
			return fmt.Errorf("bind pod %s: %w", p.Name, err)
		}
	}
	return nil
}

func (s *BatchScheduler) resolveMinAvailable(gang []*v1.Pod) int {
	if len(gang) == 0 {
		return 0
	}
	if value, ok := gang[0].Annotations[s.minAvailableAnnotation]; ok {
		if intVal, err := intstr.GetValueFromIntOrPercent(&intstr.IntOrString{Type: intstr.String, StrVal: value}, len(gang), true); err == nil {
			if intVal < 1 {
				return len(gang)
			}
			if intVal > len(gang) {
				return len(gang)
			}
			return intVal
		}
	}
	return len(gang)
}

func (s *BatchScheduler) podsForGang(namespace, gangID string) ([]*v1.Pod, error) {
	selector := labels.Set{s.gangLabel: gangID}.AsSelector()
	pods, err := s.podInformer.Lister().Pods(namespace).List(selector)
	if err != nil {
		return nil, err
	}
	// ensure scheduler matches
	filtered := make([]*v1.Pod, 0, len(pods))
	for _, p := range pods {
		if p.Spec.SchedulerName == s.schedulerName {
			filtered = append(filtered, p)
		}
	}
	return filtered, nil
}

func filterUnboundPods(pods []*v1.Pod) []*v1.Pod {
	res := make([]*v1.Pod, 0, len(pods))
	for _, p := range pods {
		if p.Spec.NodeName == "" && p.DeletionTimestamp == nil {
			res = append(res, p)
		}
	}
	return res
}

func (s *BatchScheduler) planGang(ctx context.Context, pods []*v1.Pod) ([]string, error) {
	nodes, err := s.nodeInformer.Lister().List(labels.Everything())
	if err != nil {
		return nil, err
	}
	// compute available resources for each node
	avail := make(map[string]resourceState, len(nodes))
	for _, n := range nodes {
		if !nodeReady(n) || n.Spec.Unschedulable {
			continue
		}
		avail[n.Name] = s.availableResources(n)
	}
	if len(avail) == 0 {
		return nil, fmt.Errorf("no schedulable nodes")
	}

	plan := make([]string, len(pods))
	for i, p := range pods {
		bestNode := ""
		for nodeName, state := range avail {
			if state.canFit(p) {
				bestNode = nodeName
				// optimistic allocate resources
				state.consume(p)
				avail[nodeName] = state
				break
			}
		}
		if bestNode == "" {
			return nil, fmt.Errorf("no feasible node for pod %s", p.Name)
		}
		plan[i] = bestNode
	}
	return plan, nil
}

func (s *BatchScheduler) availableResources(node *v1.Node) resourceState {
	alloc := node.Status.Allocatable
	cpu := alloc.Cpu().MilliValue()
	mem := alloc.Memory().Value()

	// subtract requests of running pods
	pods, _ := s.podInformer.Lister().Pods(metav1.NamespaceAll).List(labels.Everything())
	for _, p := range pods {
		if p.Spec.NodeName != node.Name {
			continue
		}
		req := calculateRequest(p)
		cpu -= req.cpuMilli
		mem -= req.memory
	}
	return resourceState{cpuMilli: cpu, memory: mem}
}

func nodeReady(node *v1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == v1.NodeReady {
			return cond.Status == v1.ConditionTrue
		}
	}
	return false
}

func (s *BatchScheduler) bindPod(ctx context.Context, pod *v1.Pod, nodeName string) error {
	binding := &v1.Binding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
			UID:       pod.UID,
		},
		Target: v1.ObjectReference{
			Kind: "Node",
			Name: nodeName,
		},
	}

	return s.client.CoreV1().Pods(pod.Namespace).Bind(ctx, binding, metav1.CreateOptions{})
}

// resourceState tracks available CPU/memory in milliCPU and bytes.
type resourceState struct {
	cpuMilli int64
	memory   int64
}

func (r resourceState) canFit(pod *v1.Pod) bool {
	req := calculateRequest(pod)
	return r.cpuMilli >= req.cpuMilli && r.memory >= req.memory
}

func (r *resourceState) consume(pod *v1.Pod) {
	req := calculateRequest(pod)
	r.cpuMilli -= req.cpuMilli
	r.memory -= req.memory
}

// resourceDemand sums container requests.
type resourceDemand struct {
	cpuMilli int64
	memory   int64
}

func calculateRequest(pod *v1.Pod) resourceDemand {
	var cpu, mem int64
	for _, c := range pod.Spec.Containers {
		cpu += c.Resources.Requests.Cpu().MilliValue()
		mem += c.Resources.Requests.Memory().Value()
	}
	return resourceDemand{cpuMilli: cpu, memory: mem}
}
