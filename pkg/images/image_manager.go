/*
Copyright 2018 The kube-fledged authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package images

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	fledgedv1alpha2 "github.com/senthilrch/kube-fledged/pkg/apis/kubefledged/v1alpha2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/storage/names"
	kubeinformers "k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const controllerAgentName = "fledged"
const fakeJobPrefix = "fakejob-"

const (
	// ImageWorkResultStatusSucceeded means image pull/delete succeeded
	ImageWorkResultStatusSucceeded = "succeeded"
	// ImageWorkResultStatusFailed means image pull/delete failed
	ImageWorkResultStatusFailed = "failed"
	// ImageWorkResultStatusJobCreated means job for image pull/delete created
	ImageWorkResultStatusJobCreated = "jobcreated"
	//ImageWorkResultStatusAlreadyPulled  means image is already present in the node
	ImageWorkResultStatusAlreadyPulled = "alreadypulled"
)

// ImageManager provides the functionalities for pulling and deleting images
type ImageManager struct {
	fledgedNameSpace          string
	workqueue                 workqueue.RateLimitingInterface
	imageworkqueue            workqueue.RateLimitingInterface
	kubeclientset             kubernetes.Interface
	imageworkstatus           map[string]ImageWorkResult
	kubeInformerFactory       kubeinformers.SharedInformerFactory
	podsLister                corelisters.PodLister
	podsSynced                cache.InformerSynced
	imagePullDeadlineDuration time.Duration
	dockerClientImage         string
	imagePullPolicy           string
	lock                      sync.RWMutex
}

// ImageWorkRequest has image name, node name, work type and imagecache
type ImageWorkRequest struct {
	Image                   string
	Node                    *corev1.Node
	ContainerRuntimeVersion string
	WorkType                WorkType
	Imagecache              *fledgedv1alpha2.ImageCache
}

// ImageWorkResult stores the result of pulling and deleting image
type ImageWorkResult struct {
	ImageWorkRequest ImageWorkRequest
	Status           string
	Reason           string
	Message          string
}

// WorkType refers to type of work to be done by sync handler
type WorkType string

// Work types
const (
	ImageCacheCreate       WorkType = "create"
	ImageCacheUpdate       WorkType = "update"
	ImageCacheDelete       WorkType = "delete"
	ImageCacheStatusUpdate WorkType = "statusupdate"
	ImageCacheRefresh      WorkType = "refresh"
	ImageCachePurge        WorkType = "purge"
)

// WorkQueueKey is an item in the sync handler's work queue
type WorkQueueKey struct {
	WorkType      WorkType
	ObjKey        string
	Status        *map[string]ImageWorkResult
	OldImageCache *fledgedv1alpha2.ImageCache
}

// NewImageManager returns a new image manager object
func NewImageManager(
	workqueue workqueue.RateLimitingInterface,
	imageworkqueue workqueue.RateLimitingInterface,
	kubeclientset kubernetes.Interface,
	namespace string,
	imagePullDeadlineDuration time.Duration,
	dockerClientImage, imagePullPolicy string) (*ImageManager, coreinformers.PodInformer) {

	kubeInformerFactory := kubeinformers.NewSharedInformerFactoryWithOptions(
		kubeclientset,
		time.Second*30,
		kubeinformers.WithNamespace(namespace))
	podInformer := kubeInformerFactory.Core().V1().Pods()

	imagemanager := &ImageManager{
		fledgedNameSpace:          namespace,
		workqueue:                 workqueue,
		imageworkqueue:            imageworkqueue,
		kubeclientset:             kubeclientset,
		imageworkstatus:           make(map[string]ImageWorkResult),
		kubeInformerFactory:       kubeInformerFactory,
		podsLister:                podInformer.Lister(),
		podsSynced:                podInformer.Informer().HasSynced,
		imagePullDeadlineDuration: imagePullDeadlineDuration,
		dockerClientImage:         dockerClientImage,
		imagePullPolicy:           imagePullPolicy,
	}
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		//AddFunc: ,
		UpdateFunc: func(old, new interface{}) {
			newPod := new.(*corev1.Pod)
			oldPod := old.(*corev1.Pod)
			if newPod.ResourceVersion == oldPod.ResourceVersion {
				// Periodic resync will send update events for all known Pods.
				// Two different versions of the same Pod will always have different RVs.
				return
			}
			glog.V(4).Infof("Pod %s changed status to %s", newPod.Name, newPod.Status.Phase)
			if (newPod.Status.Phase == corev1.PodSucceeded || newPod.Status.Phase == corev1.PodFailed) &&
				(oldPod.Status.Phase != corev1.PodSucceeded && oldPod.Status.Phase != corev1.PodFailed) {
				imagemanager.handlePodStatusChange(newPod)
			}
		},
		//DeleteFunc: ,
	})
	return imagemanager, podInformer
}

func (m *ImageManager) handlePodStatusChange(pod *corev1.Pod) {
	glog.V(4).Infof("Pod %s changed status to %s", pod.Name, pod.Status.Phase)
	m.lock.RLock()
	iwres, ok := m.imageworkstatus[pod.Labels["job-name"]]
	m.lock.RUnlock()
	// Corresponding job might have expired and got deleted.
	// ignore pod status change for such jobs
	if !ok {
		return
	}

	if pod.Status.Phase == corev1.PodSucceeded {
		iwres.Status = ImageWorkResultStatusSucceeded
		if iwres.ImageWorkRequest.WorkType == ImageCachePurge {
			glog.Infof("Job %s succeeded (delete:- %s --> %s, runtime: %s)", pod.Labels["job-name"], iwres.ImageWorkRequest.Image, iwres.ImageWorkRequest.Node.Labels["kubernetes.io/hostname"], iwres.ImageWorkRequest.ContainerRuntimeVersion)
		} else {
			glog.Infof("Job %s succeeded (pull:- %s --> %s, runtime: %s)", pod.Labels["job-name"], iwres.ImageWorkRequest.Image, iwres.ImageWorkRequest.Node.Labels["kubernetes.io/hostname"], iwres.ImageWorkRequest.ContainerRuntimeVersion)
		}
	}
	if pod.Status.Phase == corev1.PodFailed {
		iwres.Status = ImageWorkResultStatusFailed
		if pod.Status.ContainerStatuses[0].State.Terminated != nil {
			iwres.Reason = pod.Status.ContainerStatuses[0].State.Terminated.Reason
			iwres.Message = pod.Status.ContainerStatuses[0].State.Terminated.Message
		}
		if iwres.ImageWorkRequest.WorkType == ImageCachePurge {
			glog.Infof("Job %s failed (delete: %s --> %s)", pod.Labels["job-name"], iwres.ImageWorkRequest.Image, iwres.ImageWorkRequest.Node.Labels["kubernetes.io/hostname"])
		} else {
			glog.Infof("Job %s failed (pull: %s --> %s)", pod.Labels["job-name"], iwres.ImageWorkRequest.Image, iwres.ImageWorkRequest.Node.Labels["kubernetes.io/hostname"])
		}
	}
	m.lock.Lock()
	m.imageworkstatus[pod.Labels["job-name"]] = iwres
	m.lock.Unlock()
}

func (m *ImageManager) updatePendingImageWorkResults(imageCacheName string) error {
	m.lock.Lock()
	defer m.lock.Unlock()
	for job, iwres := range m.imageworkstatus {
		if iwres.ImageWorkRequest.Imagecache.Name == imageCacheName {
			if iwres.Status == ImageWorkResultStatusJobCreated {
				pods, err := m.podsLister.Pods(m.fledgedNameSpace).
					List(labels.Set(map[string]string{"job-name": job}).AsSelector())
				if err != nil {
					glog.Errorf("Error listing Pods: %v", err)
					return err
				}
				if len(pods) == 0 {
					glog.Errorf("No pods matched job %s", job)
					return fmt.Errorf("no pods matched job %s", job)
				}
				if len(pods) > 1 {
					glog.Errorf("More than one pod matched job %s", job)
					return fmt.Errorf("more than one pod matched job %s", job)
				}
				iwres.Status = ImageWorkResultStatusFailed
				if iwres.ImageWorkRequest.WorkType == ImageCachePurge {
					glog.Infof("Job %s expired (delete: %s --> %s)", job, iwres.ImageWorkRequest.Image, iwres.ImageWorkRequest.Node.Labels["kubernetes.io/hostname"])
				} else {
					glog.Infof("Job %s expired (pull: %s --> %s)", job, iwres.ImageWorkRequest.Image, iwres.ImageWorkRequest.Node.Labels["kubernetes.io/hostname"])
				}
				if pods[0].Status.Phase == corev1.PodPending {
					if len(pods[0].Status.ContainerStatuses) == 1 {
						if pods[0].Status.ContainerStatuses[0].State.Waiting != nil {
							iwres.Reason = pods[0].Status.ContainerStatuses[0].State.Waiting.Reason
							iwres.Message = pods[0].Status.ContainerStatuses[0].State.Waiting.Message
						}
						if pods[0].Status.ContainerStatuses[0].State.Terminated != nil {
							iwres.Reason = pods[0].Status.ContainerStatuses[0].State.Terminated.Reason
							iwres.Message = pods[0].Status.ContainerStatuses[0].State.Terminated.Message
						}
					} else {
						iwres.Reason = "Pending"
						iwres.Message = "Check if node is ready"
					}
				}
				if iwres.ImageWorkRequest.WorkType != ImageCachePurge {
					fieldSelector := fields.Set{
						"involvedObject.kind":      "Pod",
						"involvedObject.name":      pods[0].Name,
						"involvedObject.namespace": m.fledgedNameSpace,
						"reason":                   "Failed",
					}.AsSelector().String()

					eventlist, err := m.kubeclientset.CoreV1().Events(m.fledgedNameSpace).
						List(context.TODO(), metav1.ListOptions{FieldSelector: fieldSelector})
					if err != nil {
						glog.Errorf("Error listing events for pod (%s): %v", pods[0].Name, err)
						return err
					}

					for _, v := range eventlist.Items {
						iwres.Message = iwres.Message + ":" + v.Message
					}
				}
				m.imageworkstatus[job] = iwres
			}
		}
	}
	glog.V(4).Infof("imageworkstatus map: %+v", m.imageworkstatus)
	return nil
}

func (m *ImageManager) updateImageCacheStatus(imageCacheName string, errCh chan<- error) {
	wait.Poll(time.Second, m.imagePullDeadlineDuration,
		func() (done bool, err error) {
			m.lock.RLock()
			defer m.lock.RUnlock()
			done, err = true, nil
			for _, iwres := range m.imageworkstatus {
				if iwres.ImageWorkRequest.Imagecache.Name == imageCacheName {
					if iwres.Status == ImageWorkResultStatusJobCreated {
						done, err = false, nil
						return
					}
				}
			}
			return
		})
	glog.V(4).Info("wait.Poll exited successfully")
	err := m.updatePendingImageWorkResults(imageCacheName)
	if err != nil {
		glog.Errorf("Error from updatePendingImageWorkResults(): %v", err)
		errCh <- err
		return
	}
	glog.V(4).Info("m.updatePendingImageWorkResults exited successfully")
	//m.lock.Lock()
	iwstatus := map[string]ImageWorkResult{}
	//m.lock.Unlock()
	deletePropagation := metav1.DeletePropagationBackground
	var iwstatusLock sync.RWMutex
	var imageCache *fledgedv1alpha2.ImageCache
	m.lock.Lock()
	for job, iwres := range m.imageworkstatus {
		if iwres.ImageWorkRequest.Imagecache.Name == imageCacheName {
			iwstatusLock.Lock()
			iwstatus[job] = iwres
			iwstatusLock.Unlock()
			imageCache = iwres.ImageWorkRequest.Imagecache
			delete(m.imageworkstatus, job)
			// delete jobs
			if !strings.HasPrefix(job, fakeJobPrefix) {
				if err := m.kubeclientset.BatchV1().Jobs(m.fledgedNameSpace).
					Delete(context.TODO(), job, metav1.DeleteOptions{PropagationPolicy: &deletePropagation}); err != nil {
					glog.Errorf("Error deleting job %s: %v", job, err)
					m.lock.Unlock()
					errCh <- err
					return
				}
			}
		}
	}
	m.lock.Unlock()
	if imageCache == nil {
		glog.Errorf("Unable to obtain reference to image cache")
		errCh <- fmt.Errorf("unable to obtain reference to image cache")
		return
	}
	objKey, err := cache.MetaNamespaceKeyFunc(imageCache)
	if err != nil {
		glog.Errorf("Error from cache.MetaNamespaceKeyFunc(imageCache): %v", err)
		errCh <- err
		return
	}
	m.workqueue.AddRateLimited(WorkQueueKey{
		WorkType: ImageCacheStatusUpdate,
		Status:   &iwstatus,
		ObjKey:   objKey,
	})

	errCh <- nil
}

// Run starts the Image Manager go routine
func (m *ImageManager) Run(stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	glog.Info("Starting image manager")
	go m.kubeInformerFactory.Start(stopCh)
	// Wait for the caches to be synced before starting workers
	glog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, m.podsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}
	go wait.Until(m.runWorker, time.Second, stopCh)
	glog.Info("Started image manager")
	<-stopCh
	glog.Info("Shutting down image manager")
	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (m *ImageManager) runWorker() {
	for m.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (m *ImageManager) processNextWorkItem() bool {
	//glog.Info("processNextWorkItem::Beginning...")
	obj, shutdown := m.imageworkqueue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer m.imageworkqueue.Done(obj)
		var iwr ImageWorkRequest
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if iwr, ok = obj.(ImageWorkRequest); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			m.imageworkqueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("unexpected type in workqueue: %#v", obj))
			return nil
		}

		if iwr.Image == "" && iwr.Node == nil {
			m.imageworkqueue.Forget(obj)
			errCh := make(chan error)
			go m.updateImageCacheStatus(iwr.Imagecache.Name, errCh)
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// ImageCache resource to be synced.
		var job *batchv1.Job
		var err error
		var pull, delete bool
		if iwr.WorkType == ImageCachePurge {
			delete = true
			job, err = m.deleteImage(iwr)
			if err != nil {
				return fmt.Errorf("error deleting image '%s' from node '%s': %s", iwr.Image, iwr.Node.Labels["kubernetes.io/hostname"], err.Error())
			}
			glog.Infof("Job %s created (delete:- %s --> %s, runtime: %s)", job.Name, iwr.Image, iwr.Node.Labels["kubernetes.io/hostname"], iwr.ContainerRuntimeVersion)
		} else {
			pull = true
			pull, err = checkIfImageNeedsToBePulled(m.imagePullPolicy, iwr.Image, iwr.Node)
			if err != nil {
				glog.Errorf("Error from checkIfImageNeedsToBePulled(): %+v", err)
				return fmt.Errorf("error from checkIfImageNeedsToBePulled(): %+v", err)
			}
			if pull {
				job, err = m.pullImage(iwr)
				if err != nil {
					return fmt.Errorf("error pulling image '%s' to node '%s': %s", iwr.Image, iwr.Node.Labels["kubernetes.io/hostname"], err.Error())
				}
				glog.Infof("Job %s created (pull:- %s --> %s, runtime: %s)", job.Name, iwr.Image, iwr.Node.Labels["kubernetes.io/hostname"], iwr.ContainerRuntimeVersion)
			} else {
				glog.Infof("Job not created (image-already-present:- %s --> %s, runtime: %s)", iwr.Image, iwr.Node.Labels["kubernetes.io/hostname"], iwr.ContainerRuntimeVersion)
			}
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		m.lock.Lock()
		if pull || delete {
			m.imageworkstatus[job.Name] = ImageWorkResult{ImageWorkRequest: iwr, Status: ImageWorkResultStatusJobCreated}
		} else {
			// generate a random fake job name
			m.imageworkstatus[names.SimpleNameGenerator.GenerateName(fakeJobPrefix)] = ImageWorkResult{ImageWorkRequest: iwr, Status: ImageWorkResultStatusAlreadyPulled}
		}
		m.lock.Unlock()
		m.imageworkqueue.Forget(obj)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}

	return true
}

// pullImage pulls the image to the node
func (m *ImageManager) pullImage(iwr ImageWorkRequest) (*batchv1.Job, error) {
	// Construct the Job manifest
	newjob, err := newImagePullJob(iwr.Imagecache, iwr.Image, iwr.Node, m.imagePullPolicy)
	if err != nil {
		glog.Errorf("Error when constructing job manifest: %v", err)
		return nil, err
	}
	// Create a Job to pull the image into the node
	job, err := m.kubeclientset.BatchV1().Jobs(m.fledgedNameSpace).Create(context.TODO(), newjob, metav1.CreateOptions{})
	if err != nil {
		glog.Errorf("Error creating job in node %s: %v", iwr.Node, err)
		return nil, err
	}
	return job, nil
}

// deleteImage deletes the image from the node
func (m *ImageManager) deleteImage(iwr ImageWorkRequest) (*batchv1.Job, error) {
	// Construct the Job manifest
	newjob, err := newImageDeleteJob(iwr.Imagecache, iwr.Image, iwr.Node, iwr.ContainerRuntimeVersion, m.dockerClientImage)
	if err != nil {
		glog.Errorf("Error when constructing job manifest: %v", err)
		return nil, err
	}
	// Create a Job to delete the image from the node
	job, err := m.kubeclientset.BatchV1().Jobs(m.fledgedNameSpace).Create(context.TODO(), newjob, metav1.CreateOptions{})
	if err != nil {
		glog.Errorf("Error creating job in node %s: %v", iwr.Node, err)
		return nil, err
	}
	return job, nil
}
