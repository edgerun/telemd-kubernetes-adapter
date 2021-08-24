package main

import (
	"bufio"
	"errors"
	"fmt"
	goredis "github.com/go-redis/redis/v7"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"log"

	"os"
	"os/signal"
	"sync"
	"syscall"
)

func initRedis() (*goredis.Client, error) {
	host, found := os.LookupEnv("telemd_kubernetes_adapter_redis_host")
	if !found {
		return nil, errors.New("redis host env variable not set")
	}

	port, found := os.LookupEnv("telemd_kubernetes_adapter_redis_port")
	if !found {
		return nil, errors.New("redis port env variable not set")
	}

	url := "redis://" + host + ":" + port

	options, err := goredis.ParseURL(url)

	if err != nil {
		return nil, err
	}

	return goredis.NewClient(options), nil
}

func initKubeClient() (*kubernetes.Clientset, error) {
	kubeConfigLocation, found := os.LookupEnv("telemd_kubernetes_adapter_location")
	if !found {
		return nil, errors.New("Can't find telemd_kubernetes_adapter_location env variable ")
	}

	// init kube client
	var kubeconfig *restclient.Config
	var err error
	if kubeConfigLocation == "cluster" {
		kubeconfig, err = restclient.InClusterConfig()
		if err != nil {
			return nil, err
		}
	} else if kubeConfigLocation == "local" {
		kubeconfigenv := os.Getenv("KUBECONFIG")
		// Create the client configuration
		kubeconfig, err = clientcmd.BuildConfigFromFlags("", kubeconfigenv)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, errors.New(fmt.Sprintf("Unknown kube config location: %s", kubeConfigLocation))
	}

	// Create the client
	clientset, err := kubernetes.NewForConfig(kubeconfig)
	if err != nil {
		return nil, err
	}

	return clientset, nil
}
func watch(daemon *Daemon) error {
	clientset := daemon.clientSet
	stopper := daemon.stopper
	// Create the shared informer factory and use the client to connect to
	// Kubernetes
	factory := informers.NewSharedInformerFactory(clientset, 0)

	// Get the informer for the right resource, in this case a Pod
	informer := factory.Core().V1().Pods().Informer()

	// Kubernetes serves an utility to handle API crashes
	defer runtime.HandleCrash()

	// This is the part where your custom code gets triggered based on the
	// event that the shared informer catches
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		// When a new pod gets created
		AddFunc: func(obj interface{}) { log.Println("not implemented") },
		// When a pod gets updated
		UpdateFunc: func(interface{}, interface{}) { log.Println("not implemented") },
		// When a pod gets deleted
		DeleteFunc: func(interface{}) { log.Println("not implemented") },
	})

	// You need to start the informer, in my case, it runs in the background
	go informer.Run(stopper)

	return nil
}

type Daemon struct {
	stopper   chan struct{}
	rds       *goredis.Client
	clientSet *kubernetes.Clientset
}

func (d *Daemon) run() {
	go func() {
		err := watch(d)
		if err != nil {
			panic(err)
		}
	}()

}

func (daemon *Daemon) Run() {
	var wg sync.WaitGroup
	wg.Add(2)

	// run command loop
	go func() {
		daemon.run()
		wg.Done()
	}()

	wg.Wait()
}

func (d *Daemon) Stop() {
	<-d.stopper
}

func signalHandler(daemon *Daemon) {
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs

		log.Println("stopping daemon")
		daemon.Stop()

		log.Print("all resources closed")
	}()

}
func main() {
	log.Println("Start telemd-kubernetes-adapter")

	rds, err := initRedis()
	defer rds.Close()
	if err != nil {
		panic(err)
	}

	rds.Ping()

	clientset, err := initKubeClient()

	if err != nil {
		panic(err)
	}

	daemon := &Daemon{
		stopper:   make(chan struct{}),
		clientSet: clientset,
		rds:       rds,
	}

	go signalHandler(daemon)

	daemon.Run()

	bufio.NewReader(os.Stdin).ReadString('\n')
	log.Println("Stop telemd-kubernetes-adapter")
}
