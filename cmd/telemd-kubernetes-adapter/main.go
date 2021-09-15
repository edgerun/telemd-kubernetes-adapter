package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	goredis "github.com/go-redis/redis/v7"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
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
	"time"
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

func saveNodeInfo(node v1.Node, client *goredis.Client) error {
	key := "telemd.info:" + node.Name

	multi := client.TxPipeline()

	marshal, err := json.Marshal(node.Labels)
	multi.HSet(key, "labels", marshal)

	_, err = multi.Exec()
	return err
}

func saveNodeInfos(client *goredis.Client, clientset *kubernetes.Clientset) {
	nodes, _ := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	fmt.Printf("Saving nodeinfos of %d nodes", len(nodes.Items))
	for _, node := range nodes.Items {
		err := saveNodeInfo(node, client)
		if err != nil {
			log.Printf("error happened, saving nodeinfo, %s", err)
		}
	}
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

type ContainerMessage struct {
	Id    string `json:"id"`
	Image string `json:"image"`
}

type PodMessage struct {
	PodUid     types.UID                   `json:"podUid"`
	Containers map[string]ContainerMessage `json:"containers"`
	HostIP     string                      `json:"hostIP"`
	Name       string                      `json:"Name"`
	NodeName   string                      `json:"NodeName"`
	PodIP      string                      `json:"podIP"`
	QosClass   v1.PodQOSClass              `json:"qosClass"`
	StartTime  *metav1.Time                `json:"startTime"`
	Labels     map[string]string           `json:"labels"`
}

func publishAddPod(obj interface{}, daemon *Daemon) {
	pod := obj.(*v1.Pod)
	log.Printf("Added Pod: %s\n", pod.Name)
	jsonObject, err := marshallPod(pod)
	if err {
		return
	}
	log.Println(jsonObject)

	ts := float64(time.Now().UnixNano()) / float64(1000000000)
	name := "pod/create"
	value := jsonObject
	daemon.rds.Publish("galileo/events", fmt.Sprintf("%.7f %s %s", ts, name, value))
}

func marshallPod(pod *v1.Pod) (string, bool) {
	containers := make(map[string]ContainerMessage)
	for _, container := range pod.Spec.Containers {
		containers[container.Name] = ContainerMessage{
			Image: container.Image,
		}
	}
	podMessage := PodMessage{
		PodUid:     pod.UID,
		Containers: containers,
		HostIP:     pod.Status.HostIP,
		Name:       pod.Name,
		NodeName:   pod.Spec.NodeName,
		PodIP:      pod.Status.PodIP,
		QosClass:   pod.Status.QOSClass,
		StartTime:  pod.Status.StartTime,
		Labels:     pod.Labels,
	}

	marshal, err := json.Marshal(podMessage)
	if err != nil {
		log.Fatal(err)
		return "", true
	}
	jsonObject := string(marshal)
	return jsonObject, false
}

func publishDeletePod(obj interface{}, daemon *Daemon) {
	pod := obj.(*v1.Pod)
	jsonObject, err := marshallPod(pod)
	if err {
		return
	}
	log.Println(jsonObject)

	ts := time.Now().Unix()
	name := "pod/delete"
	value := jsonObject
	daemon.rds.Publish("galileo/event", fmt.Sprintf("%d %s %s", ts, name, value))
}

func watch(daemon *Daemon) error {
	clientset := daemon.clientSet

	publishDeployedPods(daemon)

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
		AddFunc: func(obj interface{}) { publishAddPod(obj, daemon) },

		// When a pod gets deleted
		DeleteFunc: func(obj interface{}) { publishDeletePod(obj, daemon) },
	})

	// You need to start the informer, in my case, it runs in the background
	go informer.Run(stopper)

	return nil
}

func publishDeployedPods(daemon *Daemon) {
	pods, err := daemon.clientSet.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}

	fmt.Printf("There are %d pods in the cluster\n", len(pods.Items))
	for _, pod := range pods.Items {
		publishAddPod(&pod, daemon)
	}
}

type Daemon struct {
	stopper   chan struct{}
	rds       *goredis.Client
	clientSet *kubernetes.Clientset
}

func (daemon *Daemon) run() {
	go func() {
		err := watch(daemon)
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

func (daemon *Daemon) Stop() {
	<-daemon.stopper
	daemon.rds.Close()
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

	if err != nil {
		panic(err)
	}

	rds.Ping()

	clientset, err := initKubeClient()

	if err != nil {
		panic(err)
	}

	saveNodeInfos(rds, clientset)

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
