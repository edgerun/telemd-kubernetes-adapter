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
	"strings"
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
	marshalAllocatable, err := json.Marshal(node.Status.Allocatable)
	multi.HSet(key, "labels", marshal)
	multi.HSet(key, "allocatable", marshalAllocatable)

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
	Id               string            `json:"id"`
	Name             string            `json:"name"`
	Image            string            `json:"image"`
	Port             int32             `json:"port"`
	ResourceRequests map[string]string `json:"resource_requests"`
	ResourceLimits   map[string]string `json:"resource_limits"`
}

type PodMessage struct {
	PodUid     types.UID                   `json:"podUid"`
	Containers map[string]ContainerMessage `json:"containers"`
	Namespace  string                      `json:"namespace"`
	HostIP     string                      `json:"hostIP"`
	Name       string                      `json:"name"`
	NodeName   string                      `json:"nodeName"`
	PodIP      string                      `json:"podIP"`
	QosClass   v1.PodQOSClass              `json:"qosClass"`
	StartTime  *metav1.Time                `json:"startTime"`
	Labels     map[string]string           `json:"labels"`
	Phase      v1.PodPhase                 `json:"status"`
}

func publishRunningPod(obj interface{}, daemon *Daemon) {
	pod := obj.(*v1.Pod)
	if pod.Status.Phase != v1.PodRunning {
		log.Println("Got notified about Pod but state is not running: ", fmt.Sprintf("%s %s", pod.Name, pod.Status.Phase))
		return
	}
	if len(pod.Status.PodIP) == 0 {
		log.Println("Got notified about Pod but does not yet have Pod IP: ", fmt.Sprintf("%s %s", pod.Name, pod.Status.Phase))
		return
	}

	if len(pod.Status.ContainerStatuses) < 1 {
		log.Println("Got notified about Pod but does not have ContainerStatus: ", fmt.Sprintf("%s %s", pod.Name, pod.Status.Phase))
		return
	}

	if len(pod.Status.ContainerStatuses[0].ContainerID) == 0 {
		log.Println("Got notified about Pod but does not have Container ID: ", fmt.Sprintf("%s %s", pod.Name, pod.Status.Phase))
		return
	}

	log.Printf("Added Pod: %s\n", pod.Name)
	publishPod(pod, "running", daemon)
}

func findContainer(name string, image string, pod *v1.Pod) (*v1.Container, bool) {
	if len(pod.Spec.Containers) == 1 {
		return &pod.Spec.Containers[0], true
	}
	for _, container := range pod.Spec.Containers {
		containerImage := strings.Split(container.Image, ":")[0]
		if container.Name == name && strings.Contains(image, containerImage) {
			return &container, true
		}
	}
	return nil, false
}

func mapResource(resources v1.ResourceList) map[string]string {
	m := make(map[string]string)
	for name, quantity := range resources {
		marshal, _ := json.Marshal(quantity)
		m[string(name)] = string(marshal)
	}
	return m
}

func marshallPod(pod *v1.Pod) (string, bool) {
	containers := make(map[string]ContainerMessage)
	for _, containerStatus := range pod.Status.ContainerStatuses {
		name := containerStatus.Name
		image := containerStatus.Image
		container, _ := findContainer(name, image, pod)
		image = container.Image
		requests := mapResource(container.Resources.Requests)
		limits := mapResource(container.Resources.Limits)

		var containerPort int32
		if len(container.Ports) > 0 {
			containerPort = container.Ports[0].ContainerPort
		} else {
			containerPort = -1
		}

		containers[container.Name] = ContainerMessage{
			Id:               containerStatus.ContainerID,
			Name:             name,
			Image:            image,
			ResourceRequests: requests,
			ResourceLimits:   limits,
			Port:             containerPort,
		}
	}

	podMessage := PodMessage{
		PodUid:     pod.UID,
		Containers: containers,
		Namespace:  pod.Namespace,
		HostIP:     pod.Status.HostIP,
		Name:       pod.Name,
		NodeName:   pod.Spec.NodeName,
		PodIP:      pod.Status.PodIP,
		QosClass:   pod.Status.QOSClass,
		StartTime:  pod.Status.StartTime,
		Labels:     pod.Labels,
		Phase:      pod.Status.Phase,
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
	publishPod(obj, "delete", daemon)
}

func publishCreatePod(obj interface{}, daemon *Daemon) {
	publishPod(obj, "create", daemon)
}

func publishPod(obj interface{}, event string, daemon *Daemon) {
	pod := obj.(*v1.Pod)
	jsonObject, err := marshallPod(pod)
	if err {
		return
	}
	log.Println(jsonObject)

	ts := fmt.Sprintf("%.7f", float64(time.Now().UnixNano())/float64(1_000_000_000))
	name := fmt.Sprintf("pod/%s", event)
	value := jsonObject
	daemon.rds.Publish("galileo/events", fmt.Sprintf("%s %s %s", ts, name, value))
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
		AddFunc: func(obj interface{}) {
			pod := obj.(*v1.Pod)
			log.Println("Add - ", pod.Status.Phase)
			publishCreatePod(obj, daemon)
		},

		// When a pod gets updated
		UpdateFunc: func(old interface{}, new interface{}) {
			oldPod := old.(*v1.Pod)
			newPod := new.(*v1.Pod)
			log.Printf("Update - old: %s, new: %s\n", oldPod.Status.Phase, newPod.Status.Phase)
			if oldPod.Status.Phase == v1.PodPending && newPod.Status.Phase == v1.PodRunning {
				publishRunningPod(new, daemon)
			} else if oldPod.Status.Phase == v1.PodPending && newPod.Status.Phase == v1.PodPending {
				publishPod(new, "pending", daemon)
			} else if oldPod.DeletionTimestamp == nil && newPod.DeletionTimestamp != nil {
				publishPod(new, "shutdown", daemon)
			}

		},

		// When a pod gets deleted
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*v1.Pod)
			log.Println("Delete - ", pod.Status.Phase)
			publishDeletePod(obj, daemon)
		},
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
		publishRunningPod(&pod, daemon)
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
