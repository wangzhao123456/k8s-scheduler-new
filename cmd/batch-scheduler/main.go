package main

import (
	"context"
	"flag"
	"os"

	"github.com/example/k8s-scheduler-new/pkg/scheduler"
	"k8s.io/klog/v2"
)

func main() {
	var kubeconfig string
	var schedulerName string
	var gangLabel string
	var minAvailableAnnotation string
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster")
	flag.StringVar(&schedulerName, "scheduler-name", "batch-scheduler", "Name of the scheduler to watch for on pods")
	flag.StringVar(&gangLabel, "gang-label", "batch.scheduling.k8s.io/gang", "Label key that identifies gang members")
	flag.StringVar(&minAvailableAnnotation, "min-available-annotation", "batch.scheduling.k8s.io/min-available", "Annotation key that defines min available gang size")
	klog.InitFlags(nil)
	flag.Parse()

	ctx := klog.NewContext(context.Background(), klog.Background())

	cfg, err := scheduler.BuildConfig(kubeconfig)
	if err != nil {
		klog.Fatalf("failed to build kubeconfig: %v", err)
	}

	batchScheduler, err := scheduler.New(ctx, cfg, scheduler.Options{
		SchedulerName:          schedulerName,
		GangLabel:              gangLabel,
		MinAvailableAnnotation: minAvailableAnnotation,
	})
	if err != nil {
		klog.Fatalf("failed to construct scheduler: %v", err)
	}

	if err := batchScheduler.Run(ctx); err != nil {
		klog.ErrorS(err, "scheduler exited with error")
		os.Exit(1)
	}
}
