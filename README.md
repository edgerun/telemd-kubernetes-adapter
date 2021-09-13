# telemd-kubernetes-adapter
Daemon that watches Pods in Kubernetes that stores metadata, such that users are later able to associate the telemd cgroup metrics with the corresponding container based on the ID.

Build
-----

To build the module's binaries using your local go installation run:

    make

To build Docker images for local usage (without a go installation) run:

    make docker


Details
-------

telemd-kubernetes-adapter reports all Pods running and newly being created.
It uses etcd's `watch` feature (via the kubernetes client lib) to monitor Pods that are created by Kubernetes.

Because the [telemd](https://github.com/edgerun/telemd) reports for running containers (including Pods and their containers) only
the container ID, we need to save some metadata in order to later associate Pods/Container Images to their respective usage.
This functionality offers the `telemd-kubernetes-adapter` daemon.

See [this SO thread](https://stackoverflow.com/a/49057417) for an explanation on how `cgroups` are structured for Pods.

Usage
-----

### Topic schema

The daemon will call for each pod the following command:
    
    publish galileo/events "{timestamp} pod/[create|delete] {metadata}"

Whereas metadata:

    {
        "podUid": "12b379f7-ef99-46e5-8125-0129a27bd799",
        "containerID": "containerd://11e9220bd2b450ed40132bcd5cdc3c115b93c69d02c37d844fec7574026edff3",
        "image": "docker.io/library/nginx:1.14.2",
        "podName": "nginx-deployment-66b6c48dd5-vd2jt",
        "hostIP": "10.0.1.2",
        "name": "nginx",
        "nodeName": "pod-host",
        "podIP": "10.42.1.95",
        "qosClass": "BestEffort",
        "startTime": "2021-08-17T18:29:58Z"
    }

All fields are extracted from:

    kubectl get pod --all-namespaces -o json | jq '.items[] | select(.metadata.uid == "12b379f7-ef99-46e5-8125-0129a27bd799")'

### Environment variables

The daemon will read redis credentials from the following environment variables:

* `telemd_kubernetes_adapter_redis_host`
* `telemd_kubernetes_adapter_redis_port`
* `telemd_kubernetes_adapter_location` (either: `local` or `cluster`)
  * In case of `local`, will read kubeconfig from `KUBECONFIG` env variable
