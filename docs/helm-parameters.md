# Helm chart parameters

| Parameter | Default value | Description |
| --------- | ------------- | ----------- |
| controllerReplicaCount    | 1        | No. of replicas of kubefledged-controller |
| webhookServerReplicaCount | 1        | No. of replicas of kubefledged-webhook-server |
| controller.hostNetwork    | false    | When set to "true", kubefledged-controller pod runs with "hostNetwork: true" |
| controller.priorityClassName    | ""    | priorityClassName of kubefledged-controller pod |
| webhookServer.enable      | true    | When set to "true", kubefledged-webhook-server is installed |
| webhookServer.hostNetwork | false    | When set to "true", kubefledged-webhook-server pod runs with "hostNetwork: true" |
| webhookServer.priorityClassName    | ""    | priorityClassName of kubefledged-webhook-server pod |
| image.kubefledgedControllerRepository | docker.io/senthilrch/kubefledged-controller | Repository name of kubefledged-controller image |
| image.kubefledgedCRIClientRepository | docker.io/senthilrch/kubefledged-cri-client | Repository name of kubefledged-cri-client image |
| image.kubefledgedWebhookServerRepository | docker.io/senthilrch/kubefledged-webhook-server | Repository name of kubefledged-webhook-server image |
| image.pullPolicy | Always | Image pull policy for kubefledged-controller and kubefledged-webhook-server pods |
| args.controllerCRISocketPath | "" | path to the cri socket on the node e.g. /run/containerd/containerd.sock (default: /var/run/docker.sock, /run/containerd/containerd.sock, /var/run/crio/crio.sock) |
| args.controllerImageCacheRefreshFrequency | 15m | The image cache is refreshed periodically to ensure the cache is up to date. Setting this flag to "0s" will disable refresh |
| args.controllerImageDeleteJobHostNetwork | false | Whether the pod for the image delete job should be run with 'HostNetwork: true' |
| args.controllerImagePullDeadlineDuration | 5m | Maximum duration allowed for pulling an image. After this duration, image pull is considered to have failed |
| args.controllerImagePullPolicy | IfNotPresent | Image pull policy for pulling images into and refreshing the cache. Possible values are 'IfNotPresent' and 'Always'. Default value is 'IfNotPresent'. Image with no or ":latest" tag are always pulled |
| args.controllerJobPriorityClassName | "" | priorityClassName of jobs created by kubefledged-controller. If not specified, priorityClassName won't be set |
| args.controllerJobRetentionPolicy | "delete" | Determines if the jobs created by kubefledged-controller would be deleted or retained (for debugging) after it finishes. Possible values are 'delete' and 'retain'. default value is 'delete'. |
| args.controllerServiceAccountName | "" | serviceAccountName used in Jobs created for pulling or deleting images. Optional flag. If not specified the default service account of the namespace is used |
| args.controllerLogLevel | INFO | Log level of kubefledged-controller |
| args.webhookServerCertFile | /var/run/secrets/webhook-server/tls.crt | Path of server certificate of kubefledged-webhook-server |
| args.webhookServerKeyFile | /var/run/secrets/webhook-server/tls.key | Path of server key of kubefledged-webhook-server |
| args.webhookServerPort | 443 | Listening port of kubefledged-webhook-server |
| args.webhookServerLogLevel | INFO | Log level of kubefledged-webhook-server |
| nameOverride | "" | nameOverride replaces the name of the chart in Chart.yaml, when this is used to construct Kubernetes object names |
| fullnameOverride | "" | fullnameOverride completely replaces the generated name |
|  |  |  |